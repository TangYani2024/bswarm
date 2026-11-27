package uimirror

import (
	"bufio"
	"bytes"
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

// ChannelName maps a branch node ID to a valid official Swarm UI channel name
// matching ^[a-z][a-z0-9-]{0,23}$ and avoiding reserved names "main", "all", "here".
func ChannelName(nodeID string) string {
	s := strings.ToLower(strings.TrimSpace(nodeID))
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "s-node"
	} else if out[0] < 'a' || out[0] > 'z' || out == "main" || out == "all" || out == "here" {
		out = "s-" + out
	}
	if len(out) > 24 {
		out = out[:24]
	}
	out = strings.TrimRight(out, "-")
	if out == "" {
		return "s-node"
	}
	return out
}

// ResolveAppDataDir resolves the host Antigravity app data dir owning tree t's root conversation.
func ResolveAppDataDir(t *store.Tree) string {
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

// HostTranscriptPath returns the host Antigravity compact transcript path watched by official swarm_board.
func HostTranscriptPath(appDataDir, convID string) string {
	appDataDir = strings.TrimSpace(appDataDir)
	convID = strings.TrimSpace(convID)
	if appDataDir == "" || convID == "" {
		return ""
	}
	return filepath.Join(appDataDir, "brain", convID, ".system_generated", "logs", "transcript.jsonl")
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
	appData := ResolveAppDataDir(t)
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
	ConversationID string   `json:"conversation_id"`
	Role           string   `json:"role"`
	Model          string   `json:"model,omitempty"`
	WakeMode       string   `json:"wake_mode"`
	WaitSince      int      `json:"wait_since"`
	LastReadSeq    int      `json:"last_read_seq"`
	Killed         bool     `json:"killed"`
	WorkspaceMode  string   `json:"workspace_mode,omitempty"`
	Workspace      string   `json:"workspace,omitempty"`
	Channels       []string `json:"channels,omitempty"`
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

type boardControl struct {
	Op      string `json:"op"`
	Channel string `json:"channel,omitempty"`
	Topic   string `json:"topic,omitempty"`
}

type officialBoardMsg struct {
	Seq     int           `json:"seq"`
	Agent   string        `json:"agent"`
	Channel string        `json:"channel,omitempty"`
	Text    string        `json:"text"`
	Time    string        `json:"time"`
	Control *boardControl `json:"control,omitempty"`
}

// SyncRosterAndPlan writes members.json, plan.json, view.json, and view.html for the official Swarm UI.
// Best-effort: UI projection errors never fail core bswarm operations.
func SyncRosterAndPlan(st *store.Store, t *store.Tree) error {
	dir := SwarmDir(t)
	if !canWriteDir(dir) {
		return nil
	}
	if err := ensureMirrorDir(dir); err != nil {
		return err
	}

	branchCh := branchChannels(t)
	roster := officialRoster{
		Title:   t.Goal,
		Members: make(map[string]officialMember, len(t.Nodes)+1),
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

	activeIDs := make([]string, 0, len(t.Nodes))
	for id := range t.Nodes {
		activeIDs = append(activeIDs, id)
	}
	sort.Strings(activeIDs)

	rootConv := ""
	if root := t.Nodes["root"]; root != nil {
		rootConv = root.ConversationID
	}

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
		var channels []string
		if n.IsRoot() {
			channels = []string{"*"}
		} else if ch, ok := branchCh[topBranchOf(t, n)]; ok {
			channels = []string{ch}
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
			Channels:       channels,
		}
		if !n.IsRoot() {
			desc := n.Role
			if n.CurrentTaskID != "" {
				if tk, ok := t.Tasks[n.CurrentTaskID]; ok && tk.Goal != "" {
					desc = tk.Goal
				}
			}
			plan.Tasks[mName] = shortTitle(fmt.Sprintf("[L%d][%s] %s", n.Depth, nodeProviderTag(n), desc), 96)
		}
	}

	// Virtual narrator keeps official Overview active even when enable-swarm-overview is off.
	roster.Members["bswarm-narrator"] = officialMember{
		ConversationID: rootConv,
		Role:           "narrator",
		Model:          "bswarm-mirror",
		WakeMode:       "mentions",
		WaitSince:      t.MirrorSeq,
		LastReadSeq:    t.MirrorSeq,
		Killed:         false,
		Channels:       []string{"*"},
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
			var channels []string
			if ch, ok := branchCh[topBranchOf(t, r.Node)]; ok {
				channels = []string{ch}
			}
			roster.Members[mName] = officialMember{
				ConversationID: r.Node.ConversationID,
				Role:           "worker",
				Model:          r.Node.Model,
				WakeMode:       "mentions",
				LastReadSeq:    t.MirrorSeq,
				Killed:         true,
				Workspace:      r.Node.WorkspacePath,
				Channels:       channels,
			}
			shown++
		}
	}

	// Group active non-root nodes into threads by depth-1 subtree root in hierarchical DFS order.
	threadMembers := map[string][]string{}
	threadOrder := []string{}
	for _, n := range orderedNonRootNodes(t) {
		top := topBranchOf(t, n)
		if _, ok := threadMembers[top]; !ok {
			threadOrder = append(threadOrder, top)
		}
		threadMembers[top] = append(threadMembers[top], MemberName(n.ID))
	}
	for _, topID := range threadOrder {
		plan.Threads = append(plan.Threads, planThread{
			ID:    "t-" + MemberName(topID),
			Title: MemberName(topID),
			Goal:  branchTopic(t, topID),
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

	if err := syncViewFiles(dir, t); err != nil {
		return err
	}

	// Ensure board.jsonl exists so both CLI live watcher and desktop sidecar detect the swarm immediately.
	boardPath := filepath.Join(dir, "board.jsonl")
	if _, err := os.Stat(boardPath); os.IsNotExist(err) {
		_ = store.WriteAtomic(boardPath, []byte{}, 0o600)
	}
	return nil
}

// AppendEvent appends a single compact UI-only event line to board.jsonl using seq,
// emitting a channel create control line first when routing into a new subtree channel.
func AppendEvent(t *store.Tree, seq int, fromNodeID, toNodeID, kind, taskID, summary, artifact string) error {
	dir := SwarmDir(t)
	if !canWriteDir(dir) {
		return nil
	}
	if err := ensureMirrorDir(dir); err != nil {
		return err
	}

	boardPath := filepath.Join(dir, "board.jsonl")
	created, lastSeq := readBoardState(boardPath)
	nextSeq := seq
	if nextSeq <= lastSeq {
		nextSeq = lastSeq + 1
	}

	branchCh := branchChannels(t)
	ch, topic := resolveEventChannel(t, branchCh, fromNodeID, toNodeID)
	nowStr := time.Now().Format("2006-01-02 15:04:05")

	var buf bytes.Buffer
	if ch != "" && !created[ch] {
		ctrlLine, err := json.Marshal(makeCreateControlMsg(nextSeq, ch, topic, nowStr))
		if err != nil {
			return err
		}
		buf.Write(ctrlLine)
		buf.WriteByte('\n')
		nextSeq++
	}

	msg := officialBoardMsg{
		Seq:     nextSeq,
		Agent:   MemberName(fromNodeID),
		Channel: ch,
		Text:    formatBoardText(toNodeID, kind, taskID, summary, artifact),
		Time:    nowStr,
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	buf.Write(line)
	buf.WriteByte('\n')

	if t != nil && nextSeq > t.MirrorSeq {
		t.MirrorSeq = nextSeq
	}

	f, err := os.OpenFile(boardPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(buf.Bytes())
	return err
}

// RebuildBoard backfills board.jsonl from current active tasks & refreshes UI mirror files.
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
		order    int
		from, to string
		kind     string
		taskID   string
		summary  string
		artifact string
	}
	var evs []ev
	for _, tk := range t.Tasks {
		evs = append(evs, ev{
			at:       tk.CreatedAt,
			order:    0,
			from:     tk.CreatedBy,
			to:       tk.AssigneeID,
			kind:     "assign",
			taskID:   tk.ID,
			summary:  tk.Goal,
			artifact: tk.SpecFile,
		})
		if tk.State == store.TaskDone || tk.State == store.TaskFailed {
			evs = append(evs, ev{
				at:       tk.UpdatedAt,
				order:    1,
				from:     tk.AssigneeID,
				to:       tk.CreatedBy,
				kind:     string(tk.State),
				taskID:   tk.ID,
				summary:  tk.Summary,
				artifact: tk.Artifact,
			})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool {
		if !evs[i].at.Equal(evs[j].at) {
			return evs[i].at.Before(evs[j].at)
		}
		if evs[i].taskID != evs[j].taskID {
			return evs[i].taskID < evs[j].taskID
		}
		return evs[i].order < evs[j].order
	})

	branchCh := branchChannels(t)
	created := map[string]bool{}
	var buf bytes.Buffer
	seq := 0
	for _, e := range evs {
		tm := e.at
		if tm.IsZero() {
			tm = time.Now()
		}
		ts := tm.Local().Format("2006-01-02 15:04:05")
		ch, topic := resolveEventChannel(t, branchCh, e.from, e.to)
		if ch != "" && !created[ch] {
			seq++
			ctrlLine, _ := json.Marshal(makeCreateControlMsg(seq, ch, topic, ts))
			buf.Write(ctrlLine)
			buf.WriteByte('\n')
			created[ch] = true
		}
		seq++
		line, _ := json.Marshal(officialBoardMsg{
			Seq:     seq,
			Agent:   MemberName(e.from),
			Channel: ch,
			Text:    formatBoardText(e.to, e.kind, e.taskID, e.summary, e.artifact),
			Time:    ts,
		})
		buf.Write(line)
		buf.WriteByte('\n')
	}
	t.MirrorSeq = seq
	if err := store.WriteAtomic(filepath.Join(dir, "board.jsonl"), buf.Bytes(), 0o600); err != nil {
		return err
	}
	return SyncRosterAndPlan(st, t)
}

func formatBoardText(toNodeID, kind, taskID, summary, artifact string) string {
	var tb strings.Builder
	if toNodeID != "" {
		fmt.Fprintf(&tb, "@%s ", MemberName(toNodeID))
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
	return tb.String()
}

func makeCreateControlMsg(seq int, ch, topic, ts string) officialBoardMsg {
	return officialBoardMsg{
		Seq:     seq,
		Agent:   "coordinator",
		Channel: ch,
		Text:    "Created #" + ch,
		Time:    ts,
		Control: &boardControl{
			Op:      "create",
			Channel: ch,
			Topic:   topic,
		},
	}
}

func readBoardState(boardPath string) (map[string]bool, int) {
	created := map[string]bool{}
	lastSeq := 0
	f, err := os.Open(boardPath)
	if err != nil {
		return created, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg officialBoardMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Seq > lastSeq {
			lastSeq = msg.Seq
		}
		if msg.Control != nil && msg.Control.Op == "create" {
			ch := msg.Control.Channel
			if ch == "" {
				ch = msg.Channel
			}
			if ch != "" {
				created[ch] = true
			}
		}
	}
	return created, lastSeq
}

func branchChannels(t *store.Tree) map[string]string {
	if t == nil {
		return nil
	}
	counts := map[string]int{}
	hasDeep := map[string]bool{}
	for _, n := range t.Nodes {
		if n == nil || n.IsRoot() || !n.IsActive() {
			continue
		}
		top := topBranchOf(t, n)
		if top == "" || top == "root" {
			continue
		}
		counts[top]++
		if n.Depth >= 2 {
			hasDeep[top] = true
		}
	}
	out := map[string]string{}
	for top, cnt := range counts {
		if cnt >= 2 || hasDeep[top] {
			out[top] = ChannelName(top)
		}
	}
	return out
}

func branchTopic(t *store.Tree, topID string) string {
	if t != nil {
		if topNode := t.Nodes[topID]; topNode != nil {
			if topNode.CurrentTaskID != "" {
				if tk, ok := t.Tasks[topNode.CurrentTaskID]; ok && tk.Goal != "" {
					return shortTitle(tk.Goal, 72)
				}
			}
			if topNode.Role != "" {
				return shortTitle(topNode.Role, 72)
			}
		}
	}
	return shortTitle(topID, 72)
}

func resolveEventChannel(t *store.Tree, branchCh map[string]string, fromNodeID, toNodeID string) (string, string) {
	if t == nil || fromNodeID == "" || fromNodeID == "root" || toNodeID == "" || toNodeID == "root" {
		return "", ""
	}
	fromNode := t.Nodes[fromNodeID]
	toNode := t.Nodes[toNodeID]
	if fromNode != nil && toNode != nil {
		fromTop := topBranchOf(t, fromNode)
		toTop := topBranchOf(t, toNode)
		if fromTop != "" && fromTop != "root" && fromTop == toTop {
			if ch, ok := branchCh[fromTop]; ok {
				return ch, branchTopic(t, fromTop)
			}
			if fromNode.Depth >= 2 || toNode.Depth >= 2 {
				return ChannelName(fromTop), branchTopic(t, fromTop)
			}
		}
		return "", ""
	}
	if fromNode != nil && fromNode.Depth >= 2 && fromNode.ParentID == toNodeID {
		top := topBranchOf(t, fromNode)
		if top != "" && top != "root" {
			return ChannelName(top), branchTopic(t, top)
		}
	}
	if toNode != nil && toNode.Depth >= 2 && toNode.ParentID == fromNodeID {
		top := topBranchOf(t, toNode)
		if top != "" && top != "root" {
			return ChannelName(top), branchTopic(t, top)
		}
	}
	return "", ""
}

func nodeProviderTag(n *store.Node) string {
	if n == nil {
		return "official"
	}
	if strings.TrimSpace(n.Provider) != "" {
		return strings.TrimSpace(n.Provider)
	}
	return "official"
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
