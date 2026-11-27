package uimirror

import (
	"bytes"
	_ "embed"
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
	maxViewWorkstreams = 40
	maxViewThreads     = 400
	maxViewAttention   = 30
	maxViewMilestones  = 20
	maxViewRisks       = 15
	maxViewJSONBytes   = 58 * 1024
)

//go:embed view.html
var embeddedViewHTML []byte

type viewJSON struct {
	Version        int               `json:"version"`
	AsOfSeq        int               `json:"as_of_seq"`
	UpdatedAt      string            `json:"updated_at"`
	Headline       string            `json:"headline"`
	Goal           viewGoal          `json:"goal"`
	RunningSummary string            `json:"running_summary"`
	Phase          viewPhase         `json:"phase"`
	Workstreams    []viewWorkstream  `json:"workstreams"`
	Threads        []viewThread      `json:"threads"`
	Now            map[string]string `json:"now"`
	Attention      []viewAttention   `json:"attention"`
	Milestones     []viewMilestone   `json:"milestones"`
	Risks          []viewRisk        `json:"risks"`
}

type viewGoal struct {
	Title string `json:"title"`
	Line  string `json:"line"`
}

type viewPhase struct {
	Label string `json:"label"`
	Index int    `json:"index"`
	Total int    `json:"total"`
}

type viewRef struct {
	Member string `json:"member"`
}

type viewAction struct {
	Type   string `json:"type"`
	Target any    `json:"target"`
	Label  string `json:"label"`
}

type viewWorkstream struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	Owners    []string     `json:"owners"`
	State     string       `json:"state"`
	BlockedOn string       `json:"blocked_on,omitempty"`
	Note      string       `json:"note"`
	Refs      []viewRef    `json:"refs"`
	Actions   []viewAction `json:"actions,omitempty"`
}

type viewThread struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Goal        string   `json:"goal"`
	Now         string   `json:"now"`
	State       string   `json:"state"`
	Workstreams []string `json:"workstreams"`
}

type viewAttention struct {
	ID       string       `json:"id"`
	Kind     string       `json:"kind"`
	Severity string       `json:"severity"`
	Title    string       `json:"title"`
	Why      string       `json:"why"`
	Refs     []viewRef    `json:"refs"`
	Actions  []viewAction `json:"actions,omitempty"`
}

type viewMilestone struct {
	ID   string    `json:"id"`
	Text string    `json:"text"`
	Refs []viewRef `json:"refs"`
}

type viewRisk struct {
	ID   string    `json:"id"`
	Text string    `json:"text"`
	Refs []viewRef `json:"refs"`
}

func syncViewFiles(dir string, t *store.Tree) error {
	v := buildViewJSON(t)
	vBytes, err := marshalBoundedView(v)
	if err != nil {
		return err
	}
	if err := store.WriteAtomic(filepath.Join(dir, "view.json"), vBytes, 0o600); err != nil {
		return err
	}
	// Anti-flicker: keep view.html mtime/size untouched when content has not changed.
	viewHTMLPath := filepath.Join(dir, "view.html")
	if existing, err := os.ReadFile(viewHTMLPath); err != nil || !bytes.Equal(existing, embeddedViewHTML) {
		if err := store.WriteAtomic(viewHTMLPath, embeddedViewHTML, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func marshalBoundedView(v viewJSON) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil || len(b) <= maxViewJSONBytes {
		return b, err
	}
	// Trim optional trailing sections first, then drop indentation if still large.
	if len(v.Milestones) > 8 {
		v.Milestones = v.Milestones[:8]
	}
	if len(v.Risks) > 8 {
		v.Risks = v.Risks[:8]
	}
	if len(v.Threads) > 40 {
		v.Threads = v.Threads[:40]
	}
	b, err = json.Marshal(v)
	if err != nil || len(b) <= maxViewJSONBytes {
		return b, err
	}
	v.Milestones = nil
	v.Risks = nil
	return json.Marshal(v)
}

func buildViewJSON(t *store.Tree) viewJSON {
	appData := ResolveAppDataDir(t)
	goalText := strings.TrimSpace(t.Goal)
	if goalText == "" {
		goalText = "bswarm " + t.ID
	}

	nodes := orderedNonRootNodes(t)
	childrenByParent := map[string][]*store.Node{}
	var workingNames []string
	workingCnt, queuedCnt, blockedCnt, errorCnt, doneCnt := 0, 0, 0, 0, 0

	for _, n := range nodes {
		childrenByParent[n.ParentID] = append(childrenByParent[n.ParentID], n)
		switch n.State {
		case store.NodeWorking:
			workingCnt++
			workingNames = append(workingNames, workingLabel(t, n))
		case store.NodeQueued:
			queuedCnt++
		case store.NodeBlocked:
			blockedCnt++
		case store.NodeError:
			errorCnt++
		default:
			if nodeHasFailedTask(t, n) {
				errorCnt++
			} else {
				doneCnt++
			}
		}
	}

	phase := computePhase(workingCnt, queuedCnt, blockedCnt, errorCnt, doneCnt)
	progParts := []string{}
	if workingCnt > 0 {
		progParts = append(progParts, fmt.Sprintf("%d working", workingCnt))
	}
	if queuedCnt > 0 {
		progParts = append(progParts, fmt.Sprintf("%d queued", queuedCnt))
	}
	if blockedCnt+errorCnt > 0 {
		progParts = append(progParts, fmt.Sprintf("%d issue", blockedCnt+errorCnt))
	}
	if doneCnt > 0 {
		progParts = append(progParts, fmt.Sprintf("%d done", doneCnt))
	}
	progStr := "awaiting tasks"
	if len(progParts) > 0 {
		progStr = strings.Join(progParts, ", ")
	}

	headline := shortTitle(fmt.Sprintf("%s — %s (%d active)", goalText, progStr, len(t.Nodes)), 240)
	runSum := "No active workers"
	if len(workingNames) > 0 {
		runSum = shortTitle("Working: "+strings.Join(workingNames, ", "), 180)
	} else if phase.Label == "Completed" {
		runSum = "All assigned work completed"
	} else if errorCnt+blockedCnt > 0 {
		runSum = fmt.Sprintf("%d node(s) need attention", errorCnt+blockedCnt)
	}

	nowMap := map[string]string{}
	if root := t.Nodes["root"]; root != nil {
		nowMap["coordinator"] = shortTitle(string(root.State), 46)
	}

	usedWSIDs := map[string]int{}
	nodeWSID := map[string]string{}
	workstreams := make([]viewWorkstream, 0, minInt(len(nodes), maxViewWorkstreams))
	for _, n := range nodes {
		mName := MemberName(n.ID)
		nowMap[mName] = shortTitle(nodeNowSummary(t, n), 46)
		if len(workstreams) >= maxViewWorkstreams {
			continue
		}
		wsID := safeViewID("ws-", mName, usedWSIDs)
		nodeWSID[n.ID] = wsID
		workstreams = append(workstreams, buildWorkstream(t, n, wsID, mName, appData, childrenByParent[n.ID]))
	}

	threads := buildThreads(t, nodes, nodeWSID)
	attention := buildAttention(t, nodes)
	milestones := buildMilestones(t)
	risks := buildRisks(nodes)

	return viewJSON{
		Version:        1,
		AsOfSeq:        t.MirrorSeq,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		Headline:       headline,
		Goal:           viewGoal{Title: shortTitle(goalText, 80), Line: shortTitle(fmt.Sprintf("Tree %s · %s · depth <= %d", t.ID, progStr, t.MaxDepth), 120)},
		RunningSummary: runSum,
		Phase:          phase,
		Workstreams:    workstreams,
		Threads:        threads,
		Now:            nowMap,
		Attention:      attention,
		Milestones:     milestones,
		Risks:          risks,
	}
}

func computePhase(working, queued, blocked, errCnt, done int) viewPhase {
	if working == 0 && queued == 0 && (blocked > 0 || errCnt > 0) {
		return viewPhase{Label: "Blocked", Index: 1, Total: 2}
	}
	if working > 0 || queued > 0 || (done == 0 && blocked == 0 && errCnt == 0) {
		return viewPhase{Label: "Active", Index: 1, Total: 2}
	}
	return viewPhase{Label: "Completed", Index: 2, Total: 2}
}

func buildWorkstream(t *store.Tree, n *store.Node, wsID, mName, appData string, children []*store.Node) viewWorkstream {
	role := n.Role
	if role == "" {
		role = "worker"
	}
	state := workstreamState(t, n)
	blockedOn := resolveBlockedOn(t, n, children)
	parentMember := MemberName(n.ParentID)
	modelTag := viewModelTag(n)
	owns := "-"
	if len(n.Owns) > 0 {
		owns = shortTitle(strings.ReplaceAll(strings.Join(n.Owns, ","), "|", "/"), 60)
	}
	summary := cleanNoteSegment(nodeTaskSummary(t, n, role))
	waitSeg := ""
	if blockedOn != "" {
		waitSeg = " | wait:" + cleanNoteSegment(blockedOn)
	}
	note := shortTitle(fmt.Sprintf("parent:%s | depth:%d | model:%s | owns:%s%s | %s",
		parentMember, n.Depth, cleanNoteSegment(modelTag), owns, waitSeg, summary), 280)

	return viewWorkstream{
		ID:        wsID,
		Title:     shortTitle(fmt.Sprintf("[L%d] %s (%s)", n.Depth, n.ID, role), 150),
		Owners:    []string{mName},
		State:     state,
		BlockedOn: blockedOn,
		Note:      note,
		Refs:      []viewRef{{Member: mName}},
		Actions:   nodeActions(t, n, mName, appData),
	}
}

func buildThreads(t *store.Tree, nodes []*store.Node, nodeWSID map[string]string) []viewThread {
	type branchAgg struct {
		topID   string
		maxD    int
		wsIDs   []string
		working int
		queued  int
		blocked int
		done    int
		nowTxt  string
	}
	aggs := map[string]*branchAgg{}
	order := []string{}
	for _, n := range nodes {
		top := topBranchOf(t, n)
		ag, ok := aggs[top]
		if !ok {
			ag = &branchAgg{topID: top, maxD: n.Depth, wsIDs: []string{}}
			aggs[top] = ag
			order = append(order, top)
		}
		if n.Depth > ag.maxD {
			ag.maxD = n.Depth
		}
		if wsID, ok := nodeWSID[n.ID]; ok {
			ag.wsIDs = append(ag.wsIDs, wsID)
		}
		switch workstreamState(t, n) {
		case "active":
			ag.working++
			if ag.nowTxt == "" {
				ag.nowTxt = nodeNowSummary(t, n)
			}
		case "queued":
			ag.queued++
		case "blocked", "failed":
			ag.blocked++
		default:
			ag.done++
		}
	}

	usedThreadIDs := map[string]int{}
	out := make([]viewThread, 0, minInt(len(order), maxViewThreads))
	for _, topID := range order {
		if len(out) >= maxViewThreads {
			break
		}
		ag := aggs[topID]
		title := MemberName(topID)
		if ag.maxD >= 2 {
			title = fmt.Sprintf("%s (L1..L%d)", MemberName(topID), ag.maxD)
		}
		st := "done"
		if ag.working > 0 {
			st = "active"
		} else if ag.blocked > 0 {
			st = "blocked"
		} else if ag.queued > 0 {
			st = "queued"
		}
		nowStr := ag.nowTxt
		if nowStr == "" {
			nowStr = fmt.Sprintf("%d active, %d done, %d issue", ag.working, ag.done, ag.blocked)
		}
		out = append(out, viewThread{
			ID:          safeViewID("t-", MemberName(topID), usedThreadIDs),
			Title:       shortTitle(title, 38),
			Goal:        shortTitle(branchTopic(t, topID), 96),
			Now:         shortTitle(nowStr, 76),
			State:       st,
			Workstreams: ag.wsIDs,
		})
	}
	return out
}

func buildAttention(t *store.Tree, nodes []*store.Node) []viewAttention {
	used := map[string]int{}
	seenNode := map[string]bool{}
	var out []viewAttention
	for _, n := range nodes {
		if len(out) >= maxViewAttention {
			break
		}
		failedTask := nodeFailedTask(t, n)
		if n.State != store.NodeError && n.State != store.NodeBlocked && failedTask == nil {
			continue
		}
		seenNode[n.ID] = true
		mName := MemberName(n.ID)
		kind, sev, why := "error", "high", strings.TrimSpace(n.LastError)
		if n.State == store.NodeBlocked {
			kind, sev = "stuck", "med"
			if strings.Contains(strings.ToLower(why), "ask") || strings.Contains(strings.ToLower(why), "decision") {
				kind = "decision"
			}
			if why == "" {
				why = "Node is blocked waiting on dependency or decision"
			}
		} else if failedTask != nil && n.State != store.NodeError {
			kind, sev = "failure", "high"
			if why == "" {
				why = strings.TrimSpace(failedTask.Summary)
			}
		}
		if why == "" {
			why = fmt.Sprintf("Node %s reported %s", n.ID, n.State)
		}
		acts := attentionActions(t, n, mName)
		out = append(out, viewAttention{
			ID:       safeViewID("att-", mName, used),
			Kind:     kind,
			Severity: sev,
			Title:    shortTitle(fmt.Sprintf("%s (%s)", n.ID, kind), 120),
			Why:      shortTitle(why, 240),
			Refs:     []viewRef{{Member: mName}},
			Actions:  acts,
		})
	}
	// Also surface failed tasks whose assignee may have been archived.
	taskIDs := make([]string, 0, len(t.Tasks))
	for id := range t.Tasks {
		taskIDs = append(taskIDs, id)
	}
	sort.Strings(taskIDs)
	for _, id := range taskIDs {
		if len(out) >= maxViewAttention {
			break
		}
		tk := t.Tasks[id]
		if tk == nil || tk.State != store.TaskFailed || seenNode[tk.AssigneeID] {
			continue
		}
		mName := MemberName(tk.AssigneeID)
		why := strings.TrimSpace(tk.Summary)
		if why == "" {
			why = tk.Goal
		}
		var acts []viewAction
		if t.MirrorSeq > 0 {
			acts = append(acts, viewAction{Type: "board_post", Target: t.MirrorSeq, Label: "Board"})
		}
		out = append(out, viewAttention{
			ID:       safeViewID("att-", tk.ID, used),
			Kind:     "failure",
			Severity: "high",
			Title:    shortTitle(fmt.Sprintf("%s failed (%s)", mName, tk.ID), 120),
			Why:      shortTitle(why, 240),
			Refs:     []viewRef{{Member: mName}},
			Actions:  acts,
		})
	}
	return out
}

func buildMilestones(t *store.Tree) []viewMilestone {
	var doneTasks []*store.Task
	for _, tk := range t.Tasks {
		if tk != nil && tk.State == store.TaskDone {
			doneTasks = append(doneTasks, tk)
		}
	}
	sort.SliceStable(doneTasks, func(i, j int) bool {
		if !doneTasks[i].UpdatedAt.Equal(doneTasks[j].UpdatedAt) {
			return doneTasks[i].UpdatedAt.After(doneTasks[j].UpdatedAt)
		}
		return doneTasks[i].ID < doneTasks[j].ID
	})
	used := map[string]int{}
	out := make([]viewMilestone, 0, minInt(len(doneTasks), maxViewMilestones))
	for _, tk := range doneTasks {
		if len(out) >= maxViewMilestones {
			break
		}
		txt := strings.TrimSpace(tk.Summary)
		if txt == "" {
			txt = tk.Goal
		}
		mName := MemberName(tk.AssigneeID)
		out = append(out, viewMilestone{
			ID:   safeViewID("ms-", tk.ID, used),
			Text: shortTitle(fmt.Sprintf("[%s] %s", mName, txt), 160),
			Refs: []viewRef{{Member: mName}},
		})
	}
	return out
}

func buildRisks(nodes []*store.Node) []viewRisk {
	used := map[string]int{}
	var out []viewRisk
	for _, n := range nodes {
		if len(out) >= maxViewRisks {
			break
		}
		if n.FailoverCount <= 0 && len(n.FailedTargets) == 0 {
			continue
		}
		mName := MemberName(n.ID)
		txt := fmt.Sprintf("%s: %d failover(s)", n.ID, n.FailoverCount)
		if len(n.FailedTargets) > 0 {
			txt += " (" + strings.Join(n.FailedTargets, ", ") + ")"
		}
		out = append(out, viewRisk{
			ID:   safeViewID("risk-", mName, used),
			Text: shortTitle(txt, 160),
			Refs: []viewRef{{Member: mName}},
		})
	}
	return out
}

func orderedNonRootNodes(t *store.Tree) []*store.Node {
	if t == nil {
		return nil
	}
	children := map[string][]*store.Node{}
	for _, n := range t.Nodes {
		if n == nil || n.IsRoot() {
			continue
		}
		children[n.ParentID] = append(children[n.ParentID], n)
	}
	for p := range children {
		sort.Slice(children[p], func(i, j int) bool { return children[p][i].ID < children[p][j].ID })
	}

	var out []*store.Node
	visited := map[string]bool{}
	var walk func(parentID string)
	walk = func(parentID string) {
		for _, ch := range children[parentID] {
			if visited[ch.ID] {
				continue
			}
			visited[ch.ID] = true
			out = append(out, ch)
			walk(ch.ID)
		}
	}
	walk("root")

	// Append any detached non-root nodes deterministically so none are lost.
	var rest []*store.Node
	for _, n := range t.Nodes {
		if n != nil && !n.IsRoot() && !visited[n.ID] {
			rest = append(rest, n)
		}
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].Depth != rest[j].Depth {
			return rest[i].Depth < rest[j].Depth
		}
		return rest[i].ID < rest[j].ID
	})
	return append(out, rest...)
}

func workstreamState(t *store.Tree, n *store.Node) string {
	switch n.State {
	case store.NodeWorking:
		return "active"
	case store.NodeQueued:
		return "queued"
	case store.NodeBlocked:
		return "blocked"
	case store.NodeError:
		return "failed"
	case store.NodeRetired, store.NodeCanceled:
		return "done"
	default:
		if nodeHasFailedTask(t, n) {
			return "failed"
		}
		return "done"
	}
}

func nodeFailedTask(t *store.Tree, n *store.Node) *store.Task {
	if t == nil || n == nil || n.CurrentTaskID == "" {
		return nil
	}
	if tk, ok := t.Tasks[n.CurrentTaskID]; ok && tk.State == store.TaskFailed {
		return tk
	}
	return nil
}

func nodeHasFailedTask(t *store.Tree, n *store.Node) bool {
	return nodeFailedTask(t, n) != nil
}

func resolveBlockedOn(t *store.Tree, n *store.Node, children []*store.Node) string {
	if n.CurrentTaskID != "" {
		if tk, ok := t.Tasks[n.CurrentTaskID]; ok {
			for _, depID := range tk.DependsOn {
				if dep, dok := t.Tasks[depID]; dok && dep.State != store.TaskDone && dep.AssigneeID != "" && dep.AssigneeID != n.ID {
					return MemberName(dep.AssigneeID)
				}
			}
		}
	}
	if n.State == store.NodeIdle || n.State == store.NodeBlocked || n.State == store.NodeQueued {
		for _, ch := range children {
			if ch.State == store.NodeWorking {
				return MemberName(ch.ID)
			}
		}
		for _, ch := range children {
			if ch.State == store.NodeQueued || ch.State == store.NodeBlocked {
				return MemberName(ch.ID)
			}
		}
	}
	return ""
}

func viewModelTag(n *store.Node) string {
	if strings.TrimSpace(n.Provider) != "" {
		if strings.TrimSpace(n.ProviderModel) != "" {
			return "ext:" + strings.TrimSpace(n.Provider) + "/" + strings.TrimSpace(n.ProviderModel)
		}
		return "ext:" + strings.TrimSpace(n.Provider)
	}
	if strings.TrimSpace(n.Model) != "" {
		return "official/" + strings.TrimSpace(n.Model)
	}
	return "official"
}

func nodeTaskSummary(t *store.Tree, n *store.Node, fallback string) string {
	if n.CurrentTaskID != "" {
		if tk, ok := t.Tasks[n.CurrentTaskID]; ok {
			if (tk.State == store.TaskDone || tk.State == store.TaskFailed) && strings.TrimSpace(tk.Summary) != "" {
				return tk.Summary
			}
			if strings.TrimSpace(tk.Goal) != "" {
				return tk.Goal
			}
		}
	}
	if strings.TrimSpace(n.LastError) != "" {
		return n.LastError
	}
	return fallback
}

func nodeNowSummary(t *store.Tree, n *store.Node) string {
	if n.State == store.NodeError && strings.TrimSpace(n.LastError) != "" {
		return "err: " + n.LastError
	}
	if n.CurrentTaskID != "" {
		if tk, ok := t.Tasks[n.CurrentTaskID]; ok && strings.TrimSpace(tk.Goal) != "" {
			return tk.Goal
		}
	}
	return string(n.State)
}

func workingLabel(t *store.Tree, n *store.Node) string {
	lbl := MemberName(n.ID)
	if n.CurrentTaskID != "" {
		if tk, ok := t.Tasks[n.CurrentTaskID]; ok && tk.Goal != "" {
			return fmt.Sprintf("%s (%s)", lbl, shortTitle(tk.Goal, 32))
		}
	}
	return lbl
}

func nodeActions(t *store.Tree, n *store.Node, mName, appData string) []viewAction {
	var acts []viewAction
	if strings.TrimSpace(n.Provider) == "" && strings.TrimSpace(n.ConversationID) != "" {
		acts = append(acts, viewAction{Type: "open_conv", Target: mName, Label: "Open chat"})
	}
	var cands []string
	if n.CurrentTaskID != "" {
		if tk, ok := t.Tasks[n.CurrentTaskID]; ok {
			cands = append(cands, tk.Artifact, tk.SpecFile)
		}
	}
	cands = append(cands, n.WorkerLog)
	seenFile := map[string]bool{}
	for _, raw := range cands {
		if len(acts) >= 3 {
			break
		}
		if cleanPath, ok := safeFileActionPath(appData, raw); ok && !seenFile[cleanPath] {
			seenFile[cleanPath] = true
			acts = append(acts, viewAction{Type: "file", Target: cleanPath, Label: "Open artifact"})
		}
	}
	if len(acts) < 4 && t.MirrorSeq > 0 {
		acts = append(acts, viewAction{Type: "board_post", Target: t.MirrorSeq, Label: "Board"})
	}
	return acts
}

func attentionActions(t *store.Tree, n *store.Node, mName string) []viewAction {
	var acts []viewAction
	if strings.TrimSpace(n.Provider) == "" && strings.TrimSpace(n.ConversationID) != "" {
		acts = append(acts, viewAction{Type: "open_conv", Target: mName, Label: "Open chat"})
	}
	if t.MirrorSeq > 0 {
		acts = append(acts, viewAction{Type: "board_post", Target: t.MirrorSeq, Label: "Board"})
	}
	return acts
}

func safeFileActionPath(appDataDir, raw string) (string, bool) {
	p := strings.TrimSpace(raw)
	if p == "" || strings.Contains(p, "%") || strings.Contains(p, "\\") || strings.Contains(p, "..") {
		return "", false
	}
	if !strings.HasPrefix(p, "/") || !filepath.IsAbs(p) {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	appClean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(appDataDir)))
	if appClean == "" || appClean == "." {
		return "", false
	}
	brainPrefix := appClean + "/brain/"
	if !strings.HasPrefix(p, brainPrefix) || len(p) <= len(brainPrefix) {
		return "", false
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return p, true
}

func safeViewID(prefix, raw string, used map[string]int) string {
	s := strings.ToLower(strings.TrimSpace(raw))
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
	body := strings.Trim(b.String(), "-")
	if body == "" {
		body = "item"
	}
	base := prefix + body
	if len(base) > 40 {
		base = strings.TrimRight(base[:40], "-")
	}
	if base == "" {
		base = prefix + "item"
	}
	if used == nil {
		return base
	}
	if used[base] == 0 {
		used[base] = 1
		return base
	}
	n := used[base] + 1
	used[base] = n
	suf := fmt.Sprintf("-%d", n)
	maxBase := 40 - len(suf)
	if maxBase < 1 {
		maxBase = 1
	}
	trimmed := base
	if len(trimmed) > maxBase {
		trimmed = strings.TrimRight(trimmed[:maxBase], "-")
	}
	cand := trimmed + suf
	used[cand] = 1
	return cand
}

func cleanNoteSegment(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "/")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
