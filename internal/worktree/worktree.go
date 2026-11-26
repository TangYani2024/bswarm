package worktree

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/TangYani2024/bswarm/internal/store"
)

type Info struct {
	Path   string
	Branch string
}

type DiffReport struct {
	Branch       string   `json:"branch"`
	AheadCommits int      `json:"ahead_commits"`
	Dirty        bool     `json:"dirty"`
	StatSummary  string   `json:"stat_summary"`
	Files        []string `json:"files,omitempty"`
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// GitRoot returns the top-level git directory containing dir, or "" if dir is not inside a git repo.
func GitRoot(dir string) string {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return out
}

// CurrentHead returns the current HEAD commit SHA of repoRoot.
func CurrentHead(repoRoot string) (string, error) {
	root := GitRoot(repoRoot)
	if root == "" {
		return "", fmt.Errorf("%s is not a git repository", repoRoot)
	}
	return runGit(root, "rev-parse", "HEAD")
}

// ResetHard rolls repoRoot back to sha (used to keep multi-branch merges atomic on failure).
func ResetHard(repoRoot, sha string) error {
	root := GitRoot(repoRoot)
	if root == "" || sha == "" {
		return nil
	}
	_, err := runGit(root, "reset", "--hard", sha)
	return err
}

// Create provisions an isolated git worktree and dedicated branch for a swarm node.
func Create(repoRoot, wtRoot, treeID, nodeID string) (*Info, error) {
	root := GitRoot(repoRoot)
	if root == "" {
		return nil, fmt.Errorf("%s is not a git repository (cannot create worktree)", repoRoot)
	}
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktrees root: %w", err)
	}
	shortTree := store.SafeFileKey(treeID)
	if len(shortTree) > 8 {
		shortTree = shortTree[:8]
	}
	slug := store.SafeFileKey(nodeID)
	baseWT := filepath.Join(wtRoot, slug)
	baseBranch := fmt.Sprintf("bswarm/%s/%s", shortTree, slug)
	wtPath := baseWT
	branch := baseBranch

	// Loop until both path and branch name are unused (safe under rapid re-spawns or kept branches).
	for i := 1; i <= 1000; i++ {
		_, statErr := os.Stat(wtPath)
		_, refErr := runGit(root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
		if os.IsNotExist(statErr) && refErr != nil {
			break
		}
		wtPath = fmt.Sprintf("%s-%d", baseWT, i)
		branch = fmt.Sprintf("%s-%d", baseBranch, i)
	}

	if _, err := runGit(root, "worktree", "add", "-b", branch, wtPath, "HEAD"); err != nil {
		return nil, err
	}
	return &Info{Path: wtPath, Branch: branch}, nil
}

// Inspect returns dirty status, commit count ahead of HEAD, and compact diff stats for a worktree.
func Inspect(repoRoot, wtPath, branch string) (*DiffReport, error) {
	rep := &DiffReport{Branch: branch}
	if wtPath != "" {
		if st, err := runGit(wtPath, "status", "--porcelain"); err == nil && st != "" {
			rep.Dirty = true
		}
	}
	root := GitRoot(repoRoot)
	if root == "" || branch == "" {
		return rep, nil
	}
	if cntOut, err := runGit(root, "rev-list", "--count", "HEAD.."+branch); err == nil {
		_, _ = fmt.Sscanf(cntOut, "%d", &rep.AheadCommits)
	}
	if names, err := runGit(root, "diff", "--name-only", "HEAD..."+branch); err == nil && names != "" {
		for _, ln := range strings.Split(names, "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				rep.Files = append(rep.Files, ln)
			}
		}
	}
	if stat, err := runGit(root, "diff", "--shortstat", "HEAD..."+branch); err == nil {
		rep.StatSummary = stat
	}
	return rep, nil
}

// Merge safely merges a worker branch into repoRoot using strategy ("merge" | "squash" | "ff-only").
func Merge(repoRoot, branch, strategy, commitMsg string) (string, error) {
	root := GitRoot(repoRoot)
	if root == "" {
		return "", fmt.Errorf("%s is not a git repository", repoRoot)
	}
	if st, err := runGit(root, "status", "--porcelain"); err != nil {
		return "", err
	} else if st != "" {
		return "", fmt.Errorf("main working tree has uncommitted changes; commit or stash before merging %s", branch)
	}
	if strategy == "" {
		strategy = "merge"
	}
	if commitMsg == "" {
		commitMsg = "bswarm: merge " + branch
	}
	switch strategy {
	case "ff-only":
		return runGit(root, "merge", "--ff-only", branch)
	case "squash":
		out, err := runGit(root, "merge", "--squash", branch)
		if err != nil {
			_, _ = runGit(root, "reset", "--merge")
			return "", err
		}
		st, serr := runGit(root, "status", "--porcelain")
		if serr != nil {
			_, _ = runGit(root, "reset", "--merge")
			return "", serr
		}
		if st == "" {
			return "already up to date", nil
		}
		cOut, err := runGit(root, "commit", "-m", commitMsg)
		if err != nil {
			_, _ = runGit(root, "reset", "--merge")
			return "", err
		}
		return strings.TrimSpace(out + "\n" + cOut), nil
	case "merge":
		out, err := runGit(root, "merge", "--no-ff", "-m", commitMsg, branch)
		if err != nil {
			_, _ = runGit(root, "merge", "--abort")
			return "", err
		}
		return out, nil
	default:
		return "", fmt.Errorf("unsupported merge strategy %q (want merge, squash, or ff-only)", strategy)
	}
}

// Remove cleans up a worktree directory and optionally deletes its temporary branch.
func Remove(repoRoot, wtPath, branch string, deleteBranch bool) error {
	root := GitRoot(repoRoot)
	if wtPath != "" {
		if root != "" {
			_, _ = runGit(root, "worktree", "remove", "--force", wtPath)
		}
		_ = os.RemoveAll(wtPath)
	}
	if root != "" {
		_, _ = runGit(root, "worktree", "prune")
		if deleteBranch && branch != "" {
			_, _ = runGit(root, "branch", "-D", branch)
		}
	}
	return nil
}
