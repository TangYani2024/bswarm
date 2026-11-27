package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TangYani2024/bswarm/internal/gateway"
	"github.com/TangYani2024/bswarm/internal/provider"
	"github.com/TangYani2024/bswarm/internal/store"
	"github.com/TangYani2024/bswarm/internal/uimirror"
)

func TestExternalGatewayRoutingConcurrencyAndPool(t *testing.T) {
	ctx := context.Background()
	st := store.New(t.TempDir())
	api := &mockAPI{}
	orch := New(st, api, nil)
	orch.Providers = &provider.Config{
		Providers: map[string]*provider.ProviderSpec{
			"unlimited": {
				URL:            "https://api.deepseek.com/v1",
				WireProtocol:   "openai",
				DefaultModel:   "deepseek-chat",
				MaxConcurrency: 0,
			},
			"hub": {
				URL:              "https://api.example.com/v1",
				WireProtocol:     "openai",
				Models:           []string{"vendor-a/ds", "vendor-b/ds"},
				MaxConcurrency:   1,
				ConcurrencyScope: "prefix:/",
			},
		},
		Pools: map[string][]string{
			"free": {
				"hub:vendor-a/ds",
				"hub:vendor-b/ds",
			},
		},
	}

	var launched []gateway.LaunchRequest
	orch.GatewayLaunch = func(req gateway.LaunchRequest) error {
		launched = append(launched, req)
		return nil
	}

	repoDir := t.TempDir()
	tree, _, err := orch.Init(InitOpts{ConversationID: "conv-root-gemini", Goal: "hybrid test", RepoRoot: repoDir})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 1. Official model still uses official agentapi Transport (untouched).
	rOfficial, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: "conv-root-gemini",
		ID:           "gemini-lead",
		Role:         "lead",
		Model:        "gemini-4-hydrogen-high",
		Goal:         "coordinate",
	})
	if err != nil {
		t.Fatalf("spawn official: %v", err)
	}
	if rOfficial.Provider != "" || rOfficial.State != store.NodeWorking || rOfficial.ConversationID == "" {
		t.Fatalf("unexpected official spawn: %+v", rOfficial)
	}
	if len(launched) != 0 {
		t.Fatalf("official spawn should not use gateway launcher")
	}

	// 2. Unlimited third-party provider launches two parallel workers immediately.
	u1, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: "conv-root-gemini",
		ID:           "ds-1",
		Provider:     "unlimited",
		Goal:         "task u1",
	})
	if err != nil || u1.State != store.NodeWorking || u1.Provider != "unlimited" {
		t.Fatalf("spawn u1: %+v err=%v", u1, err)
	}
	u2, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: "conv-root-gemini",
		ID:           "ds-2",
		Model:        "unlimited:deepseek-reasoner",
		Goal:         "task u2",
	})
	if err != nil || u2.State != store.NodeWorking || u2.Model != "unlimited:deepseek-reasoner" {
		t.Fatalf("spawn u2: %+v err=%v", u2, err)
	}
	if len(launched) != 2 {
		t.Fatalf("expected 2 unlimited gateway launches, got %d", len(launched))
	}

	// 3. Pool with per-platform concurrency=1:
	//    - p1 picks vendor-a/ds (running)
	//    - p2 skips busy vendor-a/ and picks vendor-b/ds (running in parallel!)
	//    - p3 sees both platforms busy -> automatically queued!
	p1, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-root-gemini", ID: "p1", Model: "pool:free", Goal: "pool task 1"})
	if err != nil || p1.State != store.NodeWorking || p1.Model != "hub:vendor-a/ds" {
		t.Fatalf("spawn p1: %+v err=%v", p1, err)
	}
	p2, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-root-gemini", ID: "p2", Model: "pool:free", Goal: "pool task 2"})
	if err != nil || p2.State != store.NodeWorking || p2.Model != "hub:vendor-b/ds" {
		t.Fatalf("spawn p2: %+v err=%v", p2, err)
	}
	p3, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-root-gemini", ID: "p3", Model: "pool:free", Goal: "pool task 3"})
	if err != nil || p3.State != store.NodeQueued || p3.Pool != "free" {
		t.Fatalf("expected p3 to queue while all pool slots busy, got %+v err=%v", p3, err)
	}
	if len(launched) != 4 {
		t.Fatalf("expected 4 total launches before p1 finishes, got %d", len(launched))
	}

	// 4. Complete p1 via CompleteGatewayTurn (simulating worker returning response without calling bswarm done).
	//    It should:
	//    - save response artifact & mark p1 idle
	//    - notify root (Gemini parent!) via official Transport
	//    - automatically promote queued p3 onto the freed vendor-a/ds slot!
	err = orch.CompleteGatewayTurn(ctx, tree.ID, "p1", p1.TaskID, gateway.TurnOutcome{
		ConversationID: "gw-conv-p1",
		Status:         "SUCCESS",
		Response:       "found 3 matches in main.go\ndetailed lines...",
	})
	if err != nil {
		t.Fatalf("CompleteGatewayTurn p1: %v", err)
	}
	if len(launched) != 5 || launched[4].NodeID != "p3" {
		t.Fatalf("expected p3 to be auto-promoted after p1 freed vendor-a slot; launches=%+v", launched)
	}

	tr, err := st.LoadTree(tree.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Nodes["p1"].State != store.NodeIdle || tr.Nodes["p1"].ConversationID != "gw-conv-p1" {
		t.Fatalf("p1 should be idle with bound convID, got %+v", tr.Nodes["p1"])
	}
	if tr.Nodes["p3"].State != store.NodeWorking || tr.Nodes["p3"].ProviderModel != "vendor-a/ds" {
		t.Fatalf("p3 should now run on freed vendor-a/ds slot, got %+v", tr.Nodes["p3"])
	}

	// Verify root received p1's completion mail + artifact.
	inbox, err := orch.Inbox("conv-root-gemini", tree.ID, false)
	if err != nil || len(inbox.Messages) != 1 {
		t.Fatalf("expected 1 completion mail in root inbox, got %+v err=%v", inbox, err)
	}
	if !strings.Contains(inbox.Messages[0].Summary, "found 3 matches") || inbox.Messages[0].Artifact == "" {
		t.Fatalf("unexpected mail content: %+v", inbox.Messages[0])
	}
	if _, err := os.Stat(inbox.Messages[0].Artifact); err != nil {
		t.Fatalf("expected saved gateway artifact at %s: %v", inbox.Messages[0].Artifact, err)
	}

	// 5. Assign to idle p1 while p3 still holds vendor-a/ slot -> must reject cleanly (slot busy).
	if _, err := orch.Assign(ctx, AssignOpts{CallerConvID: "conv-root-gemini", NodeID: "p1", Goal: "follow-up"}); err == nil {
		t.Fatalf("expected Assign on p1 to fail while p3 occupies hub:vendor-a slot")
	}

	// 6. Finish p3 -> frees vendor-a/ slot -> Assign on p1 succeeds and resumes via GatewayLaunch!
	if err := orch.CompleteGatewayTurn(ctx, tree.ID, "p3", p3.TaskID, gateway.TurnOutcome{
		ConversationID: "gw-conv-p3",
		Status:         "SUCCESS",
		Response:       "p3 done",
	}); err != nil {
		t.Fatalf("CompleteGatewayTurn p3: %v", err)
	}
	ar, err := orch.Assign(ctx, AssignOpts{CallerConvID: "conv-root-gemini", NodeID: "p1", Goal: "warm resume follow-up"})
	if err != nil {
		t.Fatalf("Assign p1 after slot freed: %v", err)
	}
	if ar.ConversationID != "gw-conv-p1" || len(launched) != 6 || launched[5].NodeID != "p1" {
		t.Fatalf("expected warm gateway resume for p1, ar=%+v launches=%d", ar, len(launched))
	}

	// Verify status rendering includes external model info cleanly.
	statusOut, err := orch.RenderStatus("conv-root-gemini", tree.ID, false)
	if err != nil {
		t.Fatalf("RenderStatus: %v", err)
	}
	if !strings.Contains(statusOut, "model=hub:vendor-a/ds") {
		t.Fatalf("expected external model tag in status:\n%s", statusOut)
	}
	_ = filepath.Base(repoDir)
}

func TestGatewayWorkerDoneDefersPromotionToHostSupervisor(t *testing.T) {
	ctx := context.Background()
	st := store.New(t.TempDir())
	orch := New(st, &mockAPI{}, nil)
	orch.Providers = &provider.Config{
		Providers: map[string]*provider.ProviderSpec{
			"hub": {
				URL:            "https://api.example.com/v1",
				WireProtocol:   "openai",
				Models:         []string{"p1/m1"},
				MaxConcurrency: 1,
			},
		},
	}
	var launches []string
	orch.GatewayLaunch = func(req gateway.LaunchRequest) error {
		launches = append(launches, req.NodeID)
		return nil
	}

	tree, _, err := orch.Init(InitOpts{ConversationID: "conv-root", Goal: "defer promo", RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r1, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-root", ID: "w1", Model: "hub:p1/m1", Goal: "first"})
	if err != nil || r1.State != store.NodeWorking {
		t.Fatalf("r1: %+v err=%v", r1, err)
	}
	r2, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-root", ID: "w2", Model: "hub:p1/m1", Goal: "second"})
	if err != nil || r2.State != store.NodeQueued {
		t.Fatalf("r2 should queue: %+v err=%v", r2, err)
	}

	// Simulate w1 running `bswarm done` INSIDE its sandboxed gateway worker subprocess.
	t.Setenv("BSWARM_IN_GATEWAY_WORKER", "1")
	t.Setenv("BSWARM_TREE_ID", tree.ID)
	t.Setenv("BSWARM_NODE_ID", "w1")
	if _, err := orch.Done(ctx, DoneOpts{Summary: "w1 finished inside sandbox"}); err != nil {
		t.Fatalf("Done inside sandbox: %v", err)
	}
	if len(launches) != 1 {
		t.Fatalf("sandboxed Done must NOT launch w2 inside sandbox, launches=%v", launches)
	}

	// Now host-side `_gw-run` supervisor completes the turn outside the sandbox -> promotes w2!
	t.Setenv("BSWARM_IN_GATEWAY_WORKER", "")
	if err := orch.CompleteGatewayTurn(ctx, tree.ID, "w1", r1.TaskID, gateway.TurnOutcome{
		ConversationID: "gw-w1",
		Status:         "SUCCESS",
		Response:       "done",
	}); err != nil {
		t.Fatalf("CompleteGatewayTurn: %v", err)
	}
	if len(launches) != 2 || launches[1] != "w2" {
		t.Fatalf("expected host CompleteGatewayTurn to promote w2, launches=%v", launches)
	}
}

func TestPoolAutomaticFailoverOnUpstreamError(t *testing.T) {
	ctx := context.Background()
	st := store.New(t.TempDir())
	api := &mockAPI{}
	orch := New(st, api, nil)
	orch.Providers = &provider.Config{
		Providers: map[string]*provider.ProviderSpec{
			"hub": {
				URL:              "https://api.example.com/v1",
				WireProtocol:     "openai",
				Models:           []string{"p1/m1", "p2/m2"},
				MaxConcurrency:   1,
				ConcurrencyScope: "prefix:/",
			},
		},
		Pools: map[string][]string{
			"fast": {"hub:p1/m1", "hub:p2/m2"},
		},
	}
	var launches []gateway.LaunchRequest
	orch.GatewayLaunch = func(req gateway.LaunchRequest) error {
		launches = append(launches, req)
		return nil
	}

	tree, _, err := orch.Init(InitOpts{ConversationID: "conv-root", Goal: "failover", RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r1, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-root", ID: "w1", Model: "pool:fast", Goal: "do work"})
	if err != nil || r1.Model != "hub:p1/m1" {
		t.Fatalf("expected initial pick hub:p1/m1, got %+v err=%v", r1, err)
	}

	// Simulate p1/m1 hitting an upstream error_message early-stop -> must auto-failover to hub:p2/m2!
	err = orch.CompleteGatewayTurn(ctx, tree.ID, "w1", r1.TaskID, gateway.TurnOutcome{
		EarlyStopReason: "upstream error_message (stalled 8s after error step)",
		ResultError:     "upstream 502",
	})
	if err != nil {
		t.Fatalf("CompleteGatewayTurn #1: %v", err)
	}
	tr, err := st.LoadTree(tree.ID)
	if err != nil {
		t.Fatal(err)
	}
	w1 := tr.Nodes["w1"]
	if w1.State != store.NodeWorking || w1.Model != "hub:p2/m2" || w1.FailoverCount != 1 {
		t.Fatalf("expected w1 auto-failed over to hub:p2/m2, got %+v", w1)
	}
	if len(launches) != 2 {
		t.Fatalf("expected 2 gateway launches (initial + failover), got %d", len(launches))
	}
	// Root inbox should still have 0 failure mails because failover handled it seamlessly.
	inbox, _ := orch.Inbox("conv-root", tree.ID, false)
	if len(inbox.Messages) != 0 {
		t.Fatalf("expected no failure mail during active failover, got %+v", inbox.Messages)
	}

	// If backup p2/m2 ALSO fails -> no candidates left -> must now mark failed and notify root!
	err = orch.CompleteGatewayTurn(ctx, tree.ID, "w1", r1.TaskID, gateway.TurnOutcome{
		EarlyStopReason: "stream idle timeout (no output for 90s)",
	})
	if err != nil {
		t.Fatalf("CompleteGatewayTurn #2: %v", err)
	}
	tr, _ = st.LoadTree(tree.ID)
	if tr.Nodes["w1"].State != store.NodeError {
		t.Fatalf("expected w1 error after pool exhausted, got %+v", tr.Nodes["w1"])
	}
	inbox, _ = orch.Inbox("conv-root", tree.ID, false)
	if len(inbox.Messages) != 1 || inbox.Messages[0].Kind != "failed" {
		t.Fatalf("expected 1 failed mail in root inbox after pool exhaustion, got %+v", inbox.Messages)
	}
}

func TestCancelFreesConcurrencySlotAndPromotesQueued(t *testing.T) {
	ctx := context.Background()
	st := store.New(t.TempDir())
	orch := New(st, &mockAPI{}, nil)
	orch.Providers = &provider.Config{
		Providers: map[string]*provider.ProviderSpec{
			"hub": {
				URL:            "https://api.example.com/v1",
				WireProtocol:   "openai",
				Models:         []string{"p1/m1"},
				MaxConcurrency: 1,
			},
		},
	}
	var launched []string
	orch.GatewayLaunch = func(req gateway.LaunchRequest) error {
		launched = append(launched, req.NodeID)
		return nil
	}
	tree, _, err := orch.Init(InitOpts{ConversationID: "conv-root", Goal: "cancel promo", RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-root", ID: "w1", Model: "hub:p1/m1", Goal: "g1"}); err != nil {
		t.Fatal(err)
	}
	r2, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-root", ID: "w2", Model: "hub:p1/m1", Goal: "g2"})
	if err != nil || r2.State != store.NodeQueued {
		t.Fatalf("expected w2 queued, got %+v err=%v", r2, err)
	}

	// Cancel w1 -> frees slot -> w2 must be promoted immediately!
	if _, err := orch.Retire("conv-root", tree.ID, "w1", true, true); err != nil {
		t.Fatalf("Retire/cancel w1: %v", err)
	}
	if len(launched) != 2 || launched[1] != "w2" {
		t.Fatalf("expected w2 promoted after canceling w1, launched=%v", launched)
	}
}

func TestOfficialModelsWorkWithoutAnyProvidersConfig(t *testing.T) {
	ctx := context.Background()
	st := store.New(t.TempDir())
	api := &mockAPI{}
	// Simulate zero providers configured (Providers=nil, GatewayLaunch=nil) — pure official Antigravity mode!
	orch := New(st, api, nil)
	orch.Providers = nil
	orch.GatewayLaunch = nil

	tree, _, err := orch.Init(InitOpts{ConversationID: "conv-official-root", Goal: "pure official", RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Spawn with default model (empty), explicit official model, and --provider official.
	r1, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-official-root", ID: "off-default", Role: "analyst", Goal: "default official"})
	if err != nil || r1.Provider != "" || r1.State != store.NodeWorking || r1.ConversationID == "" {
		t.Fatalf("official default spawn failed: %+v err=%v", r1, err)
	}
	r2, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-official-root", ID: "off-gemini", Role: "coder", Model: "gemini-4-hydrogen-high", Goal: "explicit official"})
	if err != nil || r2.Provider != "" || r2.State != store.NodeWorking || r2.ConversationID == "" {
		t.Fatalf("official explicit spawn failed: %+v err=%v", r2, err)
	}
	r3, err := orch.Spawn(ctx, SpawnOpts{CallerConvID: "conv-official-root", ID: "off-flag", Provider: "official", Model: "gemini-4-neon-xhigh", DependsOn: []string{r1.TaskID}, Goal: "dag official"})
	if err != nil || r3.Provider != "" || r3.State != store.NodeQueued {
		t.Fatalf("official DAG queued spawn failed: %+v err=%v", r3, err)
	}

	// Done r1 -> auto-promotes DAG child r3 via official Transport!
	d1, err := orch.Done(ctx, DoneOpts{CallerConvID: r1.ConversationID, Summary: "r1 official done"})
	if err != nil || len(d1.PromotedTasks) != 1 || d1.PromotedTasks[0] != r3.TaskID {
		t.Fatalf("official Done + DAG promotion failed: %+v err=%v", d1, err)
	}

	// Assign warm reuse on idle official r1.
	ar, err := orch.Assign(ctx, AssignOpts{CallerConvID: "conv-official-root", NodeID: "off-default", Goal: "follow-up official"})
	if err != nil || ar.ConversationID != r1.ConversationID {
		t.Fatalf("official Assign failed: %+v err=%v", ar, err)
	}

	// Retire cleanly.
	if _, err := orch.Retire("conv-official-root", tree.ID, "off-gemini", true, false); err != nil {
		t.Fatalf("official Retire failed: %v", err)
	}
}

func TestCompleteGatewayTurnRefreshesUIMirrorAndMultiLevelBoard(t *testing.T) {
	ctx := context.Background()
	st := store.New(t.TempDir())
	appData := t.TempDir()
	orch := New(st, &mockAPI{}, nil)
	orch.Providers = &provider.Config{
		Providers: map[string]*provider.ProviderSpec{
			"ext": {
				URL:          "https://api.example.com/v1",
				WireProtocol: "openai",
				DefaultModel: "m1",
			},
		},
	}
	orch.GatewayLaunch = func(req gateway.LaunchRequest) error { return nil }

	tree, _, err := orch.Init(InitOpts{
		ConversationID: "conv-root-ui",
		Goal:           "UI mirror check",
		RepoRoot:       t.TempDir(),
		AppDataDir:     appData,
	})
	if err != nil {
		t.Fatal(err)
	}

	// L1 official lead spawns L2 external worker.
	lead, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: "conv-root-ui",
		ID:           "lead-a",
		Role:         "lead",
		Goal:         "Lead subtree",
	})
	if err != nil {
		t.Fatal(err)
	}
	w1, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: lead.ConversationID,
		ID:           "w1",
		Role:         "coder",
		Provider:     "ext",
		Goal:         "Build module",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify board.jsonl attributes L1->L2 assign event to lead-a -> @lead-a-w1.
	boardPath := filepath.Join(uimirror.SwarmDir(tree), "board.jsonl")
	bRaw, err := os.ReadFile(boardPath)
	if err != nil {
		t.Fatalf("read board.jsonl: %v", err)
	}
	if !strings.Contains(string(bRaw), `"agent":"lead-a"`) || !strings.Contains(string(bRaw), "@lead-a-w1") {
		t.Fatalf("expected L1->L2 board attribution (lead-a -> @lead-a-w1), got:\n%s", string(bRaw))
	}

	// Simulate worker already called `bswarm done` inside sandbox, then CompleteGatewayTurn runs on host.
	// Delete members.json first to prove CompleteGatewayTurn refreshes SyncRosterAndPlan even on !needAutoClose!
	if _, err := orch.Done(ctx, DoneOpts{TreeID: tree.ID, ActorNodeID: w1.NodeID, Summary: "done via cli"}); err != nil {
		t.Fatal(err)
	}
	membersPath := filepath.Join(uimirror.SwarmDir(tree), "members.json")
	_ = os.Remove(membersPath)

	if err := orch.CompleteGatewayTurn(ctx, tree.ID, w1.NodeID, w1.TaskID, gateway.TurnOutcome{
		ConversationID: "conv-ext-w1",
		Status:         "SUCCESS",
	}); err != nil {
		t.Fatal(err)
	}
	mRaw, err := os.ReadFile(membersPath)
	if err != nil {
		t.Fatalf("expected CompleteGatewayTurn to refresh members.json: %v", err)
	}
	var roster struct {
		Members map[string]struct {
			ConversationID string `json:"conversation_id"`
			WakeMode       string `json:"wake_mode"`
		} `json:"members"`
	}
	if err := json.Unmarshal(mRaw, &roster); err != nil {
		t.Fatal(err)
	}
	if m, ok := roster.Members["lead-a-w1"]; !ok || m.ConversationID != "conv-ext-w1" || m.WakeMode != "mentions" {
		t.Fatalf("expected refreshed idle member with bound convID, got %+v", roster.Members["lead-a-w1"])
	}
}
