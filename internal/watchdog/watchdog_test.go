package watchdog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TangYani2024/bswarm/internal/agentapi"
	"github.com/TangYani2024/bswarm/internal/gateway"
	"github.com/TangYani2024/bswarm/internal/provider"
	"github.com/TangYani2024/bswarm/internal/store"
)

type spyAPI struct {
	sent []struct{ to, title, content string }
}

func (s *spyAPI) NewConversation(context.Context, agentapi.NewConversationOpts) (string, error) {
	return "conv-new", nil
}
func (s *spyAPI) SendMessage(_ context.Context, to, title, content string) error {
	s.sent = append(s.sent, struct{ to, title, content string }{to, title, content})
	return nil
}
func (s *spyAPI) GetMetadata(context.Context, string) (*agentapi.Metadata, error) {
	return &agentapi.Metadata{}, nil
}

func TestWatchdogQuotaAndStallAlertsNoSpam(t *testing.T) {
	root := t.TempDir()
	appData := filepath.Join(root, "appdata")
	st := store.New(filepath.Join(root, "data"))

	now := time.Now().UTC()
	tr := &store.Tree{
		Version: 1, ID: "tw1", RepoRoot: root, AppDataDir: appData, MaxDepth: 3, MaxActive: 12,
		Nodes: map[string]*store.Node{
			"root":   {ID: "root", Role: "coordinator", ConversationID: "conv-root", Depth: 0, State: store.NodeWorking, CreatedAt: now, UpdatedAt: now},
			"lead-a": {ID: "lead-a", Role: "lead", ParentID: "root", ConversationID: "conv-lead", Depth: 1, State: store.NodeWorking, CurrentTaskID: "t1", CreatedAt: now, UpdatedAt: now},
			"w-429":  {ID: "w-429", Role: "coder", ParentID: "lead-a", ConversationID: "conv-429", Depth: 2, State: store.NodeWorking, CurrentTaskID: "t2", CreatedAt: now, UpdatedAt: now},
			"w-slow": {ID: "w-slow", Role: "coder", ParentID: "lead-a", ConversationID: "conv-slow", Depth: 2, State: store.NodeWorking, CurrentTaskID: "t3", CreatedAt: now.Add(-20 * time.Minute), UpdatedAt: now.Add(-20 * time.Minute)},
		},
		Tasks: map[string]*store.Task{},
	}
	if err := st.CreateTree(tr); err != nil {
		t.Fatal(err)
	}

	// Write a 429 quota error transcript for w-429.
	tr429 := filepath.Join(appData, "brain", "conv-429", ".system_generated", "logs", "transcript.jsonl")
	_ = os.MkdirAll(filepath.Dir(tr429), 0o755)
	_ = os.WriteFile(tr429, []byte(`{"type":"ERROR_MESSAGE","status":"ERROR","error":"RESOURCE_EXHAUSTED: 429 quota exceeded"}`+"\n"), 0o644)

	// Write an old stalled transcript for w-slow.
	trSlow := filepath.Join(appData, "brain", "conv-slow", ".system_generated", "logs", "transcript.jsonl")
	_ = os.MkdirAll(filepath.Dir(trSlow), 0o755)
	_ = os.WriteFile(trSlow, []byte(`{"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"read_file"}]}`+"\n"), 0o644)
	oldTime := now.Add(-15 * time.Minute)
	_ = os.Chtimes(trSlow, oldTime, oldTime)

	spy := &spyAPI{}
	wd := New(st, spy, Config{
		TreeID:       "tw1",
		AppDataDir:   appData,
		StallTimeout: 10 * time.Minute,
	})
	wd.Now = func() time.Time { return now }

	// First tick: should alert direct parent (conv-lead) twice (quota + stall), never root!
	rep1, err := wd.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if rep1.AlertsSent != 2 || len(spy.sent) != 2 {
		t.Fatalf("expected 2 alerts, got rep=%+v sent=%+v", rep1, spy.sent)
	}
	for _, s := range spy.sent {
		if s.to != "conv-lead" {
			t.Fatalf("alert went to wrong recipient %q (expected direct parent conv-lead)", s.to)
		}
	}
	if !strings.Contains(spy.sent[0].content+spy.sent[1].content, "quota") {
		t.Fatalf("expected quota alert in %+v", spy.sent)
	}

	// Second tick with no state change: must NOT spam duplicate alerts.
	rep2, err := wd.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if rep2.AlertsSent != 0 {
		t.Fatalf("expected 0 duplicate alerts, got %d", rep2.AlertsSent)
	}

	// Outbox Relay test: simulate a sandboxed child appending mail whose direct wake failed
	// (InboxTotalSeq > InboxDeliveredSeq). Watchdog Tick must relay it once and advance InboxDeliveredSeq.
	_, err = st.UpdateTree("tw1", func(tree *store.Tree) error {
		_, merr := st.AppendMailUnlocked(tree, "root", store.Mail{
			From:    "lead-a",
			Kind:    "done",
			TaskID:  "t1",
			Summary: "all sub-tasks verified",
		})
		return merr
	})
	if err != nil {
		t.Fatal(err)
	}
	rep3, err := wd.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick 3 (relay): %v", err)
	}
	if rep3.RelayedMails != 1 {
		t.Fatalf("expected 1 relayed mail, got %d", rep3.RelayedMails)
	}
	// Tick 4 must NOT re-relay already delivered mail.
	rep4, err := wd.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick 4: %v", err)
	}
	if rep4.RelayedMails != 0 {
		t.Fatalf("expected 0 duplicate relays, got %d", rep4.RelayedMails)
	}
}

func TestWatchdogHotReloadsProvidersAndPromotesPool(t *testing.T) {
	dir := t.TempDir()
	st := store.New(filepath.Join(dir, "store"))
	cfgPath := filepath.Join(dir, "providers.json")
	t.Setenv("BSWARM_PROVIDERS_FILE", cfgPath)

	// Start with only 1 provider in pool "dyn", currently occupied by w1 -> w2 is queued.
	v1 := `{
		"providers": {"p1": {"url":"https://p1.example/v1","models":["m1"],"max_concurrency":1}},
		"pools": {"dyn": ["p1:m1"]}
	}`
	if err := os.WriteFile(cfgPath, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	tr := &store.Tree{
		Version: 1, ID: "tw-hot", RepoRoot: dir, MaxDepth: 3, MaxActive: 12,
		Nodes: map[string]*store.Node{
			"root": {ID: "root", Role: "coordinator", ConversationID: "conv-root", Depth: 0, State: store.NodeWorking, UpdatedAt: now},
			"w1":   {ID: "w1", Role: "worker", ParentID: "root", Provider: "p1", ProviderModel: "m1", Model: "p1:m1", Pool: "dyn", ConcurrencyKey: "p1", Depth: 1, State: store.NodeWorking, CurrentTaskID: "t1", UpdatedAt: now},
			"w2":   {ID: "w2", Role: "worker", ParentID: "root", Model: "pool:dyn", Pool: "dyn", Depth: 1, State: store.NodeQueued, CurrentTaskID: "t2", UpdatedAt: now},
		},
		Tasks: map[string]*store.Task{
			"t1": {ID: "t1", AssigneeID: "w1", CreatedBy: "root", Goal: "g1", State: store.TaskRunning, CreatedAt: now, UpdatedAt: now},
			"t2": {ID: "t2", AssigneeID: "w2", CreatedBy: "root", Goal: "g2", State: store.TaskQueued, CreatedAt: now, UpdatedAt: now},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateTree(tr); err != nil {
		t.Fatal(err)
	}

	reloader := provider.NewReloader(dir, st.RootDir)
	wd := New(st, &spyAPI{}, Config{TreeID: "tw-hot"})
	wd.ProviderReloader = reloader
	var launched []string
	wd.Orch.GatewayLaunch = func(req gateway.LaunchRequest) error {
		launched = append(launched, req.NodeID)
		return nil
	}

	// Tick 1: p1 is busy -> w2 stays queued.
	rep1, err := wd.Tick(context.Background())
	if err != nil || rep1.Promoted != 0 || len(launched) != 0 {
		t.Fatalf("Tick 1 expected 0 promoted while p1 busy: rep=%+v launched=%v err=%v", rep1, launched, err)
	}

	// User hot-adds a brand new provider p2 to pool "dyn" WITHOUT restarting watchdog!
	time.Sleep(15 * time.Millisecond)
	v2 := `{
		"providers": {
			"p1": {"url":"https://p1.example/v1","models":["m1"],"max_concurrency":1},
			"p2": {"url":"https://p2.example/v1","models":["m2"],"max_concurrency":1}
		},
		"pools": {"dyn": ["p1:m1", "p2:m2"]}
	}`
	if err := os.WriteFile(cfgPath, []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}

	// Tick 2: watchdog hot-reloads providers.json and immediately launches queued w2 on p2:m2!
	rep2, err := wd.Tick(context.Background())
	if err != nil || rep2.Promoted != 1 || len(launched) != 1 || launched[0] != "w2" {
		t.Fatalf("Tick 2 expected hot-reloaded promotion of w2 on p2:m2: rep=%+v launched=%v err=%v", rep2, launched, err)
	}
	updated, _ := st.LoadTree("tw-hot")
	if updated.Nodes["w2"].Model != "p2:m2" || updated.Nodes["w2"].State != store.NodeWorking {
		t.Fatalf("expected w2 running on hot-added p2:m2, got %+v", updated.Nodes["w2"])
	}
}
