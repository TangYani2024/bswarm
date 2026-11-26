package store

import (
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultMaxDepth  = 3
	DefaultMaxActive = 12
	MaxSummaryRunes  = 400
	MaxStatusBytes   = 1800
	MaxStdoutBytes   = 6000
)

type NodeState string

const (
	NodeQueued   NodeState = "queued"
	NodeWorking  NodeState = "working"
	NodeIdle     NodeState = "idle"
	NodeBlocked  NodeState = "blocked"
	NodeError    NodeState = "error"
	NodeRetired  NodeState = "retired"
	NodeCanceled NodeState = "canceled"
)

type TaskState string

const (
	TaskQueued   TaskState = "queued"
	TaskRunning  TaskState = "running"
	TaskDone     TaskState = "done"
	TaskFailed   TaskState = "failed"
	TaskCanceled TaskState = "canceled"
)

type WorkspaceMode string

const (
	WorkspaceShared   WorkspaceMode = "shared"
	WorkspaceWorktree WorkspaceMode = "worktree"
)

type Node struct {
	ID             string        `json:"id"`
	Role           string        `json:"role"`
	ConversationID string        `json:"conversation_id"`
	ParentID       string        `json:"parent_id,omitempty"`
	Depth          int           `json:"depth"`
	Model          string        `json:"model,omitempty"`
	Provider       string        `json:"provider,omitempty"`
	ProviderModel  string        `json:"provider_model,omitempty"`
	Pool           string        `json:"pool,omitempty"`
	ConcurrencyKey string        `json:"concurrency_key,omitempty"`
	WorkerPID      int           `json:"worker_pid,omitempty"`
	WorkerLog      string        `json:"worker_log,omitempty"`
	IsolatedHome   string        `json:"isolated_home,omitempty"`
	FailedTargets  []string      `json:"failed_targets,omitempty"`
	FailoverCount  int           `json:"failover_count,omitempty"`
	State          NodeState     `json:"state"`
	WorkspaceMode  WorkspaceMode `json:"workspace_mode"`
	WorkspacePath  string        `json:"workspace_path"`
	Branch         string        `json:"branch,omitempty"`
	Owns           []string      `json:"owns,omitempty"`
	CurrentTaskID     string        `json:"current_task_id,omitempty"`
	InboxReadSeq      int           `json:"inbox_read_seq"`
	InboxTotalSeq     int           `json:"inbox_total_seq"`
	InboxDeliveredSeq int           `json:"inbox_delivered_seq"`
	TouchedFiles      []string      `json:"touched_files,omitempty"`
	LastAlertKey      string        `json:"last_alert_key,omitempty"`
	LastNudgeKey      string        `json:"last_nudge_key,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

func (n *Node) IsRoot() bool {
	return n.ParentID == ""
}

func (n *Node) IsActive() bool {
	return n.State != NodeRetired && n.State != NodeCanceled
}

type Task struct {
	ID           string    `json:"id"`
	AssigneeID   string    `json:"assignee_id"`
	CreatedBy    string    `json:"created_by"`
	Goal         string    `json:"goal"`
	SpecFile     string    `json:"spec_file,omitempty"`
	DependsOn    []string  `json:"depends_on,omitempty"`
	VerifyCmd    string    `json:"verify_cmd,omitempty"`
	State        TaskState `json:"state"`
	Summary      string    `json:"summary,omitempty"`
	Artifact     string    `json:"artifact,omitempty"`
	ChangedFiles []string  `json:"changed_files,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Mail struct {
	Seq      int       `json:"seq"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Kind     string    `json:"kind"` // assign | done | ask | reply | alert
	TaskID   string    `json:"task_id,omitempty"`
	Summary  string    `json:"summary"`
	Artifact string    `json:"artifact,omitempty"`
	At       time.Time `json:"at"`
}

type Tree struct {
	Version       int              `json:"version"`
	ID            string           `json:"id"`
	Goal          string           `json:"goal"`
	RepoRoot      string           `json:"repo_root"`
	AppDataDir    string           `json:"app_data_dir,omitempty"`
	MaxDepth         int              `json:"max_depth"`
	MaxActive        int              `json:"max_active"`
	NextTaskSeq      int              `json:"next_task_seq"`
	MirrorSeq        int              `json:"mirror_seq,omitempty"`
	ArchivedCount    int              `json:"archived_count"`
	CompletedTaskIDs map[string]bool  `json:"completed_task_ids,omitempty"`
	Nodes            map[string]*Node `json:"nodes"`
	Tasks            map[string]*Task `json:"tasks"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

type ArchiveRecord struct {
	Kind       string    `json:"kind"` // "node" | "task"
	ArchivedAt time.Time `json:"archived_at"`
	Node       *Node     `json:"node,omitempty"`
	Task       *Task     `json:"task,omitempty"`
}

type ConvRef struct {
	TreeID string `json:"tree_id"`
	NodeID string `json:"node_id"`
}

type GlobalIndex struct {
	Conversations map[string]ConvRef `json:"conversations"`
}

// SafeFileKey converts a hierarchical node ID like "lead-ui/w1" into a safe filename token.
func SafeFileKey(id string) string {
	s := strings.TrimSpace(id)
	s = strings.ReplaceAll(s, "/", "__")
	s = strings.ReplaceAll(s, "\\", "__")
	s = strings.ReplaceAll(s, "..", "_")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	res := b.String()
	if res == "" || res == "." || res == ".." {
		return "node"
	}
	return res
}

// NormalizeRelPath cleans a path into forward-slash relative form when possible.
func NormalizeRelPath(base, p string) string {
	if p == "" {
		return ""
	}
	cleaned := filepath.Clean(p)
	if base != "" && filepath.IsAbs(cleaned) {
		if rel, err := filepath.Rel(base, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
			cleaned = rel
		}
	}
	cleaned = filepath.ToSlash(cleaned)
	cleaned = strings.TrimPrefix(cleaned, "./")
	return cleaned
}

// TruncateBytes hard-caps a string to at most maxBytes without splitting a UTF-8 rune.
func TruncateBytes(s string, maxBytes int, suffix string) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	limit := maxBytes - len(suffix)
	if limit <= 0 {
		return suffix[:maxBytes]
	}
	// Walk back to valid UTF-8 boundary.
	for limit > 0 && (s[limit]&0xC0) == 0x80 {
		limit--
	}
	return s[:limit] + suffix
}

// TruncateRunes limits text to maxRunes runes.
func TruncateRunes(s string, maxRunes int) (string, bool) {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s, false
	}
	if maxRunes <= 3 {
		return string(r[:maxRunes]), true
	}
	return string(r[:maxRunes-3]) + "...", true
}
