package uimirror

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TangYani2024/bswarm/internal/store"
)

const (
	markerFile       = ".bswarm_mirror"
	maxArchivedShown = 16
)

// MemberName maps a bswarm hierarchical node ID to a valid official Swarm UI roster name
// matching NAME_RUNE (letters/digits/underscore/hyphen) so @mentions & graph edges render cleanly.
func MemberName(nodeID string) string {
	if nodeID == "" || nodeID == "root" {
		return "coordinator"
	}
	s := strings.ReplaceAll(strings.TrimSpace(nodeID), "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" || out == "all" || out == "coordinator" {
		return "node-" + store.SafeFileKey(nodeID)
	}
	return strings.ToLower(out)
}

func resolveAppDataDir(t *store.Tree) string {
	if t != nil && strings.TrimSpace(t.AppDataDir) != "" {
		return strings.TrimSpace(t.AppDataDir)
	}
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Pick whichever product dir already owns the root conversation's brain dir.
	rootConv := ""
	if t != nil && t.Nodes["root"] != nil {
		rootConv = t.Nodes["root"].ConversationID
	}
	for _, prod := range []string{"antigravity-cli", "antigravity", "antigravity-ide"} {
		cand := filepath.Join(home, ".gemini", prod)
		if rootConv != "" {
			if fi, err := os.Stat(filepath.Join(cand, "brain", rootConv)); err == nil && fi.IsDir() {
				return cand
			}
		}
	}
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

// SwarmDir returns the official Antigravity Swarm UI directory for tree t's root conversation.
func SwarmDir(t *store.Tree) string {
	if os.Getenv("BSWARM_NO_UI_MIRROR") == "1" || t == nil {
		return ""
	}
	root := t.Nodes["root"]
	if root == nil || strings.TrimSpace(root.ConversationID) == "" {
		return ""
	}
	appData := resolveAppDataDir(t)
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, "brain", root.ConversationID, ".system_generated", "swarm")
}

// canWriteDir ensures we NEVER overwrite a native /swarm session's files.
// Only writes if the swarm dir does not exist yet, has no members.json, or carries .bswarm_mirror.
func canWriteDir(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, markerFile)); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "members.json")); err == nil {
		// Native /swarm owns this conversation's board; refuse to touch it.
		return false
	}
	return true
}

func ensureMirrorDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	marker := filepath.Join(dir, markerFile)
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		_ = store.WriteAtomic(marker, []byte("managed by bswarm (read-only UI projection)\n"), 0o600)
	}
	return nil
}

type officialMember struct {
	ConversationID string `json:"conversation_id"`
	Role           string `json:"role"`
	Model          string `json:"model,omitempty"`
	WakeMode       string `json:"wake_mode"`
	WaitSince      int    `json:"wait_since"`
	LastReadSeq    int    `json:"last_read_seq"`
	Killed         bool   `json:"killed"`
	WorkspaceMode  string `json:"workspace_mode,omitempty"`
	Workspace      string `json:"workspace,omitempty"`
}

type officialRoster struct {
	Title   string                    `json:"title"`
	Members map[string]officialMember `json:"members"`
}

type officialPlan struct {
	Goal    planGoal          `json:"goal"`
	Tasks   map[string]string `json:"tasks"`
	Threads []planThread      `json:"threads"`
}

type planGoal struct {
	Title string `json:"title"`
	Line  string `json:"line"`
}

type planThread struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Goal  string   `json:"goal"`
	Tasks []string `json:"tasks"`
}

type officialBoardMsg struct {
	Seq   int    `json:"seq"`
	Agent string `json:"agent"`
	Text  string `json:"text"`
	Time  string `json:"time"`
}

// SyncRosterAndPlan writes members.json and plan.json for the official Swarm UI (CLI TUI + Desktop pane).
// Best-effort: UI projection errors never fail core bswarm operations.
func SyncRosterAndPlan(st *store.Store, t *store.Tree) error {
	dir := SwarmDir(t)
	if !canWriteDir(dir) {
		return nil
	}
	if err := ensureMirrorDir(dir); err != nil {
		return err
	}

	roster := officialRoster{
		Title:   t.Goal,
		Members: make(map[string]officialMember, len(t.Nodes)),
	}
	if roster.Title == "" {
		roster.Title = "bswarm " + t.ID
	}

	plan := officialPlan{
		Goal: planGoal{
			Title: shortTitle(roster.Title, 64),
			Line:  fmt.Sprintf("bswarm tree %s: %d active, %d archived (depth <= %d)", t.ID, len(t.Nodes), t.ArchivedCount, t.MaxDepth),
		},
		Tasks: map[string]string{},
	}

	// Active nodes.
	activeIDs := make([]string, 0, len(t.Nodes))
	for id := range t.Nodes {
		activeIDs = append(activeIDs, id)
	}
	sort.Strings(activeIDs)

	for _, id := range activeIDs {
		n := t.Nodes[id]
		mName := MemberName(n.ID)
		role := "worker"
		if n.IsRoot() {
			role = "coordinator"
		}
		wake := "mentions"
		if n.State == store.NodeWorking {
			wake = "working"
		}
		wsMode := "inherit"
		if n.WorkspaceMode == store.WorkspaceWorktree {
			wsMode = "branch"
		}
		roster.Members[mName] = officialMember{
			ConversationID: n.ConversationID,
			Role:           role,
			Model:          n.Model,
			WakeMode:       wake,
			WaitSince:      t.MirrorSeq,
			LastReadSeq:    t.MirrorSeq,
			Killed:         false,
			WorkspaceMode:  wsMode,
			Workspace:      n.WorkspacePath,
		}
		if !n.IsRoot() {
			desc := n.Role
			if n.CurrentTaskID != "" {
				if tk, ok := t.Tasks[n.CurrentTaskID]; ok && tk.Goal != "" {
					desc = shortTitle(tk.Goal, 72)
				}
			}
			plan.Tasks[mName] = desc
		}
	}

	// Include up to maxArchivedShown latest archived nodes as killed=true so user can inspect recent history
	// without ever letting 100+ corpses bloat the UI.
	if st != nil && t.ArchivedCount > 0 {
		recs, _ := st.ReadArchive(t.ID, maxArchivedShown*2)
		shown := 0
		for i := len(recs) - 1; i >= 0 && shown < maxArchivedShown; i-- {
			r := recs[i]
			if r.Kind != "node" || r.Node == nil || r.Node.IsRoot() {
				continue
			}
			mName := MemberName(r.Node.ID)
			if _, exists := roster.Members[mName]; exists {
				continue
			}
			roster.Members[mName] = officialMember{
				ConversationID: r.Node.ConversationID,
				Role:           "worker",
				Model:          r.Node.Model,
				WakeMode:       "mentions",
				LastReadSeq:    t.MirrorSeq,
				Killed:         true,
				Workspace:      r.Node.WorkspacePath,
			}
			shown++
		}
	}

	// Group active non-root nodes into threads by depth-1 subtree root.
	threadMembers := map[string][]string{}
	threadOrder := []string{}
	for _, id := range activeIDs {
		n := t.Nodes[id]
		if n.IsRoot() {
			continue
		}
		top := topBranchOf(t, n)
		if _, ok := threadMembers[top]; !ok {
			threadOrder = append(threadOrder, top)
		}
		threadMembers[top] = append(threadMembers[top], MemberName(n.ID))
	}
	sort.Strings(threadOrder)
	for _, topID := range threadOrder {
		topNode := t.Nodes[topID]
		tGoal := topID
		if topNode != nil && topNode.CurrentTaskID != "" {
			if tk, ok := t.Tasks[topNode.CurrentTaskID]; ok && tk.Goal != "" {
				tGoal = shortTitle(tk.Goal, 72)
			}
		}
		plan.Threads = append(plan.Threads, planThread{
			ID:    "t-" + MemberName(topID),
			Title: MemberName(topID),
			Goal:  tGoal,
			Tasks: threadMembers[topID],
		})
	}

	mBytes, err := json.MarshalIndent(roster, "", "  ")
	if err != nil {
		return err
	}
	if err := store.WriteAtomic(filepath.Join(dir, "members.json"), mBytes, 0o600); err != nil {
		return err
	}

	pBytes, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	if err := store.WriteAtomic(filepath.Join(dir, "plan.json"), pBytes, 0o600); err != nil {
		return err
	}

	// Ensure board.jsonl exists so both CLI live watcher and desktop sidecar detect the swarm immediately.
	boardPath := filepath.Join(dir, "board.jsonl")
	if _, err := os.Stat(boardPath); os.IsNotExist(err) {
		_ = store.WriteAtomic(boardPath, []byte{}, 0o600)
	}
	return nil
}

// AppendEvent appends a single compact UI-only event line to board.jsonl using seq.
func AppendEvent(t *store.Tree, seq int, fromNodeID, toNodeID, kind, taskID, summary, artifact string) error {
	dir := SwarmDir(t)
	if !canWriteDir(dir) {
		return nil
	}
	if err := ensureMirrorDir(dir); err != nil {
		return err
	}
	fromName := MemberName(fromNodeID)
	toName := ""
	if toNodeID != "" {
		toName = MemberName(toNodeID)
	}
	var tb strings.Builder
	if toName != "" {
		fmt.Fprintf(&tb, "@%s ", toName)
	}
	switch kind {
	case "done":
		tb.WriteString("[done] ")
	case "failed":
		tb.WriteString("[failed] ")
	case "alert":
		tb.WriteString("[stall] ")
	default:
		if kind != "" {
			fmt.Fprintf(&tb, "[%s] ", kind)
		}
	}
	if taskID != "" {
		fmt.Fprintf(&tb, "(%s) ", taskID)
	}
	s, _ := store.TruncateRunes(strings.TrimSpace(summary), store.MaxSummaryRunes)
	tb.WriteString(s)
	if artifact != "" {
		fmt.Fprintf(&tb, "\nArtifact: %s", artifact)
	}

	msg := officialBoardMsg{
		Seq:   seq,
		Agent: fromName,
		Text:  tb.String(),
		Time:  time.Now().Format("2006-01-02 15:04:05"),
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(filepath.Join(dir, "board.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// RebuildBoard backfills board.jsonl from current active tasks & private inboxes (used by `bswarm sync-ui`).
func RebuildBoard(st *store.Store, t *store.Tree) error {
	dir := SwarmDir(t)
	if !canWriteDir(dir) {
		return nil
	}
	if err := ensureMirrorDir(dir); err != nil {
		return err
	}
	type ev struct {
		at       time.Time
		from, to string
		kind     string
		taskID   string
		summary  string
		artifact string
	}
	var evs []ev
	// Include active task assignments in chronological order.
	for _, tk := range t.Tasks {
		evs = append(evs, ev{
			at:       tk.CreatedAt,
			from:     tk.CreatedBy,
			to:       tk.AssigneeID,
			kind:     "assign",
			taskID:   tk.ID,
			summary:  tk.Goal,
			artifact: tk.SpecFile,
		})
		if tk.State == store.TaskDone || tk.State == store.TaskFailed {
			parentID := tk.CreatedBy
			evs = append(evs, ev{
				at:       tk.UpdatedAt,
				from:     tk.AssigneeID,
				to:       parentID,
				kind:     string(tk.State),
				taskID:   tk.ID,
				summary:  tk.Summary,
				artifact: tk.Artifact,
			})
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].at.Before(evs[j].at) })

	var buf strings.Builder
	seq := 0
	for _, e := range evs {
		seq++
		var tb strings.Builder
		if e.to != "" {
			fmt.Fprintf(&tb, "@%s ", MemberName(e.to))
		}
		if e.kind == "done" {
			tb.WriteString("[done] ")
		} else {
			fmt.Fprintf(&tb, "[%s] ", e.kind)
		}
		if e.taskID != "" {
			fmt.Fprintf(&tb, "(%s) ", e.taskID)
		}
		s, _ := store.TruncateRunes(strings.TrimSpace(e.summary), store.MaxSummaryRunes)
		tb.WriteString(s)
		if e.artifact != "" {
			fmt.Fprintf(&tb, "\nArtifact: %s", e.artifact)
		}
		tm := e.at
		if tm.IsZero() {
			tm = time.Now()
		}
		line, _ := json.Marshal(officialBoardMsg{
			Seq:   seq,
			Agent: MemberName(e.from),
			Text:  tb.String(),
			Time:  tm.Local().Format("2006-01-02 15:04:05"),
		})
		buf.Write(line)
		buf.WriteByte('\n')
	}
	t.MirrorSeq = seq
	if err := store.WriteAtomic(filepath.Join(dir, "board.jsonl"), []byte(buf.String()), 0o600); err != nil {
		return err
	}
	return SyncRosterAndPlan(st, t)
}

func topBranchOf(t *store.Tree, n *store.Node) string {
	cur := n
	seen := 0
	for cur != nil && cur.ParentID != "" && cur.ParentID != "root" && seen < 16 {
		next, ok := t.Nodes[cur.ParentID]
		if !ok {
			break
		}
		cur = next
		seen++
	}
	if cur == nil {
		return n.ID
	}
	return cur.ID
}

func shortTitle(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	out, _ := store.TruncateRunes(s, maxRunes)
	return out
}
