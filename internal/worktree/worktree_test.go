package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		if _, err := runGit(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	mustGit("init")
	mustGit("config", "user.email", "test@bswarm.local")
	mustGit("config", "user.name", "bswarm-test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit("add", ".")
	mustGit("commit", "-m", "initial")
	return dir
}

func TestWorktreeCreateInspectMergeRemove(t *testing.T) {
	repo := initTestRepo(t)
	wtRoot := filepath.Join(t.TempDir(), "wt")

	info, err := Create(repo, wtRoot, "tree-12345678", "lead-a/w1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatalf("worktree path missing: %v", err)
	}

	// Make a commit inside the worktree.
	newFile := filepath.Join(info.Path, "feature.txt")
	if err := os.WriteFile(newFile, []byte("from worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(info.Path, "add", "feature.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(info.Path, "commit", "-m", "add feature"); err != nil {
		t.Fatal(err)
	}

	rep, err := Inspect(repo, info.Path, info.Branch)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if rep.Dirty || rep.AheadCommits != 1 || len(rep.Files) != 1 || rep.Files[0] != "feature.txt" {
		t.Fatalf("unexpected inspect report: %+v", rep)
	}

	if _, err := Merge(repo, info.Branch, "squash", "merge w1"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("merged file missing in main repo: %v", err)
	}

	if err := Remove(repo, info.Path, info.Branch, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		t.Fatalf("expected worktree dir removed")
	}
}
