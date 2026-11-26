package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/TangYani2024/bswarm/internal/agentapi"
	"github.com/TangYani2024/bswarm/internal/gateway"
	"github.com/TangYani2024/bswarm/internal/orchestrator"
	"github.com/TangYani2024/bswarm/internal/provider"
	"github.com/TangYani2024/bswarm/internal/store"
	"github.com/TangYani2024/bswarm/internal/uimirror"
)

var quotaRe = regexp.MustCompile(`(?i)RESOURCE_EXHAUSTED|quota|\b429\b|exhausted your capacity|rate.?limit`)

const DefaultStallTimeout = 5 * time.Minute

type Config struct {
	TreeID       string
	AppDataDir   string
	LSAddress    string
	PollInterval time.Duration
	StallTimeout time.Duration
	NudgeTimeout time.Duration // 0 => StallTimeout/2; <0 => disable mid-stall nudges
	IdleExit     time.Duration
}

type Watchdog struct {
	Store            *store.Store
	API              agentapi.Transport
	Orch             *orchestrator.Orchestrator
	ProviderReloader *provider.Reloader
	KillWorker       func(pid int)
	Cfg              Config
	Now              func() time.Time
}

func defaultKillWorker(pid int) {
	if pid <= 0 {
		return
	}
	_ = gateway.KillProcessGroup(pid)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !gateway.ProcessAlive(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	if gateway.ProcessAlive(pid) {
		_ = gateway.ForceKillProcessGroup(pid)
	}
}

func resolveDefaultStallTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("BSWARM_STALL_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return DefaultStallTimeout
}

func New(st *store.Store, api agentapi.Transport, cfg Config) *Watchdog {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = resolveDefaultStallTimeout()
	}
	if cfg.NudgeTimeout == 0 && cfg.StallTimeout >= 2*time.Minute {
		cfg.NudgeTimeout = cfg.StallTimeout / 2
	}
	if cfg.IdleExit <= 0 {
		cfg.IdleExit = 3 * time.Minute
	}
	if cfg.AppDataDir == "" {
		cfg.AppDataDir = strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR"))
	}
	if cfg.LSAddress == "" {
		cfg.LSAddress = strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
	}
	return &Watchdog{
		Store:      st,
		API:        api,
		Orch:       orchestrator.New(st, api, nil),
		KillWorker: defaultKillWorker,
		Cfg:        cfg,
		Now:        time.Now,
	}
}

type TickReport struct {
	HasActiveWork bool
	AlertsSent    int
	NudgesSent    int
	KilledWorkers int
	RelayedMails  int
	Promoted      int
}

// Run executes the zero-token watchdog loop under a per-tree single-instance lock until
// the tree becomes idle for IdleExit, the parent LS port closes, or ctx is canceled.
func (w *Watchdog) Run(ctx context.Context) error {
	release, acquired, err := w.Store.TryWatchLock(w.Cfg.TreeID)
	if err != nil {
		return err
	}
	if !acquired {
		// Another watchdog instance is already running for this tree.
		return nil
	}
	defer release()

	var idleSince time.Time
	ticker := time.NewTicker(w.Cfg.PollInterval)
	defer ticker.Stop()

	for {
		rep, err := w.Tick(ctx)
		if err != nil {
			return err
		}
		if rep.HasActiveWork {
			idleSince = time.Time{}
		} else {
			if idleSince.IsZero() {
				idleSince = w.Now()
			} else if w.Now().Sub(idleSince) >= w.Cfg.IdleExit {
				return nil
			}
		}
		if w.Cfg.LSAddress != "" && !lsPortReachable(w.Cfg.LSAddress) {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Tick runs one zero-token inspection pass over active nodes' transcript.jsonl tails.
func (w *Watchdog) Tick(ctx context.Context) (TickReport, error) {
	var rep TickReport
	t, err := w.Store.LoadTree(w.Cfg.TreeID)
	if err != nil {
		return rep, err
	}
	appData := w.Cfg.AppDataDir
	if appData == "" {
		appData = t.AppDataDir
	}
	if appData == "" {
		rootConv := ""
		if root := t.Nodes["root"]; root != nil {
			rootConv = root.ConversationID
		}
		appData = store.ProbeAppDataDir(rootConv)
	}

	// Outbox Relay: deliver any inbox wakeups written by sandboxed children whose direct
	// `agentapi send-message` was blocked by network isolation (0 permission prompts!).
	for _, n := range t.Nodes {
		if n.Provider != "" || n.ConversationID == "" || n.InboxTotalSeq <= n.InboxDeliveredSeq {
			continue
		}
		pending, rerr := w.Store.ReadInbox(t.ID, n.ID, n.InboxDeliveredSeq)
		if rerr != nil || len(pending) == 0 {
			continue
		}
		deliveredUpTo := n.InboxDeliveredSeq
		for _, m := range pending {
			title := fmt.Sprintf("[bswarm] %s from %s", m.Kind, m.From)
			if serr := w.API.SendMessage(ctx, n.ConversationID, title, orchestrator.BuildMailWakeNotice(m)); serr != nil {
				break
			}
			deliveredUpTo = m.Seq
			rep.RelayedMails++
		}
		if deliveredUpTo > n.InboxDeliveredSeq {
			targetID := n.ID
			_, _ = w.Store.UpdateTree(w.Cfg.TreeID, func(tr *store.Tree) error {
				if node, ok := tr.Nodes[targetID]; ok && deliveredUpTo > node.InboxDeliveredSeq {
					node.InboxDeliveredSeq = deliveredUpTo
				}
				return nil
			})
		}
	}

	// Hot-reload external providers.json on every tick (zero-restart config updates!).
	if w.ProviderReloader != nil && w.Orch != nil {
		if cfg, cfgPath, _, rerr := w.ProviderReloader.Current(); rerr == nil && cfg != nil {
			w.Orch.Providers = cfg
			w.Orch.ProvidersFile = cfgPath
		}
	}

	// Promote any DAG or concurrency-queued tasks that became ready.
	if promoted, _ := w.Orch.PromoteReadyTasks(ctx, w.Cfg.TreeID); len(promoted) > 0 {
		rep.Promoted = len(promoted)
		t, err = w.Store.LoadTree(w.Cfg.TreeID)
		if err != nil {
			return rep, err
		}
	}

	now := w.Now()
	type pendingAlert struct {
		nodeID     string
		taskID     string
		parentID   string
		parentConv string
		alertKey   string
		newState   store.NodeState
		summary    string
		killPID    int
		failTask   bool
	}
	type pendingNudge struct {
		nodeID   string
		taskID   string
		nodeConv string
		nudgeKey string
		message  string
	}
	var alerts []pendingAlert
	var nudges []pendingNudge

	for _, n := range t.Nodes {
		if n.State == store.NodeQueued {
			// Check for failed/canceled DAG dependency so queued tasks never spin watchdog forever.
			if tk, ok := t.Tasks[n.CurrentTaskID]; ok {
				if depID, depState, bad := store.FailedDependency(t, tk.DependsOn); bad {
					parent := t.Nodes[n.ParentID]
					if parent != nil {
						key := fmt.Sprintf("dag-deadlock:%s:%s", n.CurrentTaskID, depID)
						if n.LastAlertKey != key {
							alerts = append(alerts, pendingAlert{
								nodeID:     n.ID,
								taskID:     n.CurrentTaskID,
								parentID:   parent.ID,
								parentConv: parent.ConversationID,
								alertKey:   key,
								newState:   store.NodeError,
								summary:    fmt.Sprintf("Queued node %q aborted: dependency %s %s.", n.ID, depID, depState),
							})
						}
					}
					continue
				}
			}
			rep.HasActiveWork = true
			continue
		}
		if n.State != store.NodeWorking {
			continue
		}
		if !n.IsRoot() {
			rep.HasActiveWork = true
		}
		// External gateway worker monitoring: check WorkerPID liveness & WorkerLog stall.
		if n.Provider != "" {
			if n.WorkerPID > 0 && !gateway.ProcessAlive(n.WorkerPID) && now.Sub(n.UpdatedAt) > 15*time.Second {
				parent := t.Nodes[n.ParentID]
				if parent != nil {
					key := fmt.Sprintf("gw-dead:%d:%s", n.WorkerPID, n.CurrentTaskID)
					if n.LastAlertKey != key {
						alerts = append(alerts, pendingAlert{
							nodeID:     n.ID,
							taskID:     n.CurrentTaskID,
							parentID:   parent.ID,
							parentConv: parent.ConversationID,
							alertKey:   key,
							newState:   store.NodeError,
							failTask:   true,
							summary:    fmt.Sprintf("Gateway worker %q (pid=%d, model=%s) exited unexpectedly (task %s).", n.ID, n.WorkerPID, n.Model, n.CurrentTaskID),
						})
					}
				}
				continue
			}
			lastActivity := n.UpdatedAt
			if n.WorkerLog != "" {
				if st, serr := os.Stat(n.WorkerLog); serr == nil && st.ModTime().After(lastActivity) {
					lastActivity = st.ModTime()
				}
			}
			if now.Sub(lastActivity) > w.Cfg.StallTimeout {
				parent := t.Nodes[n.ParentID]
				if parent != nil {
					key := fmt.Sprintf("gw-stall:%s:%d", n.CurrentTaskID, statMtimeBucket(lastActivity))
					if n.LastAlertKey != key {
						summary := fmt.Sprintf("Gateway node %q (%s) stalled for >%s with no log activity (task %s).",
							n.ID, n.Model, formatShortDuration(w.Cfg.StallTimeout), n.CurrentTaskID)
						newState := store.NodeBlocked
						killPID := 0
						failTask := false
						if n.WorkerPID > 0 {
							killPID = n.WorkerPID
							newState = store.NodeError
							failTask = true
							summary = fmt.Sprintf("Gateway node %q (%s) stalled for >%s with no log activity (task %s); terminated hung worker process group pid=%d and released concurrency slot.",
								n.ID, n.Model, formatShortDuration(w.Cfg.StallTimeout), n.CurrentTaskID, n.WorkerPID)
						}
						alerts = append(alerts, pendingAlert{
							nodeID:     n.ID,
							taskID:     n.CurrentTaskID,
							parentID:   parent.ID,
							parentConv: parent.ConversationID,
							alertKey:   key,
							newState:   newState,
							killPID:    killPID,
							failTask:   failTask,
							summary:    summary,
						})
					}
				}
			}
			continue
		}
		if n.IsRoot() || n.ConversationID == "" {
			continue
		}
		nodeAppData := appData
		if nodeAppData == "" {
			nodeAppData = store.ProbeAppDataDir(n.ConversationID)
		}
		if nodeAppData == "" {
			continue
		}
		trPath := filepath.Join(nodeAppData, "brain", n.ConversationID, ".system_generated", "logs", "transcript.jsonl")
		stat, serr := os.Stat(trPath)
		if serr != nil {
			// Freshly spawned conversation might not have created transcript yet; stall check uses UpdatedAt.
			if now.Sub(n.UpdatedAt) > w.Cfg.StallTimeout {
				parent := t.Nodes[n.ParentID]
				if parent != nil {
					key := fmt.Sprintf("stall-notr:%s:%d", n.CurrentTaskID, statMtimeBucket(n.UpdatedAt))
					if n.LastAlertKey != key {
						alerts = append(alerts, pendingAlert{
							nodeID:     n.ID,
							taskID:     n.CurrentTaskID,
							parentID:   parent.ID,
							parentConv: parent.ConversationID,
							alertKey:   key,
							newState:   store.NodeBlocked,
							summary:    fmt.Sprintf("Node %q has no transcript activity for >%s (task %s).", n.ID, formatShortDuration(w.Cfg.StallTimeout), n.CurrentTaskID),
						})
					}
				}
			}
			continue
		}

		tail := readTranscriptTail(trPath)
		if tail.Last != nil {
			if kind, errTxt := classifyStepFailure(tail.Last, stat.ModTime(), now); kind != "" {
				parent := t.Nodes[n.ParentID]
				if parent != nil {
					key := fmt.Sprintf("%s:%d:%s", kind, stat.Size(), n.CurrentTaskID)
					if n.LastAlertKey != key {
						errShort, _ := store.TruncateRunes(errTxt, 200)
						alerts = append(alerts, pendingAlert{
							nodeID:     n.ID,
							taskID:     n.CurrentTaskID,
							parentID:   parent.ID,
							parentConv: parent.ConversationID,
							alertKey:   key,
							newState:   store.NodeError,
							summary:    fmt.Sprintf("Node %q hit %s error (task %s): %s", n.ID, kind, n.CurrentTaskID, errShort),
						})
					}
				}
				continue
			}
			// Early-detect repeated hook-deny or consecutive tool error loops before waiting for full StallTimeout.
			if (tail.HookDenyStreak >= 2 || tail.ConsecutiveErrors >= 3) && now.Sub(stat.ModTime()) >= 15*time.Second {
				parent := t.Nodes[n.ParentID]
				if parent != nil {
					key := fmt.Sprintf("err-loop:%d:%s", stat.Size(), n.CurrentTaskID)
					if n.LastAlertKey != key {
						reason := fmt.Sprintf("%d consecutive step errors", tail.ConsecutiveErrors)
						if tail.HookDenyStreak >= 2 {
							reason = fmt.Sprintf("%d consecutive pre-tool hook denials", tail.HookDenyStreak)
						}
						summary := fmt.Sprintf("Node %q stuck in %s (task %s).", n.ID, reason, n.CurrentTaskID)
						if tail.Hint != "" {
							summary = fmt.Sprintf("Node %q stuck in %s (task %s; %s).", n.ID, reason, n.CurrentTaskID, tail.Hint)
						}
						alerts = append(alerts, pendingAlert{
							nodeID:     n.ID,
							taskID:     n.CurrentTaskID,
							parentID:   parent.ID,
							parentConv: parent.ConversationID,
							alertKey:   key,
							newState:   store.NodeBlocked,
							summary:    summary,
						})
					}
				}
				continue
			}
		}

		// Stall & mid-stall nudge checks based on transcript activity.
		lastActivity := stat.ModTime()
		if n.UpdatedAt.After(lastActivity) {
			lastActivity = n.UpdatedAt
		}
		idleFor := now.Sub(lastActivity)
		if idleFor > w.Cfg.StallTimeout {
			parent := t.Nodes[n.ParentID]
			if parent != nil {
				key := fmt.Sprintf("stall:%d:%s", stat.Size(), n.CurrentTaskID)
				if n.LastAlertKey != key {
					summary := fmt.Sprintf("Node %q stalled for >%s with no progress (task %s).",
						n.ID, formatShortDuration(w.Cfg.StallTimeout), n.CurrentTaskID)
					if tail.Hint != "" {
						summary = fmt.Sprintf("Node %q stalled for >%s with no progress (task %s; %s).",
							n.ID, formatShortDuration(w.Cfg.StallTimeout), n.CurrentTaskID, tail.Hint)
					}
					alerts = append(alerts, pendingAlert{
						nodeID:     n.ID,
						taskID:     n.CurrentTaskID,
						parentID:   parent.ID,
						parentConv: parent.ConversationID,
						alertKey:   key,
						newState:   store.NodeBlocked,
						summary:    summary,
					})
				}
			}
		} else if w.Cfg.NudgeTimeout > 0 && idleFor > w.Cfg.NudgeTimeout {
			nudgeKey := fmt.Sprintf("nudge:%d:%s", stat.Size(), n.CurrentTaskID)
			if n.LastNudgeKey != nudgeKey {
				msg := fmt.Sprintf("[bswarm watchdog] No transcript progress for >%s on task %s. If a heavy command is hung, stop it and switch to a lighter or timeout-bounded approach; if finished, run `bswarm done --summary \"...\"` (write reports inside your artifacts dir).",
					formatShortDuration(w.Cfg.NudgeTimeout), n.CurrentTaskID)
				if tail.Last != nil && tail.Last.Type == "PLANNER_RESPONSE" && strings.EqualFold(tail.Last.Status, "DONE") {
					msg = fmt.Sprintf("[bswarm watchdog] Your last turn completed >%s ago without `bswarm done` (task %s). Please run `bswarm done --summary \"...\" [--artifact <path>]` now, or `bswarm ask parent` if blocked.",
						formatShortDuration(w.Cfg.NudgeTimeout), n.CurrentTaskID)
				}
				nudges = append(nudges, pendingNudge{
					nodeID:   n.ID,
					taskID:   n.CurrentTaskID,
					nodeConv: n.ConversationID,
					nudgeKey: nudgeKey,
					message:  msg,
				})
			}
		}
	}

	// Send mid-stall nudges directly to idle/hung workers without resetting their stall clock.
	for _, nd := range nudges {
		if nd.nodeConv == "" {
			continue
		}
		if serr := w.API.SendMessage(ctx, nd.nodeConv, "[bswarm] watchdog check-in", nd.message); serr == nil {
			rep.NudgesSent++
			_, _ = w.Store.UpdateTree(w.Cfg.TreeID, func(tr *store.Tree) error {
				if node, ok := tr.Nodes[nd.nodeID]; ok && node.State == store.NodeWorking && node.CurrentTaskID == nd.taskID {
					node.LastNudgeKey = nd.nudgeKey
				}
				return nil
			})
		}
	}

	freedSlot := false
	for _, al := range alerts {
		if al.killPID > 0 && w.KillWorker != nil {
			w.KillWorker(al.killPID)
			rep.KilledWorkers++
		}
		var mail store.Mail
		_, err := w.Store.UpdateTree(w.Cfg.TreeID, func(tr *store.Tree) error {
			n, ok := tr.Nodes[al.nodeID]
			if !ok || n.LastAlertKey == al.alertKey {
				return nil
			}
			// Guard against race where node finished `bswarm done` (now idle) while Tick inspected it.
			if (n.State != store.NodeWorking && n.State != store.NodeQueued) || n.CurrentTaskID != al.taskID {
				return nil
			}
			if (n.State == store.NodeQueued && al.newState == store.NodeError) || al.failTask {
				if tk, ok := tr.Tasks[al.taskID]; ok && (tk.State == store.TaskQueued || tk.State == store.TaskRunning) {
					tk.State = store.TaskFailed
					tk.Summary = al.summary
					tk.UpdatedAt = now.UTC()
				}
				n.CurrentTaskID = ""
				if n.Provider != "" {
					n.WorkerPID = 0
					freedSlot = true
				}
			}
			n.State = al.newState
			n.LastAlertKey = al.alertKey
			n.LastError = al.summary
			n.UpdatedAt = now.UTC()
			m, merr := w.Store.AppendMailUnlocked(tr, al.parentID, store.Mail{
				From:    "watchdog",
				Kind:    "alert",
				TaskID:  al.taskID,
				Summary: al.summary,
			})
			if merr != nil {
				return merr
			}
			mail = m
			return nil
		})
		if err == nil && mail.Seq > 0 && al.parentConv != "" {
			if serr := w.API.SendMessage(ctx, al.parentConv, "[bswarm] watchdog alert: "+al.nodeID, orchestrator.BuildMailWakeNotice(mail)); serr == nil {
				rep.AlertsSent++
				_, _ = w.Store.UpdateTree(w.Cfg.TreeID, func(tr *store.Tree) error {
					if p, ok := tr.Nodes[al.parentID]; ok && mail.Seq > p.InboxDeliveredSeq {
						p.InboxDeliveredSeq = mail.Seq
					}
					return nil
				})
			}
			if updated, uerr := w.Store.UpdateTree(w.Cfg.TreeID, func(tr *store.Tree) error {
				tr.MirrorSeq++
				return nil
			}); uerr == nil {
				_ = uimirror.AppendEvent(updated, updated.MirrorSeq, al.nodeID, al.parentID, "alert", mail.TaskID, al.summary, "")
				_ = uimirror.SyncRosterAndPlan(w.Store, updated)
			}
		}
	}
	if freedSlot && w.Orch != nil {
		if promoted, _ := w.Orch.PromoteReadyTasks(ctx, w.Cfg.TreeID); len(promoted) > 0 {
			rep.Promoted += len(promoted)
		}
	}
	return rep, nil
}

type transcriptStep struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Error   string `json:"error"`
	Content string `json:"content"`
}

type transcriptTail struct {
	Last              *transcriptStep
	ConsecutiveErrors int
	HookDenyStreak    int
	Hint              string
}

func readTranscriptTail(path string) transcriptTail {
	var res transcriptTail
	f, err := os.Open(path)
	if err != nil {
		return res
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return res
	}
	const tailWindow = 64 * 1024
	start := fi.Size() - tailWindow
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return res
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return res
	}
	lines := strings.Split(string(buf), "\n")
	var steps []transcriptStep
	for i := len(lines) - 1; i >= 0 && len(steps) < 8; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" {
			continue
		}
		var st transcriptStep
		if err := json.Unmarshal([]byte(ln), &st); err == nil && st.Type != "" {
			steps = append(steps, st)
		}
	}
	if len(steps) == 0 {
		return res
	}
	last := steps[0]
	res.Last = &last

	for _, st := range steps {
		if st.Type == "SYSTEM_MESSAGE" {
			continue
		}
		if strings.EqualFold(st.Status, "ERROR") || st.Type == "ERROR_MESSAGE" {
			res.ConsecutiveErrors++
			low := strings.ToLower(st.Error)
			if strings.Contains(low, "pre-tool hook") || strings.Contains(low, "[bswarm]") {
				res.HookDenyStreak++
			}
			continue
		}
		break
	}

	res.Hint = buildStepHint(&last)
	return res
}

func buildStepHint(st *transcriptStep) string {
	if st == nil {
		return ""
	}
	if e := collapseWS(st.Error); e != "" {
		short, _ := store.TruncateRunes(e, 140)
		return "last error: " + short
	}
	if st.Type == "PLANNER_RESPONSE" && strings.EqualFold(st.Status, "DONE") {
		return "last step: PLANNER_RESPONSE/DONE (may have ended turn without bswarm done)"
	}
	if st.Status != "" {
		return fmt.Sprintf("last step: %s/%s", st.Type, st.Status)
	}
	return "last step: " + st.Type
}

func collapseWS(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func formatShortDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.Round(time.Second).String()
}

func classifyStepFailure(st *transcriptStep, mtime, now time.Time) (string, string) {
	modelErr := st.Type == "ERROR_MESSAGE"
	settledToolErr := st.Status == "ERROR" && now.Sub(mtime) >= 60*time.Second
	if !modelErr && !settledToolErr {
		return "", ""
	}
	msg := strings.TrimSpace(st.Error)
	if msg == "" {
		if modelErr {
			msg = "model error"
		} else {
			msg = "tool step failed"
		}
	}
	if quotaRe.MatchString(msg) {
		return "quota", msg
	}
	return "error", msg
}

func statMtimeBucket(t time.Time) int64 {
	return t.Unix() / 600
}

func lsPortReachable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 1500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
