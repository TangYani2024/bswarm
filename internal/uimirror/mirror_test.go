package uimirror

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TangYani2024/bswarm/internal/store"
)

var (
	reChannelName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	reViewID      = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
)

func TestChannelNameSanitizationAndReserved(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"main", "s-main"},
		{"all", "s-all"},
		{"here", "s-here"},
		{"", "s-node"},
		{"---", "s-node"},
		{"123-branch", "s-123-branch"},
		{"Lead_Audit/Core", "lead-audit-core"},
		{"very-long-branch-name-exceeding-twenty-four", "very-long-branch-name-ex"},
		{"aaaaaaaaaaaaaaaaaaaaaaa-tail", "aaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for _, tc := range cases {
		got := ChannelName(tc.in)
		if got != tc.want {
			t.Errorf("ChannelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !reChannelName.MatchString(got) {
			t.Errorf("ChannelName(%q) = %q does not match %s", tc.in, got, reChannelName.String())
		}
		if got == "main" || got == "all" || got == "here" {
			t.Errorf("ChannelName(%q) collided with reserved name %q", tc.in, got)
		}
	}
}

func TestMultiLevelTreeViewSchemaChannelsNarratorAndAntiFlicker(t *testing.T) {
	rootDir := t.TempDir()
	appData := filepath.Join(rootDir, "appdata")
	st := store.New(filepath.Join(rootDir, "data"))

	// Create a valid artifact file inside <appData>/brain/<conv>/artifacts/report.md
	artPath := filepath.Join(appData, "brain", "conv-w1-3333", "artifacts", "report.md")
	if err := os.MkdirAll(filepath.Dir(artPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artPath, []byte("# Report\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	tr := &store.Tree{
		Version:    1,
		ID:         "tree-v050",
		Goal:       "Implement bswarm v0.5.0 UI mirror",
		RepoRoot:   rootDir,
		AppDataDir: appData,
		MaxDepth:   3,
		MaxActive:  12,
		Nodes: map[string]*store.Node{
			"root": {
				ID: "root", Role: "coordinator", ConversationID: "conv-root-0000",
				Depth: 0, Model: "gemini-4-pro", State: store.NodeWorking,
				CreatedAt: now, UpdatedAt: now,
			},
			// Multi-level branch: L1 -> L2 -> L3 (mixed official + external providers)
			"lead-ui": {
				ID: "lead-ui", Role: "lead", ParentID: "root", ConversationID: "conv-lead-1111",
				Depth: 1, Model: "gemini-4-high", State: store.NodeIdle,
				Owns: []string{"internal/uimirror/**"}, CurrentTaskID: "t1",
				CreatedAt: now, UpdatedAt: now,
			},
			"lead-ui/w1": {
				ID: "lead-ui/w1", Role: "coder", ParentID: "lead-ui", ConversationID: "conv-w1-3333",
				Depth: 2, Model: "gemini-4-high", State: store.NodeWorking,
				Owns: []string{"internal/uimirror/view.go"}, CurrentTaskID: "t2",
				CreatedAt: now, UpdatedAt: now,
			},
			"lead-ui/w2_api": {
				ID: "lead-ui/w2_api", Role: "coder", ParentID: "lead-ui",
				Depth: 2, Provider: "deepseek", ProviderModel: "deepseek-chat", Model: "deepseek",
				State: store.NodeError, FailoverCount: 1, FailedTargets: []string{"deepseek"},
				LastError: "rate limit 429", CurrentTaskID: "t3",
				CreatedAt: now, UpdatedAt: now,
			},
			"lead-ui/w1/leaf": {
				ID: "lead-ui/w1/leaf", Role: "tester", ParentID: "lead-ui/w1",
				Depth: 3, Provider: "qwen", ProviderModel: "qwen-plus", Model: "qwen",
				State: store.NodeIdle, CurrentTaskID: "t4",
				CreatedAt: now, UpdatedAt: now,
			},
			// Standalone L1 worker (no children -> defaults to #main, no subtree channel)
			"solo-doc": {
				ID: "solo-doc", Role: "docs", ParentID: "root", ConversationID: "conv-solo-4444",
				Depth: 1, Model: "gemini-4-flash", State: store.NodeBlocked,
				LastError: "waiting on schema decision", CurrentTaskID: "t5",
				CreatedAt: now, UpdatedAt: now,
			},
		},
		Tasks: map[string]*store.Task{
			"t1": {ID: "t1", AssigneeID: "lead-ui", CreatedBy: "root", Goal: "Lead UI mirror", State: store.TaskRunning, CreatedAt: now, UpdatedAt: now},
			"t2": {ID: "t2", AssigneeID: "lead-ui/w1", CreatedBy: "lead-ui", Goal: "Build view.json", Artifact: artPath, State: store.TaskRunning, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)},
			"t3": {ID: "t3", AssigneeID: "lead-ui/w2_api", CreatedBy: "lead-ui", Goal: "Build view.html", State: store.TaskFailed, Summary: "429 upstream", CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)},
			"t4": {ID: "t4", AssigneeID: "lead-ui/w1/leaf", CreatedBy: "lead-ui/w1", Goal: "Run tests", State: store.TaskDone, Summary: "All unit tests green", CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second)},
			"t5": {ID: "t5", AssigneeID: "solo-doc", CreatedBy: "root", Goal: "Write docs", DependsOn: []string{"t2"}, State: store.TaskQueued, CreatedAt: now.Add(4 * time.Second), UpdatedAt: now.Add(4 * time.Second)},
		},
	}

	if err := RebuildBoard(st, tr); err != nil {
		t.Fatalf("RebuildBoard: %v", err)
	}
	sdir := SwarmDir(tr)

	// 1. Verify members.json: coordinator, virtual narrator, subtree channel membership, standalone nil channels.
	mRaw, err := os.ReadFile(filepath.Join(sdir, "members.json"))
	if err != nil {
		t.Fatalf("read members.json: %v", err)
	}
	var roster officialRoster
	if err := json.Unmarshal(mRaw, &roster); err != nil {
		t.Fatalf("unmarshal members.json: %v", err)
	}
	narr, ok := roster.Members["bswarm-narrator"]
	if !ok {
		t.Fatalf("missing virtual bswarm-narrator in members.json")
	}
	if narr.Role != "narrator" || narr.Model != "bswarm-mirror" || narr.Killed || len(narr.Channels) != 1 || narr.Channels[0] != "*" {
		t.Fatalf("unexpected bswarm-narrator entry: %+v", narr)
	}
	if coord := roster.Members["coordinator"]; len(coord.Channels) != 1 || coord.Channels[0] != "*" {
		t.Fatalf("coordinator should have channels=[\"*\"], got %+v", coord.Channels)
	}
	expectedCh := ChannelName("lead-ui")
	for _, m := range []string{"lead-ui", "lead-ui-w1", "lead-ui-w2_api", "lead-ui-w1-leaf"} {
		mem, exists := roster.Members[m]
		if !exists {
			t.Fatalf("missing member %q", m)
		}
		if len(mem.Channels) != 1 || mem.Channels[0] != expectedCh {
			t.Fatalf("member %q channels = %v, want [%q]", m, mem.Channels, expectedCh)
		}
	}
	if solo := roster.Members["solo-doc"]; len(solo.Channels) != 0 {
		t.Fatalf("standalone L1 worker solo-doc should default to #main (nil channels), got %v", solo.Channels)
	}

	// 2. Verify board.jsonl: control create emitted once before subtree channel msgs, root events on #main, monotonic seq.
	bRaw, err := os.ReadFile(filepath.Join(sdir, "board.jsonl"))
	if err != nil {
		t.Fatalf("read board.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(bRaw)), "\n")
	createCount := 0
	prevSeq := 0
	sawSubtreeMsg := false
	sawMainMsg := false
	for _, ln := range lines {
		var msg officialBoardMsg
		if err := json.Unmarshal([]byte(ln), &msg); err != nil {
			t.Fatalf("invalid board.jsonl line %q: %v", ln, err)
		}
		if msg.Seq <= prevSeq {
			t.Fatalf("non-monotonic seq %d after %d", msg.Seq, prevSeq)
		}
		prevSeq = msg.Seq
		if msg.Control != nil && msg.Control.Op == "create" {
			createCount++
			if msg.Control.Channel != expectedCh || msg.Channel != expectedCh {
				t.Fatalf("unexpected create control channel: %+v", msg)
			}
			if sawSubtreeMsg {
				t.Fatalf("create control appeared AFTER subtree message")
			}
		} else if msg.Channel == expectedCh {
			sawSubtreeMsg = true
			if createCount == 0 {
				t.Fatalf("subtree message appeared before create control")
			}
		} else if msg.Channel == "" {
			sawMainMsg = true
		}
	}
	if createCount != 1 || !sawSubtreeMsg || !sawMainMsg {
		t.Fatalf("board checks failed: createCount=%d sawSubtree=%v sawMain=%v", createCount, sawSubtreeMsg, sawMainMsg)
	}

	// AppendEvent onto existing subtree channel must NOT duplicate create and must keep seq monotonic.
	tr.MirrorSeq++
	if err := AppendEvent(tr, tr.MirrorSeq, "lead-ui/w1/leaf", "lead-ui/w1", "done", "t4", "Completed leaf", ""); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	bRaw2, _ := os.ReadFile(filepath.Join(sdir, "board.jsonl"))
	lines2 := strings.Split(strings.TrimSpace(string(bRaw2)), "\n")
	if len(lines2) != len(lines)+1 {
		t.Fatalf("expected 1 new line (no duplicate create), got %d -> %d", len(lines), len(lines2))
	}
	var lastMsg officialBoardMsg
	_ = json.Unmarshal([]byte(lines2[len(lines2)-1]), &lastMsg)
	if lastMsg.Channel != expectedCh || lastMsg.Seq <= prevSeq {
		t.Fatalf("appended subtree msg invalid: %+v (prevSeq=%d)", lastMsg, prevSeq)
	}

	// 3. Verify plan.json has [L<depth>] and provider tag.
	pRaw, err := os.ReadFile(filepath.Join(sdir, "plan.json"))
	if err != nil {
		t.Fatalf("read plan.json: %v", err)
	}
	var plan officialPlan
	if err := json.Unmarshal(pRaw, &plan); err != nil {
		t.Fatalf("unmarshal plan.json: %v", err)
	}
	if !strings.Contains(plan.Tasks["lead-ui-w2_api"], "[L2][deepseek]") {
		t.Fatalf("expected [L2][deepseek] in plan.Tasks, got %q", plan.Tasks["lead-ui-w2_api"])
	}

	// 4. Verify view.json schema, IDs, DFS order, refs, threads->workstreams referential integrity, size < 60KB.
	vRaw, err := os.ReadFile(filepath.Join(sdir, "view.json"))
	if err != nil {
		t.Fatalf("read view.json: %v", err)
	}
	if len(vRaw) >= 60*1024 {
		t.Fatalf("view.json size %d >= 60KB", len(vRaw))
	}
	var view viewJSON
	if err := json.Unmarshal(vRaw, &view); err != nil {
		t.Fatalf("unmarshal view.json: %v", err)
	}
	if view.Version != 1 || view.Goal.Title == "" || view.Headline == "" {
		t.Fatalf("missing top-level view fields: %+v", view)
	}
	wsIDs := map[string]bool{}
	var gotOrder []string
	for _, ws := range view.Workstreams {
		if !reViewID.MatchString(ws.ID) {
			t.Fatalf("invalid workstream ID %q", ws.ID)
		}
		if len(ws.Refs) == 0 || ws.Refs[0].Member == "" {
			t.Fatalf("workstream %q has empty refs", ws.ID)
		}
		wsIDs[ws.ID] = true
		gotOrder = append(gotOrder, ws.Owners[0])
	}
	// Check hierarchical DFS order: lead-ui must appear before its children, and lead-ui/w1 immediately before lead-ui/w1/leaf.
	idxOf := func(name string) int {
		for i, v := range gotOrder {
			if v == name {
				return i
			}
		}
		return -1
	}
	if !(idxOf("lead-ui") < idxOf("lead-ui-w1") && idxOf("lead-ui-w1")+1 == idxOf("lead-ui-w1-leaf")) {
		t.Fatalf("workstreams not in hierarchical DFS order: %v", gotOrder)
	}
	// Verify blocked_on for lead-ui (idle parent waiting on working child) and solo-doc (depends on t2 -> lead-ui-w1).
	for _, ws := range view.Workstreams {
		if ws.Owners[0] == "lead-ui" && ws.BlockedOn != "lead-ui-w1" {
			t.Fatalf("lead-ui blocked_on = %q, want lead-ui-w1", ws.BlockedOn)
		}
		if ws.Owners[0] == "solo-doc" && ws.BlockedOn != "lead-ui-w1" {
			t.Fatalf("solo-doc blocked_on = %q, want lead-ui-w1", ws.BlockedOn)
		}
		if ws.Owners[0] == "lead-ui-w1" {
			hasFile := false
			hasConv := false
			for _, a := range ws.Actions {
				if a.Type == "file" && a.Target == artPath {
					hasFile = true
				}
				if a.Type == "open_conv" && a.Target == "lead-ui-w1" {
					hasConv = true
				}
			}
			if !hasFile || !hasConv {
				t.Fatalf("lead-ui-w1 missing expected file/open_conv actions: %+v", ws.Actions)
			}
		}
	}
	if len(view.Threads) != 2 {
		t.Fatalf("expected 2 threads, got %d", len(view.Threads))
	}
	for _, th := range view.Threads {
		if !reViewID.MatchString(th.ID) {
			t.Fatalf("invalid thread ID %q", th.ID)
		}
		for _, refWS := range th.Workstreams {
			if !wsIDs[refWS] {
				t.Fatalf("thread %q references unknown workstream %q", th.ID, refWS)
			}
		}
	}
	if len(view.Attention) < 2 {
		t.Fatalf("expected at least 2 attention items (error + blocked), got %+v", view.Attention)
	}
	for _, att := range view.Attention {
		if !reViewID.MatchString(att.ID) || len(att.Refs) == 0 {
			t.Fatalf("invalid attention item: %+v", att)
		}
	}
	if len(view.Milestones) != 1 || !reViewID.MatchString(view.Milestones[0].ID) || len(view.Milestones[0].Refs) == 0 {
		t.Fatalf("invalid milestones: %+v", view.Milestones)
	}
	if len(view.Risks) != 1 || !reViewID.MatchString(view.Risks[0].ID) || len(view.Risks[0].Refs) == 0 {
		t.Fatalf("invalid risks: %+v", view.Risks)
	}

	// 5. Verify embedded view.html constraints and anti-flicker mtime preservation.
	htmlPath := filepath.Join(sdir, "view.html")
	htmlInfo1, err := os.Stat(htmlPath)
	if err != nil {
		t.Fatalf("stat view.html: %v", err)
	}
	if htmlInfo1.Size() > 12*1024 {
		t.Fatalf("view.html size %d exceeds 12KB", htmlInfo1.Size())
	}
	htmlBytes, _ := os.ReadFile(htmlPath)
	htmlStr := string(htmlBytes)
	if strings.Contains(htmlStr, "http://") || strings.Contains(htmlStr, "https://") {
		t.Fatalf("view.html must not contain external URLs")
	}
	if !strings.Contains(htmlStr, "window.narrator") {
		t.Fatalf("view.html missing window.narrator integration")
	}

	// Set an older mtime and ensure subsequent SyncRosterAndPlan does NOT rewrite identical view.html.
	oldTime := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	if err := os.Chtimes(htmlPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := SyncRosterAndPlan(st, tr); err != nil {
		t.Fatalf("second SyncRosterAndPlan: %v", err)
	}
	htmlInfo2, err := os.Stat(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !htmlInfo2.ModTime().Equal(oldTime) {
		t.Fatalf("view.html was rewritten unnecessarily! mtime changed from %v to %v", oldTime, htmlInfo2.ModTime())
	}
}

func TestRefusesToOverwriteNativeSwarmWithoutMarker(t *testing.T) {
	rootDir := t.TempDir()
	appData := filepath.Join(rootDir, "appdata")
	st := store.New(filepath.Join(rootDir, "data"))

	nativeConv := "conv-native-99999999"
	nativeDir := filepath.Join(appData, "brain", nativeConv, ".system_generated", "swarm")
	if err := os.MkdirAll(nativeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "members.json"), []byte(`{"title":"REAL NATIVE"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	trNative := &store.Tree{
		ID:         "t-nat",
		Goal:       "Should not overwrite",
		AppDataDir: appData,
		Nodes:      map[string]*store.Node{"root": {ID: "root", ConversationID: nativeConv}},
	}
	if err := SyncRosterAndPlan(st, trNative); err != nil {
		t.Fatalf("SyncRosterAndPlan native: %v", err)
	}
	if err := AppendEvent(trNative, 1, "root", "w1", "assign", "t1", "hi", ""); err != nil {
		t.Fatalf("AppendEvent native: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(nativeDir, "members.json"))
	if string(after) != `{"title":"REAL NATIVE"}` {
		t.Fatalf("mirror overwrote a native swarm members.json!")
	}
	if _, err := os.Stat(filepath.Join(nativeDir, "view.json")); !os.IsNotExist(err) {
		t.Fatalf("mirror wrote view.json into native swarm dir!")
	}
	if _, err := os.Stat(filepath.Join(nativeDir, "view.html")); !os.IsNotExist(err) {
		t.Fatalf("mirror wrote view.html into native swarm dir!")
	}
}
