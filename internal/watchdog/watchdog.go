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

type Config struct {
	TreeID       string
	AppDataDir   string
	LSAddress    string
	PollInterval time.Duration
	StallTimeout time.Duration
	IdleExit     time.Duration
}

type Watchdog struct {
	Store            *store.Store
	API              agentapi.Transport
	Orch             *orchestrator.Orchestrator
	ProviderReloader *provider.Reloader
	Cfg              Config
	Now              func() time.Time
}

func New(st *store.Store, api agentapi.Transport, cfg Config) *Watchdog {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = 10 * time.Minute
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
		Store: st,
		API:   api,
		Orch:  orchestrator.New(st, api, nil),
		Cfg:   cfg,
		Now:   time.Now,
	}
}

type TickReport struct {
	HasActiveWork bool
	AlertsSent    int
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
	}
	var alerts []pendingAlert

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
						alerts = append(alerts, pendingAlert{
							nodeID:     n.ID,
							taskID:     n.CurrentTaskID,
							parentID:   parent.ID,
							parentConv: parent.ConversationID,
							alertKey:   key,
							newState:   store.NodeBlocked,
							summary:    fmt.Sprintf("Gateway node %q (%s) stalled for >%dm with no log activity (task %s).", n.ID, n.Model, int(w.Cfg.StallTimeout.Minutes()), n.CurrentTaskID),
						})
					}
				}
			}
			continue
		}
		if n.IsRoot() || n.ConversationID == "" || appData == "" {
			continue
		}
		trPath := filepath.Join(appData, "brain", n.ConversationID, ".system_generated", "logs", "transcript.jsonl")
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
							summary:    fmt.Sprintf("Node %q has no transcript activity for >%dm (task %s).", n.ID, int(w.Cfg.StallTimeout.Minutes()), n.CurrentTaskID),
						})
					}
				}
			}
			continue
		}

		lastStep := readLastTranscriptStep(trPath)
		if lastStep != nil {
			if kind, errTxt := classifyStepFailure(lastStep, stat.ModTime(), now); kind != "" {
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
		}

		// Stall check when transcript stopped moving for StallTimeout.
		lastActivity := stat.ModTime()
		if n.UpdatedAt.After(lastActivity) {
			lastActivity = n.UpdatedAt
		}
		if now.Sub(lastActivity) > w.Cfg.StallTimeout {
			parent := t.Nodes[n.ParentID]
			if parent != nil {
				key := fmt.Sprintf("stall:%d:%s", stat.Size(), n.CurrentTaskID)
				if n.LastAlertKey != key {
					alerts = append(alerts, pendingAlert{
						nodeID:     n.ID,
						taskID:     n.CurrentTaskID,
						parentID:   parent.ID,
						parentConv: parent.ConversationID,
						alertKey:   key,
						newState:   store.NodeBlocked,
						summary:    fmt.Sprintf("Node %q stalled for >%dm with no progress (task %s).", n.ID, int(w.Cfg.StallTimeout.Minutes()), n.CurrentTaskID),
					})
				}
			}
		}
	}

	for _, al := range alerts {
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
			if n.State == store.NodeQueued && al.newState == store.NodeError {
				if tk, ok := tr.Tasks[al.taskID]; ok && tk.State == store.TaskQueued {
					tk.State = store.TaskFailed
					tk.Summary = al.summary
					tk.UpdatedAt = now.UTC()
				}
				n.CurrentTaskID = ""
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
	return rep, nil
}

type transcriptStep struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func readLastTranscriptStep(path string) *transcriptStep {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil
	}
	const tailWindow = 64 * 1024
	start := fi.Size() - tailWindow
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" {
			continue
		}
		var st transcriptStep
		if err := json.Unmarshal([]byte(ln), &st); err == nil && st.Type != "" {
			return &st
		}
	}
	return nil
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
