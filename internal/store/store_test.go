package store

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreLifecycleSpillAndArchive(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)

	tree := &Tree{
		Version:   1,
		ID:        "tree-1",
		Goal:      "test tree",
		RepoRoot:  dir,
		MaxDepth:  DefaultMaxDepth,
		MaxActive: DefaultMaxActive,
		Nodes: map[string]*Node{
			"root": {
				ID:             "root",
				Role:           "coordinator",
				ConversationID: "conv-root",
				Depth:          0,
				State:          NodeWorking,
				WorkspaceMode:  WorkspaceShared,
				WorkspacePath:  dir,
				CreatedAt:      time.Now().UTC(),
				UpdatedAt:      time.Now().UTC(),
			},
			"lead": {
				ID:             "lead",
				Role:           "lead",
				ConversationID: "conv-lead",
				ParentID:       "root",
				Depth:          1,
				State:          NodeWorking,
				WorkspaceMode:  WorkspaceShared,
				WorkspacePath:  dir,
				CreatedAt:      time.Now().UTC(),
				UpdatedAt:      time.Now().UTC(),
			},
		},
		Tasks:     map[string]*Task{},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateTree(tree); err != nil {
		t.Fatalf("CreateTree: %v", err)
	}
	if err := st.BindConversation("conv-lead", "tree-1", "lead"); err != nil {
		t.Fatalf("BindConversation: %v", err)
	}

	ref, err := st.ResolveConversation("conv-lead")
	if err != nil || ref == nil || ref.TreeID != "tree-1" || ref.NodeID != "lead" {
		t.Fatalf("ResolveConversation mismatch: %+v, err=%v", ref, err)
	}
	if none, err := st.ResolveConversation("unknown-conv"); err != nil || none != nil {
		t.Fatalf("expected nil for unknown conv, got %+v err=%v", none, err)
	}

	// Test pass-by-reference spill on >400 rune mail.
	longText := strings.Repeat("详细逆向结果", 120) // 720 runes
	updated, err := st.UpdateTree("tree-1", func(tr *Tree) error {
		m, err := st.AppendMailUnlocked(tr, "root", Mail{
			From:    "lead",
			Kind:    "done",
			Summary: longText,
		})
		if err != nil {
			return err
		}
		if len([]rune(m.Summary)) > MaxSummaryRunes {
			t.Errorf("summary was not capped: len=%d", len([]rune(m.Summary)))
		}
		if m.Artifact == "" {
			t.Errorf("expected spilled artifact file path")
		}
		if _, err := os.Stat(m.Artifact); err != nil {
			t.Errorf("spilled artifact missing: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateTree: %v", err)
	}
	if updated.Nodes["root"].InboxTotalSeq != 1 {
		t.Fatalf("expected root inbox seq=1, got %d", updated.Nodes["root"].InboxTotalSeq)
	}

	// Read unread inbox.
	mails, err := st.ReadInbox("tree-1", "root", 0)
	if err != nil || len(mails) != 1 {
		t.Fatalf("ReadInbox: len=%d err=%v", len(mails), err)
	}

	// Archive lead node and verify active/archive separation.
	var unbound []string
	_, err = st.UpdateTree("tree-1", func(tr *Tree) error {
		var aerr error
		unbound, aerr = st.ArchiveNodesUnlocked(tr, []string{"lead"}, NodeRetired)
		return aerr
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := st.UnbindConversations(unbound); err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	loaded, err := st.LoadTree("tree-1")
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if _, exists := loaded.Nodes["lead"]; exists {
		t.Fatalf("archived node still in active.json")
	}
	if loaded.ArchivedCount != 1 {
		t.Fatalf("expected archived_count=1, got %d", loaded.ArchivedCount)
	}
	recs, err := st.ReadArchive("tree-1", 10)
	if err != nil || len(recs) != 1 || recs[0].Node.ID != "lead" {
		t.Fatalf("ReadArchive mismatch: %+v err=%v", recs, err)
	}
	if refAfter, _ := st.ResolveConversation("conv-lead"); refAfter != nil {
		t.Fatalf("expected archived conversation to be unbound")
	}
}

func TestConcurrentUpdateTree(t *testing.T) {
	st := New(t.TempDir())
	tr := &Tree{
		Version: 1, ID: "conc", RepoRoot: ".", MaxDepth: 3, MaxActive: 12,
		Nodes: map[string]*Node{"root": {ID: "root", State: NodeWorking}},
		Tasks: map[string]*Task{},
	}
	if err := st.CreateTree(tr); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.UpdateTree("conc", func(t *Tree) error {
				t.NextTaskSeq++
				return nil
			})
			if err != nil {
				t.Errorf("concurrent update error: %v", err)
			}
		}()
	}
	wg.Wait()
	final, err := st.LoadTree("conc")
	if err != nil {
		t.Fatal(err)
	}
	if final.NextTaskSeq != 16 {
		t.Fatalf("expected NextTaskSeq=16, got %d", final.NextTaskSeq)
	}
}

func TestDAGAndOwnsMatching(t *testing.T) {
	tr := &Tree{
		Tasks: map[string]*Task{
			"t1": {ID: "t1", State: TaskDone},
			"t2": {ID: "t2", State: TaskQueued, DependsOn: []string{"t1"}},
			"t3": {ID: "t3", State: TaskQueued, DependsOn: []string{"t2"}},
		},
	}
	if err := ValidateNewTaskDeps(tr, "t2", []string{"t3"}); err == nil {
		t.Fatalf("expected cycle error for t2 -> t3 -> t2")
	}
	if err := ValidateNewTaskDeps(tr, "t4", []string{"does-not-exist"}); err == nil {
		t.Fatalf("expected error for unknown dependency task ID")
	}
	if DependenciesSatisfied(tr, []string{"canceled-and-archived"}) {
		t.Fatalf("absent uncompleted task must NOT be treated as satisfied")
	}
	tr.CompletedTaskIDs = map[string]bool{"archived-done": true}
	if !DependenciesSatisfied(tr, []string{"archived-done"}) {
		t.Fatalf("explicitly completed archived task should be satisfied")
	}
	ready := ReadyQueuedTasks(tr)
	if len(ready) != 1 || ready[0].ID != "t2" {
		t.Fatalf("expected only t2 ready, got %+v", ready)
	}

	if !MatchOwnPattern("src/ui/**", "src/ui/button/index.tsx") {
		t.Fatalf("expected src/ui/** to match nested file")
	}
	if MatchOwnPattern("src/ui/**", "src/api/server.go") {
		t.Fatalf("did not expect src/ui/** to match src/api/server.go")
	}
	if _, _, overlap := OwnsOverlap([]string{"src/ui/**"}, []string{"src/ui/App.tsx"}); !overlap {
		t.Fatalf("expected overlap between src/ui/** and src/ui/App.tsx")
	}
	if _, _, overlap := OwnsOverlap([]string{"src/ui/**"}, []string{"src/api/**"}); overlap {
		t.Fatalf("did not expect overlap between src/ui/** and src/api/**")
	}
}
