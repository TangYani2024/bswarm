package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var spillCounter atomic.Uint64
var ErrNotFound = errors.New("not found")
var ErrAlreadyExists = errors.New("already exists")

// Store manages bswarm state inside PLUGIN_DATA with strict active/archive separation.
type Store struct {
	RootDir string
}

// ResolveDataDir determines the persistent data directory using official Antigravity conventions.
func ResolveDataDir() string {
	if v := strings.TrimSpace(os.Getenv("BSWARM_DATA_DIR")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("PLUGIN_DATA")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR")); v != "" {
		return filepath.Join(v, "plugin_data", "bswarm")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli", "plugin_data", "bswarm")
}

func New(rootDir string) *Store {
	if rootDir == "" {
		rootDir = ResolveDataDir()
	}
	return &Store{RootDir: rootDir}
}

func (s *Store) indexPath() string     { return filepath.Join(s.RootDir, "index.json") }
func (s *Store) indexLockPath() string { return filepath.Join(s.RootDir, "index.lock") }

func (s *Store) TreeDir(treeID string) string {
	return filepath.Join(s.RootDir, "trees", SafeFileKey(treeID))
}

func (s *Store) activePath(treeID string) string {
	return filepath.Join(s.TreeDir(treeID), "active.json")
}

func (s *Store) archivePath(treeID string) string {
	return filepath.Join(s.TreeDir(treeID), "archive.jsonl")
}

func (s *Store) treeLockPath(treeID string) string {
	return filepath.Join(s.TreeDir(treeID), "tree.lock")
}

func (s *Store) WatchLockPath(treeID string) string {
	return filepath.Join(s.TreeDir(treeID), "watch.lock")
}

func (s *Store) InboxPath(treeID, nodeID string) string {
	return filepath.Join(s.TreeDir(treeID), "inbox", SafeFileKey(nodeID)+".jsonl")
}

func (s *Store) ArtifactsDir(treeID, nodeID string) string {
	return filepath.Join(s.TreeDir(treeID), "artifacts", SafeFileKey(nodeID))
}

func (s *Store) WorktreesRoot(treeID string) string {
	return filepath.Join(s.RootDir, "worktrees", SafeFileKey(treeID))
}

func (s *Store) GatewayHomeDir(treeID, nodeID string) string {
	return filepath.Join(s.TreeDir(treeID), "gw_home", SafeFileKey(nodeID))
}

func (s *Store) GatewayLogPath(treeID, nodeID string) string {
	return filepath.Join(s.TreeDir(treeID), "logs", SafeFileKey(nodeID)+".jsonl")
}

func (s *Store) GatewayPromptPath(treeID, nodeID, taskID string) string {
	return filepath.Join(s.TreeDir(treeID), "prompts", SafeFileKey(nodeID)+"_"+SafeFileKey(taskID)+".txt")
}

// TryWatchLock tries to acquire a non-blocking single-instance watchdog lock for treeID.
func (s *Store) TryWatchLock(treeID string) (func(), bool, error) {
	l, ok, err := tryAcquireLock(s.WatchLockPath(treeID))
	if err != nil || !ok {
		return nil, ok, err
	}
	return l.Release, true, nil
}

// WriteAtomic writes data to a temporary file in the same directory and renames it into place.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func appendJSONLine(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

func (s *Store) loadIndexUnlocked() (*GlobalIndex, error) {
	idx := &GlobalIndex{Conversations: map[string]ConvRef{}}
	raw, err := os.ReadFile(s.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, idx); err != nil {
		return nil, fmt.Errorf("corrupt index.json: %w", err)
	}
	if idx.Conversations == nil {
		idx.Conversations = map[string]ConvRef{}
	}
	return idx, nil
}

// ResolveConversation performs an O(1) lookup from conversation_id to {tree_id, node_id}.
// Returns (nil, nil) if the conversation is not part of any active bswarm tree.
func (s *Store) ResolveConversation(convID string) (*ConvRef, error) {
	if strings.TrimSpace(convID) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(s.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var idx GlobalIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, nil
	}
	ref, ok := idx.Conversations[convID]
	if !ok {
		return nil, nil
	}
	return &ref, nil
}

func (s *Store) BindConversation(convID, treeID, nodeID string) error {
	if convID == "" {
		return nil
	}
	lock, err := acquireLock(s.indexLockPath())
	if err != nil {
		return err
	}
	defer lock.Release()
	idx, err := s.loadIndexUnlocked()
	if err != nil {
		return err
	}
	idx.Conversations[convID] = ConvRef{TreeID: treeID, NodeID: nodeID}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(s.indexPath(), data, 0o600)
}

func (s *Store) UnbindConversations(convIDs []string) error {
	if len(convIDs) == 0 {
		return nil
	}
	lock, err := acquireLock(s.indexLockPath())
	if err != nil {
		return err
	}
	defer lock.Release()
	idx, err := s.loadIndexUnlocked()
	if err != nil {
		return err
	}
	changed := false
	for _, cid := range convIDs {
		if _, ok := idx.Conversations[cid]; ok {
			delete(idx.Conversations, cid)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(s.indexPath(), data, 0o600)
}

func (s *Store) LoadTree(treeID string) (*Tree, error) {
	raw, err := os.ReadFile(s.activePath(treeID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var t Tree
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse active.json: %w", err)
	}
	if t.Nodes == nil {
		t.Nodes = map[string]*Node{}
	}
	if t.Tasks == nil {
		t.Tasks = map[string]*Task{}
	}
	if t.CompletedTaskIDs == nil {
		t.CompletedTaskIDs = map[string]bool{}
	}
	return &t, nil
}

func (s *Store) SaveTreeUnlocked(t *Tree) error {
	t.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(s.activePath(t.ID), data, 0o600)
}

// CreateTree writes a brand new active.json under lock (refusing to overwrite an existing tree)
// and registers its root conversation.
func (s *Store) CreateTree(t *Tree) error {
	lock, err := acquireLock(s.treeLockPath(t.ID))
	if err != nil {
		return err
	}
	defer lock.Release()
	if _, err := os.Stat(s.activePath(t.ID)); err == nil {
		return fmt.Errorf("tree %q: %w", t.ID, ErrAlreadyExists)
	}
	if t.CompletedTaskIDs == nil {
		t.CompletedTaskIDs = map[string]bool{}
	}
	if err := s.SaveTreeUnlocked(t); err != nil {
		return err
	}
	if root, ok := t.Nodes["root"]; ok && root.ConversationID != "" {
		return s.BindConversation(root.ConversationID, t.ID, root.ID)
	}
	return nil
}

// UpdateTree runs fn under an exclusive per-tree file lock and saves active.json if fn returns nil.
func (s *Store) UpdateTree(treeID string, fn func(t *Tree) error) (*Tree, error) {
	lock, err := acquireLock(s.treeLockPath(treeID))
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	t, err := s.LoadTree(treeID)
	if err != nil {
		return nil, err
	}
	if err := fn(t); err != nil {
		return nil, err
	}
	if err := s.SaveTreeUnlocked(t); err != nil {
		return nil, err
	}
	return t, nil
}

// ListTrees returns all tree IDs currently stored under RootDir/trees.
func (s *Store) ListTrees() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.RootDir, "trees"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(s.activePath(e.Name())); err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ArchiveNodesUnlocked moves the specified nodes and their completed/canceled tasks
// out of active.json into archive.jsonl so active.json and status stay lean forever.
func (s *Store) ArchiveNodesUnlocked(t *Tree, nodeIDs []string, state NodeState) ([]string, error) {
	now := time.Now().UTC()
	var unboundConvs []string
	nodeSet := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		n, ok := t.Nodes[id]
		if !ok {
			continue
		}
		nodeSet[id] = true
		n.State = state
		n.UpdatedAt = now
		if err := appendJSONLine(s.archivePath(t.ID), ArchiveRecord{
			Kind:       "node",
			ArchivedAt: now,
			Node:       n,
		}); err != nil {
			return nil, err
		}
		if n.ConversationID != "" {
			unboundConvs = append(unboundConvs, n.ConversationID)
		}
		delete(t.Nodes, id)
		t.ArchivedCount++
	}
	for tid, task := range t.Tasks {
		if !nodeSet[task.AssigneeID] {
			continue
		}
		if task.State == TaskDone {
			if t.CompletedTaskIDs == nil {
				t.CompletedTaskIDs = map[string]bool{}
			}
			t.CompletedTaskIDs[tid] = true
		} else if task.State == TaskQueued || task.State == TaskRunning {
			task.State = TaskCanceled
			task.UpdatedAt = now
		}
		if err := appendJSONLine(s.archivePath(t.ID), ArchiveRecord{
			Kind:       "task",
			ArchivedAt: now,
			Task:       task,
		}); err != nil {
			return nil, err
		}
		delete(t.Tasks, tid)
	}
	return unboundConvs, nil
}

// ReadArchive returns archived records for a tree (used only when --archived is explicitly requested).
func (s *Store) ReadArchive(treeID string, limit int) ([]ArchiveRecord, error) {
	f, err := os.Open(s.archivePath(treeID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []ArchiveRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec ArchiveRecord
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			out = append(out, rec)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, sc.Err()
}

// SpillIfLong enforces the pass-by-reference contract: text longer than maxRunes is written
// to artifacts/<node_id>/<label>_<ts>_<seq>.md and replaced with a compact preview + file path.
func (s *Store) SpillIfLong(treeID, nodeID, label, text, existingArtifact string, maxRunes int) (summary, artifact string, spilled bool, err error) {
	if maxRunes <= 0 {
		maxRunes = MaxSummaryRunes
	}
	clean := strings.TrimSpace(text)
	r := []rune(clean)
	if len(r) <= maxRunes {
		return clean, existingArtifact, false, nil
	}
	dir := s.ArtifactsDir(treeID, nodeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", existingArtifact, false, err
	}
	seq := spillCounter.Add(1)
	fname := fmt.Sprintf("%s_%d_%d.md", SafeFileKey(label), time.Now().UTC().UnixNano(), seq)
	outPath := filepath.Join(dir, fname)
	if err := WriteAtomic(outPath, []byte(clean+"\n"), 0o600); err != nil {
		return "", existingArtifact, false, err
	}
	if existingArtifact == "" {
		existingArtifact = outPath
		preview, _ := TruncateRunes(clean, maxRunes)
		return preview, existingArtifact, true, nil
	}
	suffix := " [full: " + outPath + "]"
	suffixRunes := len([]rune(suffix))
	headCap := maxRunes - suffixRunes
	if headCap < 16 {
		preview, _ := TruncateRunes(clean, maxRunes)
		return preview, existingArtifact, true, nil
	}
	preview, _ := TruncateRunes(clean, headCap)
	return preview + suffix, existingArtifact, true, nil
}

// AppendMailUnlocked appends a message to recipient's private inbox and bumps InboxTotalSeq if recipient is in t.
func (s *Store) AppendMailUnlocked(t *Tree, toNodeID string, m Mail) (Mail, error) {
	summary, artifact, _, err := s.SpillIfLong(t.ID, m.From, "mail_"+m.Kind, m.Summary, m.Artifact, MaxSummaryRunes)
	if err != nil {
		return Mail{}, err
	}
	m.Summary = summary
	m.Artifact = artifact
	m.To = toNodeID
	if m.At.IsZero() {
		m.At = time.Now().UTC()
	}
	nextSeq := 1
	if target, ok := t.Nodes[toNodeID]; ok {
		nextSeq = target.InboxTotalSeq + 1
		target.InboxTotalSeq = nextSeq
	} else {
		existing, rerr := s.ReadInbox(t.ID, toNodeID, 0)
		if rerr != nil {
			return Mail{}, fmt.Errorf("read inbox for %q: %w", toNodeID, rerr)
		}
		if len(existing) > 0 {
			nextSeq = existing[len(existing)-1].Seq + 1
		}
	}
	m.Seq = nextSeq
	if err := appendJSONLine(s.InboxPath(t.ID, toNodeID), m); err != nil {
		return Mail{}, err
	}
	return m, nil
}

// ReadInbox returns messages in nodeID's private inbox with seq > afterSeq (deduplicated by Seq).
func (s *Store) ReadInbox(treeID, nodeID string, afterSeq int) ([]Mail, error) {
	f, err := os.Open(s.InboxPath(treeID, nodeID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Mail
	seen := map[int]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m Mail
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			if m.Seq > afterSeq && !seen[m.Seq] {
				seen[m.Seq] = true
				out = append(out, m)
			}
		}
	}
	return out, sc.Err()
}

func normalizeOwnPattern(pattern string) string {
	pat := strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")
	pat = strings.TrimPrefix(pat, "./")
	if pat == "/" || pat == "/**" || pat == "." || pat == "./" {
		return "**"
	}
	pat = strings.TrimPrefix(pat, "/")
	if runtime.GOOS == "windows" {
		pat = strings.ToLower(pat)
	}
	return pat
}

// MatchOwnPattern checks whether relPath matches a glob/directory ownership pattern like
// "src/ui/**", "src/parser.go", "internal/store/", or "*.md".
func MatchOwnPattern(pattern, relPath string) bool {
	pat := normalizeOwnPattern(pattern)
	p := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(relPath), "\\", "/"), "./")
	p = strings.TrimPrefix(p, "/")
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	if pat == "" || p == "" {
		return false
	}
	if pat == "*" || pat == "**" {
		return true
	}
	if strings.HasSuffix(pat, "/**") {
		prefix := strings.TrimSuffix(pat, "/**")
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	if strings.HasSuffix(pat, "/") {
		prefix := strings.TrimSuffix(pat, "/")
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	if p == pat || strings.HasPrefix(p, pat+"/") {
		return true
	}
	if ok, _ := filepath.Match(pat, p); ok {
		return true
	}
	if !strings.Contains(pat, "/") {
		if ok, _ := filepath.Match(pat, filepath.Base(p)); ok {
			return true
		}
	}
	return false
}

// MatchesAnyOwn reports whether relPath matches any ownership rule in owns.
func MatchesAnyOwn(owns []string, relPath string) bool {
	for _, pat := range owns {
		if MatchOwnPattern(pat, relPath) {
			return true
		}
	}
	return false
}

// OwnsOverlap checks whether two ownership pattern sets conflict in a shared workspace.
func OwnsOverlap(a, b []string) (string, string, bool) {
	for _, pa := range a {
		for _, pb := range b {
			na := normalizeOwnPattern(pa)
			nb := normalizeOwnPattern(pb)
			if na == "" || nb == "" {
				continue
			}
			if na == "**" || na == "*" || nb == "**" || nb == "*" {
				return pa, pb, true
			}
			// Root-level glob like "*.go" overlaps any directory/file pattern unless incompatible extension.
			if (!strings.Contains(na, "/") && strings.ContainsAny(na, "*?[")) ||
				(!strings.Contains(nb, "/") && strings.ContainsAny(nb, "*?[")) {
				if MatchOwnPattern(pa, filepath.Base(strings.TrimSuffix(nb, "/**"))) ||
					MatchOwnPattern(pb, filepath.Base(strings.TrimSuffix(na, "/**"))) ||
					strings.HasSuffix(na, "/**") || strings.HasSuffix(nb, "/**") {
					return pa, pb, true
				}
			}
			baseA := strings.TrimSuffix(strings.TrimSuffix(na, "/**"), "/")
			baseB := strings.TrimSuffix(strings.TrimSuffix(nb, "/**"), "/")
			if MatchOwnPattern(pa, baseB) || MatchOwnPattern(pb, baseA) || baseA == baseB {
				return pa, pb, true
			}
			// Same parent dir with wildcards (e.g. internal/*.go vs internal/a*.go).
			if filepath.Dir(na) == filepath.Dir(nb) && (strings.ContainsAny(filepath.Base(na), "*?[") || strings.ContainsAny(filepath.Base(nb), "*?[")) {
				return pa, pb, true
			}
		}
	}
	return "", "", false
}
