package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TangYani2024/bswarm/internal/store"
)

func TestHookGuardsZeroNoiseAndStopPullback(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(repo, 0o755)
	st := store.New(filepath.Join(dir, "data"))
	h := NewHandler(st)

	// 1. Non-swarm conversation gets empty {} with zero injection.
	res := h.HandlePreInvocation(&HookInput{ConversationID: "plain-conv"})
	if b, _ := json.Marshal(res); string(b) != "{}" {
		t.Fatalf("expected {} for non-swarm conv, got %s", string(b))
	}

	// 2. Setup active tree with 2 shared nodes + 1 worktree node.
	wtPath := filepath.Join(dir, "wt-c")
	_ = os.MkdirAll(wtPath, 0o755)
	tr := &store.Tree{
		Version: 1, ID: "t1", RepoRoot: repo, MaxDepth: 3, MaxActive: 12,
		Nodes: map[string]*store.Node{
			"root": {ID: "root", Role: "coordinator", ConversationID: "conv-root", Depth: 0, State: store.NodeWorking, WorkspaceMode: store.WorkspaceShared, WorkspacePath: repo, InboxTotalSeq: 2, InboxReadSeq: 1},
			"w-a":  {ID: "w-a", Role: "ui", ParentID: "root", ConversationID: "conv-a", Depth: 1, State: store.NodeWorking, WorkspaceMode: store.WorkspaceShared, WorkspacePath: repo, Owns: []string{"src/ui/**"}, CurrentTaskID: "t1"},
			"w-b":  {ID: "w-b", Role: "api", ParentID: "root", ConversationID: "conv-b", Depth: 1, State: store.NodeWorking, WorkspaceMode: store.WorkspaceShared, WorkspacePath: repo, Owns: []string{"src/api/**"}, CurrentTaskID: "t2"},
			"w-wt": {ID: "w-wt", Role: "iso", ParentID: "root", ConversationID: "conv-wt", Depth: 1, State: store.NodeWorking, WorkspaceMode: store.WorkspaceWorktree, WorkspacePath: wtPath, Branch: "bswarm/t1/w-wt", CurrentTaskID: "t3"},
		},
		Tasks:     map[string]*store.Task{},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateTree(tr); err != nil {
		t.Fatal(err)
	}
	_ = st.BindConversation("conv-a", "t1", "w-a")
	_ = st.BindConversation("conv-b", "t1", "w-b")
	_ = st.BindConversation("conv-wt", "t1", "w-wt")

	// w-a touching w-b's file -> DENY.
	deny := h.HandlePreToolUse(&HookInput{
		ConversationID: "conv-a",
		CWD:            repo,
		ToolCall:       &ToolCallPayload{Name: "edit_file", Args: map[string]any{"Path": filepath.Join(repo, "src/api/server.go")}},
	})
	preOut, ok := deny.(*PreToolOutput)
	if !ok || preOut.Decision != "deny" {
		t.Fatalf("expected deny when w-a edits w-b's file, got %#v", deny)
	}

	// w-a touching its own file -> explicit ALLOW.
	allow := h.HandlePreToolUse(&HookInput{
		ConversationID: "conv-a",
		CWD:            repo,
		ToolCall:       &ToolCallPayload{Name: "edit_file", Args: map[string]any{"Path": filepath.Join(repo, "src/ui/App.tsx")}},
	})
	if out, ok := allow.(*PreToolOutput); !ok || out.Decision != "allow" {
		t.Fatalf("expected decision=allow for own file, got %#v", allow)
	}

	// Non-swarm conversation PreToolUse -> MUST still return explicit decision=allow (never {}).
	plainPre := h.HandlePreToolUse(&HookInput{
		ConversationID: "plain-conv",
		ToolCall:       &ToolCallPayload{Name: "edit_file", Args: map[string]any{"Path": "/tmp/a.txt"}},
	})
	if out, ok := plainPre.(*PreToolOutput); !ok || out.Decision != "allow" {
		t.Fatalf("expected decision=allow for non-swarm PreToolUse, got %#v", plainPre)
	}

	// Worktree node trying to edit main repo -> DENY.
	wtDeny := h.HandlePreToolUse(&HookInput{
		ConversationID: "conv-wt",
		CWD:            wtPath,
		ToolCall:       &ToolCallPayload{Name: "write_file", Args: map[string]any{"Path": filepath.Join(repo, "main.go")}},
	})
	if out, ok := wtDeny.(*PreToolOutput); !ok || out.Decision != "deny" {
		t.Fatalf("expected deny when worktree node writes main repo, got %#v", wtDeny)
	}

	// PreInvocation injects tiny unread notice for root.
	inv := h.HandlePreInvocation(&HookInput{ConversationID: "conv-root"})
	invOut, ok := inv.(PreInvocationOutput)
	if !ok || len(invOut.InjectSteps) != 1 || !strings.Contains(invOut.InjectSteps[0].SystemMessage.SystemMessage, "1 unread") {
		t.Fatalf("expected unread inject for root, got %#v", inv)
	}

	// Stop hook pulls back leaf child that forgot bswarm done.
	stopRes := h.HandleStop(&HookInput{ConversationID: "conv-a", TerminationReason: "NO_TOOL_CALL", ContinuationActive: false})
	if so, ok := stopRes.(StopOutput); !ok || so.Decision != "continue" {
		t.Fatalf("expected continue on forgot-done child, got %#v", stopRes)
	}
	// Stop hook MUST allow an intermediate lead (depth >= 1) to sleep when it has active children (P1-15).
	_, _ = st.UpdateTree("t1", func(tree *store.Tree) error {
		tree.Nodes["w-a/sub1"] = &store.Node{
			ID: "w-a/sub1", Role: "sub", ParentID: "w-a", ConversationID: "conv-sub1",
			Depth: 2, State: store.NodeWorking, CurrentTaskID: "t9",
		}
		return nil
	})
	leadSleep := h.HandleStop(&HookInput{ConversationID: "conv-a", TerminationReason: "NO_TOOL_CALL", ContinuationActive: false})
	if so, ok := leadSleep.(StopOutput); !ok || so.Decision != "stop" {
		t.Fatalf("expected decision=stop for intermediate lead waiting on active children, got %#v", leadSleep)
	}
	// When continuationActive is true, Stop hook MUST return decision=stop (not loop).
	stopLoop := h.HandleStop(&HookInput{ConversationID: "conv-b", TerminationReason: "NO_TOOL_CALL", ContinuationActive: true})
	if so, ok := stopLoop.(StopOutput); !ok || so.Decision != "stop" {
		t.Fatalf("expected decision=stop when continuationActive=true, got %#v", stopLoop)
	}

	// Native post_message guards: unaddressed [done] and >4KB bloat.
	natDeny1 := h.HandlePreToolUse(&HookInput{
		ConversationID: "native-worker",
		ToolCall:       &ToolCallPayload{Name: "post_message", Args: map[string]any{"text": "[done] fixed parser"}},
	})
	if out, ok := natDeny1.(*PreToolOutput); !ok || out.Decision != "deny" {
		t.Fatalf("expected deny on native unaddressed [done], got %#v", natDeny1)
	}
	natDeny2 := h.HandlePreToolUse(&HookInput{
		ConversationID: "native-worker",
		ToolCall:       &ToolCallPayload{Name: "post_message", Args: map[string]any{"text": "@coordinator " + strings.Repeat("X", 5000)}},
	})
	if out, ok := natDeny2.(*PreToolOutput); !ok || out.Decision != "deny" {
		t.Fatalf("expected deny on >4KB native post, got %#v", natDeny2)
	}
	natAllow := h.HandlePreToolUse(&HookInput{
		ConversationID: "native-worker",
		ToolCall:       &ToolCallPayload{Name: "post_message", Args: map[string]any{"text": "@coordinator [done] see artifacts/report.md"}},
	})
	if out, ok := natAllow.(*PreToolOutput); !ok || out.Decision != "allow" {
		t.Fatalf("expected decision=allow on valid concise native post, got %#v", natAllow)
	}
}

func TestHookEnvFallbackForIsolatedGatewayWorkers(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(repo, 0o755)
	st := store.New(filepath.Join(dir, "data"))
	h := NewHandler(st)

	tr := &store.Tree{
		Version: 1, ID: "t-gw", RepoRoot: repo, MaxDepth: 3, MaxActive: 12,
		Nodes: map[string]*store.Node{
			"root":  {ID: "root", Role: "coordinator", ConversationID: "conv-root", Depth: 0, State: store.NodeWorking, WorkspaceMode: store.WorkspaceShared, WorkspacePath: repo},
			"gw-w1": {ID: "gw-w1", Role: "worker", ParentID: "root", Provider: "hub", ProviderModel: "vendor-a/ds", Depth: 1, State: store.NodeWorking, WorkspaceMode: store.WorkspaceShared, WorkspacePath: repo, Owns: []string{"web/**"}, CurrentTaskID: "t1"},
		},
		Tasks:     map[string]*store.Task{},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateTree(tr); err != nil {
		t.Fatal(err)
	}

	// Gateway worker before ConversationID is even bound: resolved via BSWARM_TREE_ID + BSWARM_NODE_ID!
	t.Setenv("BSWARM_TREE_ID", "t-gw")
	t.Setenv("BSWARM_NODE_ID", "gw-w1")
	t.Setenv("BSWARM_BIN", "/usr/local/bin/bswarm")

	deny := h.HandlePreToolUse(&HookInput{
		ConversationID: "unbound-gw-conv",
		CWD:            repo,
		ToolCall:       &ToolCallPayload{Name: "write_file", Args: map[string]any{"Path": filepath.Join(repo, "server/secret.go")}},
	})
	if out, ok := deny.(*PreToolOutput); !ok || out.Decision != "deny" || !strings.Contains(out.Reason, "artifacts dir") {
		t.Fatalf("expected --owns deny with artifacts dir tip via env fallback, got %#v", deny)
	}

	stopRes := h.HandleStop(&HookInput{ConversationID: "unbound-gw-conv", TerminationReason: "NO_TOOL_CALL"})
	if so, ok := stopRes.(StopOutput); !ok || so.Decision != "continue" || !strings.Contains(so.Reason, "bswarm done") {
		t.Fatalf("expected continue with bswarm command name, got %#v", stopRes)
	}
}
