package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/TangYani2024/bswarm/internal/agentapi"
	"github.com/TangYani2024/bswarm/internal/store"
)

type mockAPI struct {
	mu      sync.Mutex
	seq     int
	created []agentapi.NewConversationOpts
	sent    []struct {
		to, title, content string
	}
}

func (m *mockAPI) NewConversation(_ context.Context, opts agentapi.NewConversationOpts) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.created = append(m.created, opts)
	return fmt.Sprintf("conv-child-%d", m.seq), nil
}

func (m *mockAPI) SendMessage(_ context.Context, recipientID, title, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, struct{ to, title, content string }{recipientID, title, content})
	return nil
}

func (m *mockAPI) GetMetadata(_ context.Context, convID string) (*agentapi.Metadata, error) {
	return &agentapi.Metadata{ConversationID: convID}, nil
}

func TestThreeLevelTreeEncapsulationReuseAndCascadeRetire(t *testing.T) {
	ctx := context.Background()
	st := store.New(t.TempDir())
	api := &mockAPI{}
	orch := New(st, api, nil)
	repo := t.TempDir()

	// 1. Root initializes tree and spawns Lead-A.
	tree, created, err := orch.Init(InitOpts{
		ConversationID: "conv-root",
		Goal:           "Refactor core modules",
		RepoRoot:       repo,
		AppDataDir:     t.TempDir(),
		MaxDepth:       3,
		MaxActive:      12,
	})
	if err != nil || !created {
		t.Fatalf("Init: created=%v err=%v", created, err)
	}

	leadRes, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: "conv-root",
		ID:           "lead-a",
		Role:         "lead",
		Goal:         "Coordinate parser + lexer rewrite",
		Owns:         []string{"src/frontend/**"},
	})
	if err != nil {
		t.Fatalf("spawn lead-a: %v", err)
	}

	// Ownership overlap guard in shared workspace must block conflicting sibling.
	if _, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: "conv-root",
		ID:           "lead-b",
		Role:         "lead",
		Goal:         "Touch same frontend dir",
		Owns:         []string{"src/frontend/parser.go"},
	}); err == nil {
		t.Fatalf("expected ownership overlap error")
	}

	// 2. Subagent-of-subagent: Lead-A spawns Worker-1 and DAG-dependent Worker-2.
	w1Res, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: leadRes.ConversationID,
		ID:           "w1",
		Role:         "coder",
		Goal:         "Implement lexer",
		VerifyCmd:    "echo verify-ok",
	})
	if err != nil {
		t.Fatalf("spawn w1: %v", err)
	}
	if w1Res.NodeID != "lead-a/w1" {
		t.Fatalf("expected hierarchical node id lead-a/w1, got %s", w1Res.NodeID)
	}

	w2Res, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: leadRes.ConversationID,
		ID:           "w2",
		Role:         "verifier",
		Goal:         "Verify lexer output",
		DependsOn:    []string{w1Res.TaskID},
	})
	if err != nil {
		t.Fatalf("spawn w2: %v", err)
	}
	if w2Res.State != store.NodeQueued || w2Res.ConversationID != "" {
		t.Fatalf("expected w2 queued without burning tokens yet, got state=%s conv=%s", w2Res.State, w2Res.ConversationID)
	}

	// Verify gate failure must block bswarm done.
	orch.RunVerify = func(context.Context, string, string) (string, error) {
		return "FAIL TestLexer", errors.New("exit status 1")
	}
	if _, err := orch.Done(ctx, DoneOpts{
		CallerConvID: w1Res.ConversationID,
		Summary:      "lexer finished",
	}); err == nil {
		t.Fatalf("expected verify gate to reject done")
	}

	// Verify passes -> W1 done notifies ONLY Lead-A (NOT Root!), becomes idle, and auto-promotes W2!
	orch.RunVerify = func(context.Context, string, string) (string, error) {
		return "PASS", nil
	}
	doneW1, err := orch.Done(ctx, DoneOpts{
		CallerConvID: w1Res.ConversationID,
		Summary:      strings.Repeat("L", 600), // force spill to artifact
	})
	if err != nil {
		t.Fatalf("w1 Done: %v", err)
	}
	if doneW1.Notified != "lead-a" {
		t.Fatalf("expected w1 to notify lead-a, got %q", doneW1.Notified)
	}
	if doneW1.Artifact == "" {
		t.Fatalf("expected long summary to spill to artifact")
	}
	if len(doneW1.PromotedTasks) != 1 || doneW1.PromotedTasks[0] != w2Res.TaskID {
		t.Fatalf("expected w2 task %s to auto-promote, got %v", w2Res.TaskID, doneW1.PromotedTasks)
	}

	// Strict tree encapsulation check: Root inbox must be completely empty!
	rootInbox, err := orch.Inbox("conv-root", "", false)
	if err != nil || len(rootInbox.Messages) != 0 {
		t.Fatalf("root should NOT receive w1 child noise, got %+v", rootInbox.Messages)
	}
	// Lead-A inbox must have W1's compact receipt.
	leadInbox, err := orch.Inbox(leadRes.ConversationID, "", false)
	if err != nil || len(leadInbox.Messages) != 1 || leadInbox.Messages[0].From != "lead-a/w1" {
		t.Fatalf("expected 1 receipt from lead-a/w1 in lead-a inbox, got %+v", leadInbox.Messages)
	}

	// 3. Role reuse: Lead-A spawns another "coder" with --reuse -> reuses idle lead-a/w1!
	reuseRes, err := orch.Spawn(ctx, SpawnOpts{
		CallerConvID: leadRes.ConversationID,
		Role:         "coder",
		Goal:         "Fix edge case in lexer",
		Reuse:        true,
	})
	if err != nil {
		t.Fatalf("reuse spawn: %v", err)
	}
	if !reuseRes.Reused || reuseRes.NodeID != "lead-a/w1" || reuseRes.ConversationID != w1Res.ConversationID {
		t.Fatalf("expected reuse of lead-a/w1, got %+v", reuseRes)
	}

	// 4. Cascade retire: retiring lead-a without --cascade must fail; with --cascade archives lead-a, w1, w2 together!
	if _, err := orch.Retire("conv-root", "", "lead-a", false, false); err == nil {
		t.Fatalf("expected non-cascade retire with active descendants to fail")
	}
	retRes, err := orch.Retire("conv-root", "", "lead-a", true, false)
	if err != nil {
		t.Fatalf("cascade retire: %v", err)
	}
	if len(retRes.ArchivedNodes) != 3 {
		t.Fatalf("expected 3 archived nodes (w1, w2, lead-a), got %v", retRes.ArchivedNodes)
	}

	// Status stays strictly lean even after archiving 100+ historical nodes.
	_, _ = st.UpdateTree(tree.ID, func(tr *store.Tree) error {
		tr.ArchivedCount = 117
		return nil
	})
	statusText, err := orch.RenderStatus("conv-root", "", false)
	if err != nil {
		t.Fatalf("RenderStatus: %v", err)
	}
	if len(statusText) > store.MaxStatusBytes || strings.Contains(statusText, "lead-a") {
		t.Fatalf("status bloated or leaked archived nodes:\n%s", statusText)
	}
	if !strings.Contains(statusText, "archived=117") {
		t.Fatalf("expected archived count line, got:\n%s", statusText)
	}
}
