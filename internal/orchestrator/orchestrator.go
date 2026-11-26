package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/TangYani2024/bswarm/internal/agentapi"
	"github.com/TangYani2024/bswarm/internal/gateway"
	"github.com/TangYani2024/bswarm/internal/provider"
	"github.com/TangYani2024/bswarm/internal/store"
	"github.com/TangYani2024/bswarm/internal/uimirror"
	"github.com/TangYani2024/bswarm/internal/worktree"
)

type WatchStarterFunc func(treeID string) error
type VerifyRunnerFunc func(ctx context.Context, dir, cmdStr string) (string, error)
type GatewayLaunchFunc func(req gateway.LaunchRequest) error

type Orchestrator struct {
	Store         *store.Store
	API           agentapi.Transport
	WatchStarter  WatchStarterFunc
	RunVerify     VerifyRunnerFunc
	Providers     *provider.Config
	ProvidersFile string
	GatewayLaunch GatewayLaunchFunc
}

func New(st *store.Store, api agentapi.Transport, watchStarter WatchStarterFunc) *Orchestrator {
	if st == nil {
		st = store.New("")
	}
	if api == nil {
		api = agentapi.NewCLITransport()
	}
	return &Orchestrator{
		Store:        st,
		API:          api,
		WatchStarter: watchStarter,
		RunVerify:    defaultVerifyRunner,
		Providers:    &provider.Config{Providers: map[string]*provider.ProviderSpec{}, Pools: map[string][]string{}},
	}
}

func defaultVerifyRunner(ctx context.Context, dir, cmdStr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if len(text) > 1600 {
		cut := len(text) - 1500
		for cut < len(text) && (text[cut]&0xC0) == 0x80 {
			cut++
		}
		text = "...(truncated)\n" + text[cut:]
	}
	return text, err
}

type InitOpts struct {
	ConversationID string
	Goal           string
	RepoRoot       string
	AppDataDir     string
	MaxDepth       int
	MaxActive      int
}

func (o *Orchestrator) Init(opts InitOpts) (*store.Tree, bool, error) {
	convID := strings.TrimSpace(opts.ConversationID)
	if convID == "" {
		return nil, false, errors.New("ANTIGRAVITY_CONVERSATION_ID is required (or pass --conv)")
	}
	if ref, err := o.Store.ResolveConversation(convID); err == nil && ref != nil {
		if existing, err := o.Store.LoadTree(ref.TreeID); err == nil {
			return existing, false, nil
		}
	}
	repoRoot := strings.TrimSpace(opts.RepoRoot)
	if repoRoot == "" {
		repoRoot, _ = os.Getwd()
	}
	if gr := worktree.GitRoot(repoRoot); gr != "" {
		repoRoot = gr
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = store.DefaultMaxDepth
	}
	maxActive := opts.MaxActive
	if maxActive <= 0 {
		maxActive = store.DefaultMaxActive
	}
	appData := strings.TrimSpace(opts.AppDataDir)
	if appData == "" {
		appData = strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR"))
	}
	prefix := store.SafeFileKey(convID)
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	sum := sha256.Sum256([]byte(convID))
	treeID := prefix + "-" + hex.EncodeToString(sum[:3])
	now := time.Now().UTC()
	goal, _ := store.TruncateRunes(strings.TrimSpace(opts.Goal), store.MaxSummaryRunes)
	t := &store.Tree{
		Version:          1,
		ID:               treeID,
		Goal:             goal,
		RepoRoot:         repoRoot,
		AppDataDir:       appData,
		MaxDepth:         maxDepth,
		MaxActive:        maxActive,
		CompletedTaskIDs: map[string]bool{},
		Nodes: map[string]*store.Node{
			"root": {
				ID:             "root",
				Role:           "coordinator",
				ConversationID: convID,
				Depth:          0,
				State:          store.NodeWorking,
				WorkspaceMode:  store.WorkspaceShared,
				WorkspacePath:  repoRoot,
				CreatedAt:      now,
				UpdatedAt:      now,
			},
		},
		Tasks:     map[string]*store.Task{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := o.Store.CreateTree(t); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			if existing, lerr := o.Store.LoadTree(treeID); lerr == nil {
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	_ = uimirror.SyncRosterAndPlan(o.Store, t)
	return t, true, nil
}

// mirrorEvent appends a compact event to the official Swarm UI board.jsonl and refreshes members.json/plan.json.
func (o *Orchestrator) mirrorEvent(treeID, fromID, toID, kind, taskID, summary, artifact string) {
	t, err := o.Store.UpdateTree(treeID, func(tr *store.Tree) error {
		tr.MirrorSeq++
		return nil
	})
	if err != nil {
		return
	}
	_ = uimirror.AppendEvent(t, t.MirrorSeq, fromID, toID, kind, taskID, summary, artifact)
	_ = uimirror.SyncRosterAndPlan(o.Store, t)
}

func (o *Orchestrator) mirrorRoster(treeID string) {
	if t, err := o.Store.LoadTree(treeID); err == nil {
		_ = uimirror.SyncRosterAndPlan(o.Store, t)
	}
}

func (o *Orchestrator) markDelivered(treeID, nodeID string, seq int) {
	if nodeID == "" || seq <= 0 {
		return
	}
	_, _ = o.Store.UpdateTree(treeID, func(t *store.Tree) error {
		if n, ok := t.Nodes[nodeID]; ok && seq > n.InboxDeliveredSeq {
			n.InboxDeliveredSeq = seq
		}
		return nil
	})
}

func (o *Orchestrator) resolveActor(convID, treeOverride string) (*store.ConvRef, error) {
	if convID != "" {
		if ref, err := o.Store.ResolveConversation(convID); err == nil && ref != nil {
			return ref, nil
		}
	}
	envTree := strings.TrimSpace(os.Getenv("BSWARM_TREE_ID"))
	envNode := strings.TrimSpace(os.Getenv("BSWARM_NODE_ID"))
	if envTree != "" && envNode != "" && (treeOverride == "" || treeOverride == envTree) {
		return &store.ConvRef{TreeID: envTree, NodeID: envNode}, nil
	}
	if treeOverride != "" {
		return &store.ConvRef{TreeID: treeOverride, NodeID: "root"}, nil
	}
	return nil, errors.New("current conversation is not in any bswarm tree; run `bswarm init` first")
}

func (o *Orchestrator) providersCfg() *provider.Config {
	if o.Providers != nil {
		return o.Providers
	}
	return &provider.Config{Providers: map[string]*provider.ProviderSpec{}, Pools: map[string][]string{}}
}

type SpawnOpts struct {
	CallerConvID string
	TreeID       string
	ID           string
	Role         string
	Goal         string
	SpecFile     string
	Provider     string
	Model        string
	Profile      string
	Reuse        bool
	UseWorktree  bool
	Owns         []string
	DependsOn    []string
	VerifyCmd    string
	RepoRoot     string
}

type SpawnResult struct {
	TreeID         string          `json:"tree_id"`
	NodeID         string          `json:"node_id"`
	TaskID         string          `json:"task_id"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Provider       string          `json:"provider,omitempty"`
	Model          string          `json:"model,omitempty"`
	Pool           string          `json:"pool,omitempty"`
	ConcurrencyKey string          `json:"concurrency_key,omitempty"`
	Reused         bool            `json:"reused"`
	State          store.NodeState `json:"state"`
	WorkspacePath  string          `json:"workspace_path"`
	Branch         string          `json:"branch,omitempty"`
	SpecFile       string          `json:"spec_file,omitempty"`
}

func (o *Orchestrator) Spawn(ctx context.Context, opts SpawnOpts) (*SpawnResult, error) {
	if strings.TrimSpace(opts.Goal) == "" {
		return nil, errors.New("--goal is required")
	}
	ref, err := o.resolveActor(opts.CallerConvID, opts.TreeID)
	if err != nil {
		// Implicitly initialize root tree when a fresh conversation calls spawn directly.
		if opts.CallerConvID == "" {
			return nil, err
		}
		t, _, ierr := o.Init(InitOpts{
			ConversationID: opts.CallerConvID,
			Goal:           opts.Goal,
			RepoRoot:       opts.RepoRoot,
		})
		if ierr != nil {
			return nil, ierr
		}
		ref = &store.ConvRef{TreeID: t.ID, NodeID: "root"}
	}

	// Check reuse candidate first, validating workspace/model/provider/slot constraints and falling back to spawn on race.
	if opts.Reuse && strings.TrimSpace(opts.Role) != "" && len(opts.DependsOn) == 0 {
		wantWS := store.WorkspaceShared
		if opts.UseWorktree {
			wantWS = store.WorkspaceWorktree
		}
		wantProv, wantMod, wantPool, _ := o.providersCfg().ParseModelSpec(opts.Provider, opts.Model)
		if t, lerr := o.Store.LoadTree(ref.TreeID); lerr == nil {
			var candidates []*store.Node
			for _, n := range t.Nodes {
				if n.ParentID != ref.NodeID || n.Role != opts.Role || n.State != store.NodeIdle || n.ConversationID == "" {
					continue
				}
				if opts.ID != "" && n.ID != opts.ID && n.ID != ref.NodeID+"/"+opts.ID {
					continue
				}
				if n.WorkspaceMode != wantWS {
					continue
				}
				if wantPool != "" {
					if n.Pool != wantPool {
						continue
					}
				} else if wantProv != "" {
					if n.Provider != wantProv || (wantMod != "" && n.ProviderModel != wantMod) {
						continue
					}
				} else if opts.Model != "" && n.Model != opts.Model {
					continue
				}
				if n.Provider != "" {
					if _, avail, cerr := o.providersCfg().CanStartNode(n, t.Nodes); cerr != nil || !avail {
						continue
					}
				}
				candidates = append(candidates, n)
			}
			sort.Slice(candidates, func(i, j int) bool { return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt) })
			for _, cand := range candidates {
				ar, aerr := o.Assign(ctx, AssignOpts{
					CallerConvID: opts.CallerConvID,
					TreeID:       ref.TreeID,
					NodeID:       cand.ID,
					Goal:         opts.Goal,
					SpecFile:     opts.SpecFile,
					VerifyCmd:    opts.VerifyCmd,
					Owns:         opts.Owns,
				})
				if aerr == nil {
					return &SpawnResult{
						TreeID:         ref.TreeID,
						NodeID:         ar.NodeID,
						TaskID:         ar.TaskID,
						ConversationID: ar.ConversationID,
						Provider:       cand.Provider,
						Model:          cand.Model,
						Pool:           cand.Pool,
						ConcurrencyKey: cand.ConcurrencyKey,
						Reused:         true,
						State:          store.NodeWorking,
						WorkspacePath:  cand.WorkspacePath,
						Branch:         cand.Branch,
						SpecFile:       ar.SpecFile,
					}, nil
				}
			}
		}
	}

	rawID := strings.TrimSpace(opts.ID)
	role := strings.TrimSpace(opts.Role)
	if role == "" {
		role = rawID
	}
	if role == "" {
		role = "worker"
	}

	var (
		treeID       string
		nodeID       string
		taskID       string
		nodeCopy     *store.Node
		taskCopy     *store.Task
		treeCopy     *store.Tree
		startNow     bool
		resolved     provider.ResolvedTarget
		wtInfo       *worktree.Info
		createdWT    bool
		repoRoot     string
		wtRoot       string
		artifactsDir string
	)

	// Phase 1: validate constraints & reserve node/task atomically under tree lock.
	_, err = o.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
		parent, ok := t.Nodes[ref.NodeID]
		if !ok {
			return fmt.Errorf("caller node %q not found in active tree", ref.NodeID)
		}
		if parent.Depth+1 > t.MaxDepth {
			return fmt.Errorf("max nesting depth %d reached (node %q is at depth %d); execute directly instead of spawning",
				t.MaxDepth, parent.ID, parent.Depth)
		}
		if len(t.Nodes) >= t.MaxActive {
			return fmt.Errorf("max active nodes (%d) reached; reuse an idle node (`--reuse` / `bswarm assign`) or retire finished nodes first",
				t.MaxActive)
		}

		// Resolve official vs external gateway provider/pool and check concurrency slot availability under tree.lock.
		resTarget, rerr := o.providersCfg().ResolveTarget(opts.Provider, opts.Model, t.Nodes)
		if rerr != nil {
			return rerr
		}
		if resTarget.IsExternal && o.GatewayLaunch == nil {
			return fmt.Errorf("external provider %q selected, but no gateway launcher is configured", resTarget.ProviderName)
		}
		resolved = resTarget

		// Build hierarchical node ID (non-root children are always scoped under parent.ID/).
		if rawID == "" {
			rawID = fmt.Sprintf("%s-%d", store.SafeFileKey(role), t.NextTaskSeq+1)
		}
		if parent.ID != "root" {
			prefix := parent.ID + "/"
			if !strings.HasPrefix(rawID, prefix) {
				rawID = strings.TrimLeft(strings.ReplaceAll(rawID, "\\", "/"), "/")
				if strings.Contains(rawID, "/") {
					rawID = store.SafeFileKey(rawID)
				}
				nodeID = prefix + rawID
			} else {
				nodeID = rawID
			}
		} else {
			nodeID = rawID
		}
		if _, exists := t.Nodes[nodeID]; exists {
			return fmt.Errorf("active node %q already exists (use `bswarm assign %s` to reuse it)", nodeID, nodeID)
		}

		// Validate shared-workspace file ownership conflicts.
		wsMode := store.WorkspaceShared
		wsPath := parent.WorkspacePath
		if opts.UseWorktree {
			wsMode = store.WorkspaceWorktree
		}
		if wsMode == store.WorkspaceShared && len(opts.Owns) > 0 {
			for _, other := range t.Nodes {
				if other.ID == parent.ID || !other.IsActive() || other.WorkspaceMode != store.WorkspaceShared {
					continue
				}
				if pa, pb, conflict := store.OwnsOverlap(opts.Owns, other.Owns); conflict {
					return fmt.Errorf("ownership conflict in shared workspace: %q overlaps with active node %q (%q); use `--worktree` or disjoint `--owns`",
						pa, other.ID, pb)
				}
			}
		}

		t.NextTaskSeq++
		taskID = fmt.Sprintf("t%d", t.NextTaskSeq)
		if err := store.ValidateNewTaskDeps(t, taskID, opts.DependsOn); err != nil {
			return err
		}

		// Spill overly long goal text into spec file.
		goalSummary, specFile, _, err := o.Store.SpillIfLong(t.ID, nodeID, "spec_"+taskID, opts.Goal, opts.SpecFile, store.MaxSummaryRunes)
		if err != nil {
			return err
		}

		repoRoot = t.RepoRoot
		wtRoot = o.Store.WorktreesRoot(t.ID)
		if wsMode == store.WorkspaceWorktree {
			info, werr := worktree.Create(repoRoot, wtRoot, t.ID, nodeID)
			if werr != nil {
				return werr
			}
			wtInfo = info
			createdWT = true
			wsPath = info.Path
		}

		now := time.Now().UTC()
		startNow = store.DependenciesSatisfied(t, opts.DependsOn) && !resolved.NeedsQueue
		nState := store.NodeQueued
		tState := store.TaskQueued
		if startNow {
			nState = store.NodeWorking
			tState = store.TaskRunning
		}

		modelDisplay := opts.Model
		if resolved.IsExternal {
			modelDisplay = resolved.DisplayModel
		}
		n := &store.Node{
			ID:             nodeID,
			Role:           role,
			ParentID:       parent.ID,
			Depth:          parent.Depth + 1,
			Model:          modelDisplay,
			Provider:       resolved.ProviderName,
			ProviderModel:  resolved.Model,
			Pool:           resolved.PoolName,
			ConcurrencyKey: resolved.ConcurrencyKey,
			State:          nState,
			WorkspaceMode:  wsMode,
			WorkspacePath:  wsPath,
			Owns:           opts.Owns,
			CurrentTaskID:  taskID,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if resolved.IsExternal {
			n.WorkerLog = o.Store.GatewayLogPath(t.ID, nodeID)
			n.IsolatedHome = o.Store.GatewayHomeDir(t.ID, nodeID)
		} else {
			n.ProviderModel = ""
		}
		if wtInfo != nil {
			n.Branch = wtInfo.Branch
		}
		tk := &store.Task{
			ID:         taskID,
			AssigneeID: nodeID,
			CreatedBy:  parent.ID,
			Goal:       goalSummary,
			SpecFile:   specFile,
			DependsOn:  opts.DependsOn,
			VerifyCmd:  strings.TrimSpace(opts.VerifyCmd),
			State:      tState,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		t.Nodes[nodeID] = n
		t.Tasks[taskID] = tk

		treeID = t.ID
		artifactsDir = o.Store.ArtifactsDir(t.ID, nodeID)
		nC := *n
		tkC := *tk
		tC := *t
		nodeCopy = &nC
		taskCopy = &tkC
		treeCopy = &tC
		return nil
	})
	if err != nil {
		if createdWT && wtInfo != nil {
			_ = worktree.Remove(repoRoot, wtInfo.Path, wtInfo.Branch, true)
		}
		return nil, err
	}

	var convID string
	rollbackSpawn := func() {
		_, _ = o.Store.UpdateTree(treeID, func(t *store.Tree) error {
			delete(t.Nodes, nodeID)
			delete(t.Tasks, taskID)
			return nil
		})
		if createdWT && wtInfo != nil {
			_ = worktree.Remove(repoRoot, wtInfo.Path, wtInfo.Branch, true)
		}
		if taskCopy != nil && taskCopy.SpecFile != "" && taskCopy.SpecFile != opts.SpecFile {
			_ = os.Remove(taskCopy.SpecFile)
		}
	}
	if startNow {
		prompt := BuildSpawnPrompt(treeCopy, nodeCopy, taskCopy, artifactsDir)
		if resolved.IsExternal {
			if lerr := o.GatewayLaunch(gateway.LaunchRequest{
				TreeID:        treeID,
				NodeID:        nodeID,
				TaskID:        taskID,
				Prompt:        prompt,
				ProvidersFile: o.ProvidersFile,
			}); lerr != nil {
				rollbackSpawn()
				return nil, fmt.Errorf("launch gateway worker for %q (%s): %w", nodeID, resolved.DisplayModel, lerr)
			}
		} else {
			title := fmt.Sprintf("[bswarm] %s (%s)", nodeCopy.ID, nodeCopy.Role)
			convID, err = o.API.NewConversation(ctx, agentapi.NewConversationOpts{
				Model:   opts.Model,
				Title:   title,
				Profile: opts.Profile,
				Prompt:  prompt,
			})
			if err != nil {
				rollbackSpawn()
				return nil, fmt.Errorf("launch child conversation for %q: %w", nodeID, err)
			}
			stillActive := false
			_, err = o.Store.UpdateTree(treeID, func(t *store.Tree) error {
				if n, ok := t.Nodes[nodeID]; ok {
					n.ConversationID = convID
					n.UpdatedAt = time.Now().UTC()
					stillActive = true
				}
				return nil
			})
			if err != nil {
				rollbackSpawn()
				return nil, err
			}
			if stillActive {
				if err := o.Store.BindConversation(convID, treeID, nodeID); err != nil {
					rollbackSpawn()
					return nil, err
				}
			}
		}
	}

	if o.WatchStarter != nil {
		_ = o.WatchStarter(treeID)
	}
	o.mirrorEvent(treeID, nodeCopy.ParentID, nodeID, "assign", taskID, taskCopy.Goal, taskCopy.SpecFile)

	return &SpawnResult{
		TreeID:         treeID,
		NodeID:         nodeID,
		TaskID:         taskID,
		ConversationID: convID,
		Provider:       nodeCopy.Provider,
		Model:          nodeCopy.Model,
		Pool:           nodeCopy.Pool,
		ConcurrencyKey: nodeCopy.ConcurrencyKey,
		Reused:         false,
		State:          nodeCopy.State,
		WorkspacePath:  nodeCopy.WorkspacePath,
		Branch:         nodeCopy.Branch,
		SpecFile:       taskCopy.SpecFile,
	}, nil
}

type AssignOpts struct {
	CallerConvID string
	TreeID       string
	NodeID       string
	Goal         string
	SpecFile     string
	VerifyCmd    string
	Owns         []string
}

type AssignResult struct {
	TreeID         string `json:"tree_id"`
	NodeID         string `json:"node_id"`
	TaskID         string `json:"task_id"`
	ConversationID string `json:"conversation_id"`
	SpecFile       string `json:"spec_file,omitempty"`
}

// Assign reuses an existing idle/error/blocked direct child node with a new task,
// preserving its warm codebase context instead of killing and cold-starting a new agent.
func (o *Orchestrator) Assign(ctx context.Context, opts AssignOpts) (*AssignResult, error) {
	if strings.TrimSpace(opts.NodeID) == "" || strings.TrimSpace(opts.Goal) == "" {
		return nil, errors.New("node id and --goal are required")
	}
	ref, err := o.resolveActor(opts.CallerConvID, opts.TreeID)
	if err != nil {
		return nil, err
	}

	var (
		targetConv string
		notice     string
		taskID     string
		specFile   string
		prevState  store.NodeState
		prevTaskID string
		prevOwns   []string
		assignedM  store.Mail
		isExternal bool
	)
	_, err = o.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
		n, ok := t.Nodes[opts.NodeID]
		if !ok {
			return fmt.Errorf("active node %q not found", opts.NodeID)
		}
		if ref.NodeID != "root" && n.ParentID != ref.NodeID {
			return fmt.Errorf("node %q can only assign its own direct children (target %q belongs to %q)",
				ref.NodeID, n.ID, n.ParentID)
		}
		isExternal = n.Provider != ""
		if !isExternal && n.ConversationID == "" {
			return fmt.Errorf("node %q has no conversation yet (state=%s)", n.ID, n.State)
		}
		if n.State == store.NodeWorking || n.State == store.NodeBlocked {
			return fmt.Errorf("node %q is currently %s on %s; resolve or wait before reassigning", n.ID, n.State, n.CurrentTaskID)
		}
		if isExternal {
			if o.GatewayLaunch == nil {
				return fmt.Errorf("node %q uses external provider %q, but no gateway launcher is configured", n.ID, n.Provider)
			}
			_, avail, cerr := o.providersCfg().CanStartNode(n, t.Nodes)
			if cerr != nil {
				return cerr
			}
			if !avail {
				return fmt.Errorf("concurrency slot %q for node %q is currently busy; wait for active worker to finish", n.ConcurrencyKey, n.ID)
			}
		}
		// Validate ownership conflicts if new Owns were specified on shared workspace.
		if len(opts.Owns) > 0 && n.WorkspaceMode == store.WorkspaceShared {
			for _, other := range t.Nodes {
				if other.ID == n.ID || other.ID == ref.NodeID || !other.IsActive() || other.WorkspaceMode != store.WorkspaceShared {
					continue
				}
				if pa, pb, conflict := store.OwnsOverlap(opts.Owns, other.Owns); conflict {
					return fmt.Errorf("ownership conflict in shared workspace: %q overlaps with active node %q (%q)",
						pa, other.ID, pb)
				}
			}
		}

		prevState = n.State
		prevTaskID = n.CurrentTaskID
		prevOwns = append([]string(nil), n.Owns...)
		if len(opts.Owns) > 0 {
			n.Owns = append([]string(nil), opts.Owns...)
		}
		t.NextTaskSeq++
		taskID = fmt.Sprintf("t%d", t.NextTaskSeq)
		goalSummary, sf, _, err := o.Store.SpillIfLong(t.ID, n.ID, "spec_"+taskID, opts.Goal, opts.SpecFile, store.MaxSummaryRunes)
		if err != nil {
			return err
		}
		specFile = sf
		now := time.Now().UTC()
		tk := &store.Task{
			ID:         taskID,
			AssigneeID: n.ID,
			CreatedBy:  ref.NodeID,
			Goal:       goalSummary,
			SpecFile:   specFile,
			VerifyCmd:  strings.TrimSpace(opts.VerifyCmd),
			State:      store.TaskRunning,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		t.Tasks[taskID] = tk
		n.CurrentTaskID = taskID
		n.State = store.NodeWorking
		n.TouchedFiles = nil
		n.LastAlertKey = ""
		n.LastError = ""
		n.UpdatedAt = now
		targetConv = n.ConversationID
		notice = BuildAssignNotice(n, tk)

		assignedM, err = o.Store.AppendMailUnlocked(t, n.ID, store.Mail{
			From:     ref.NodeID,
			Kind:     "assign",
			TaskID:   taskID,
			Summary:  goalSummary,
			Artifact: specFile,
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	var wakeErr error
	if isExternal {
		wakeErr = o.GatewayLaunch(gateway.LaunchRequest{
			TreeID:        ref.TreeID,
			NodeID:        opts.NodeID,
			TaskID:        taskID,
			Prompt:        notice,
			ProvidersFile: o.ProvidersFile,
		})
	} else {
		wakeErr = o.API.SendMessage(ctx, targetConv, "[bswarm] new task "+taskID, notice)
	}
	if wakeErr != nil {
		// Roll back node/task state on wake failure so node does not stay stuck working.
		_, _ = o.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
			if n, ok := t.Nodes[opts.NodeID]; ok && n.CurrentTaskID == taskID {
				n.State = prevState
				n.CurrentTaskID = prevTaskID
				n.Owns = prevOwns
				n.UpdatedAt = time.Now().UTC()
			}
			delete(t.Tasks, taskID)
			return nil
		})
		return nil, fmt.Errorf("wake node %q: %w", opts.NodeID, wakeErr)
	}
	o.markDelivered(ref.TreeID, opts.NodeID, assignedM.Seq)
	if o.WatchStarter != nil {
		_ = o.WatchStarter(ref.TreeID)
	}
	o.mirrorEvent(ref.TreeID, ref.NodeID, opts.NodeID, "assign", taskID, opts.Goal, specFile)
	return &AssignResult{
		TreeID:         ref.TreeID,
		NodeID:         opts.NodeID,
		TaskID:         taskID,
		ConversationID: targetConv,
		SpecFile:       specFile,
	}, nil
}

type DoneOpts struct {
	CallerConvID string
	TreeID       string
	ActorNodeID  string // optional explicit node ID (used by gateway supervisor auto-close)
	Summary      string
	Artifact     string
	Status       string // "done" (default) | "failed"
}

type DoneResult struct {
	TreeID        string   `json:"tree_id"`
	NodeID        string   `json:"node_id"`
	TaskID        string   `json:"task_id,omitempty"`
	Notified      string   `json:"notified_parent,omitempty"`
	Summary       string   `json:"summary"`
	Artifact      string   `json:"artifact,omitempty"`
	PromotedTasks []string `json:"promoted_tasks,omitempty"`
}

// Done finishes the caller's current task, enforces verify_cmd, spills overly long summaries
// to an artifact file, transitions the node to idle (for reuse), wakes ONLY the direct parent,
// and promotes any newly unblocked DAG/concurrency-queued tasks.
func (o *Orchestrator) Done(ctx context.Context, opts DoneOpts) (*DoneResult, error) {
	if strings.TrimSpace(opts.Summary) == "" {
		return nil, errors.New("--summary is required")
	}
	status := strings.ToLower(strings.TrimSpace(opts.Status))
	if status == "" {
		status = "done"
	}
	if status != "done" && status != "failed" {
		return nil, fmt.Errorf("--status must be done or failed, got %q", status)
	}

	var (
		ref *store.ConvRef
		err error
	)
	if strings.TrimSpace(opts.ActorNodeID) != "" && strings.TrimSpace(opts.TreeID) != "" {
		ref = &store.ConvRef{TreeID: strings.TrimSpace(opts.TreeID), NodeID: strings.TrimSpace(opts.ActorNodeID)}
	} else {
		ref, err = o.resolveActor(opts.CallerConvID, opts.TreeID)
		if err != nil {
			return nil, err
		}
	}

	// Pre-check verify_cmd before acquiring tree write lock for long-running verification.
	tSnap, err := o.Store.LoadTree(ref.TreeID)
	if err != nil {
		return nil, err
	}
	selfSnap, ok := tSnap.Nodes[ref.NodeID]
	if !ok {
		return nil, fmt.Errorf("node %q not found in active tree", ref.NodeID)
	}
	if status == "done" && selfSnap.CurrentTaskID != "" {
		if tk, ok := tSnap.Tasks[selfSnap.CurrentTaskID]; ok && tk.VerifyCmd != "" && o.RunVerify != nil {
			out, verr := o.RunVerify(ctx, selfSnap.WorkspacePath, tk.VerifyCmd)
			if verr != nil {
				return nil, fmt.Errorf("verify command failed (`%s`): %v\n%s", tk.VerifyCmd, verr, out)
			}
		}
	}

	var (
		parentID      string
		parentConv    string
		parentExt     bool
		parentTaskID  string
		sentMail      store.Mail
		taskID        string
		finalSum      string
		finalArt      string
		cascadeAlerts []struct {
			conv string
			mail store.Mail
		}
	)
	expectedTaskID := selfSnap.CurrentTaskID
	_, err = o.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
		self, ok := t.Nodes[ref.NodeID]
		if !ok {
			return fmt.Errorf("node %q not found in active tree", ref.NodeID)
		}
		if self.State != store.NodeWorking && self.State != store.NodeBlocked {
			return fmt.Errorf("node %q is already %s (no open task to complete)", self.ID, self.State)
		}
		if expectedTaskID != "" && self.CurrentTaskID != expectedTaskID {
			return fmt.Errorf("task changed during verification (expected %s, now %s); re-run bswarm done", expectedTaskID, self.CurrentTaskID)
		}
		taskID = self.CurrentTaskID
		summary, art, _, err := o.Store.SpillIfLong(t.ID, self.ID, "report_"+taskID, opts.Summary, opts.Artifact, store.MaxSummaryRunes)
		if err != nil {
			return err
		}
		finalSum = summary
		finalArt = art

		now := time.Now().UTC()
		if taskID != "" {
			if tk, ok := t.Tasks[taskID]; ok {
				if status == "done" {
					tk.State = store.TaskDone
					if t.CompletedTaskIDs == nil {
						t.CompletedTaskIDs = map[string]bool{}
					}
					t.CompletedTaskIDs[taskID] = true
				} else {
					tk.State = store.TaskFailed
				}
				tk.Summary = summary
				tk.Artifact = art
				tk.ChangedFiles = append([]string(nil), self.TouchedFiles...)
				tk.UpdatedAt = now
			}
		}
		self.CurrentTaskID = ""
		if status == "done" {
			self.State = store.NodeIdle
			self.LastError = ""
		} else {
			self.State = store.NodeError
			self.LastError = summary
		}
		self.LastAlertKey = ""
		self.UpdatedAt = now

		// Propagate failed dependency to any queued downstream tasks so they never deadlock forever.
		if status == "failed" && taskID != "" {
			for _, qtk := range t.Tasks {
				if qtk.State != store.TaskQueued {
					continue
				}
				if depID, depState, bad := store.FailedDependency(t, qtk.DependsOn); bad {
					qtk.State = store.TaskFailed
					qtk.Summary = fmt.Sprintf("aborted: dependency %s %s", depID, depState)
					qtk.UpdatedAt = now
					if qn, ok := t.Nodes[qtk.AssigneeID]; ok {
						qn.State = store.NodeError
						qn.LastError = qtk.Summary
						qn.CurrentTaskID = ""
						qn.UpdatedAt = now
						if p, ok := t.Nodes[qn.ParentID]; ok {
							if am, aerr := o.Store.AppendMailUnlocked(t, p.ID, store.Mail{
								From:    qn.ID,
								Kind:    "failed",
								TaskID:  qtk.ID,
								Summary: qtk.Summary,
							}); aerr == nil && p.ConversationID != "" {
								cascadeAlerts = append(cascadeAlerts, struct {
									conv string
									mail store.Mail
								}{conv: p.ConversationID, mail: am})
							}
						}
					}
				}
			}
		}

		// Strictly notify ONLY the direct parent (tree encapsulation: grandparent never sees child noise).
		if self.ParentID != "" {
			if p, ok := t.Nodes[self.ParentID]; ok {
				parentID = p.ID
				parentConv = p.ConversationID
				parentExt = p.Provider != ""
				parentTaskID = p.CurrentTaskID
				m, merr := o.Store.AppendMailUnlocked(t, p.ID, store.Mail{
					From:     self.ID,
					Kind:     status,
					TaskID:   taskID,
					Summary:  summary,
					Artifact: art,
				})
				if merr != nil {
					return merr
				}
				sentMail = m
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	notified := parentID
	if parentExt && parentID != "" && o.GatewayLaunch != nil {
		// External parent node can be resumed via gateway launcher if idle/waiting.
		if err := o.GatewayLaunch(gateway.LaunchRequest{
			TreeID:        ref.TreeID,
			NodeID:        parentID,
			TaskID:        parentTaskID,
			Prompt:        BuildMailWakeNotice(sentMail),
			ProvidersFile: o.ProvidersFile,
		}); err != nil {
			notified = parentID + " (queued for watchdog relay: " + err.Error() + ")"
		} else {
			o.markDelivered(ref.TreeID, parentID, sentMail.Seq)
		}
	} else if parentConv != "" {
		title := fmt.Sprintf("[bswarm] %s %s", ref.NodeID, status)
		if err := o.API.SendMessage(ctx, parentConv, title, BuildMailWakeNotice(sentMail)); err != nil {
			notified = parentID + " (queued for watchdog relay: " + err.Error() + ")"
		} else {
			o.markDelivered(ref.TreeID, parentID, sentMail.Seq)
		}
	}
	for _, ca := range cascadeAlerts {
		if err := o.API.SendMessage(ctx, ca.conv, "[bswarm] queued task aborted", BuildMailWakeNotice(ca.mail)); err == nil {
			o.markDelivered(ref.TreeID, ca.mail.To, ca.mail.Seq)
		}
	}

	o.mirrorEvent(ref.TreeID, ref.NodeID, parentID, status, taskID, finalSum, finalArt)
	var promoted []string
	// Inside a sandboxed gateway worker subprocess, defer queue promotion to the host-side
	// `_gw-run` supervisor (or watchdog) so newly launched child workers aren't reaped with the sandbox.
	if os.Getenv("BSWARM_IN_GATEWAY_WORKER") != "1" {
		promoted, _ = o.PromoteReadyTasks(ctx, ref.TreeID)
	}

	return &DoneResult{
		TreeID:        ref.TreeID,
		NodeID:        ref.NodeID,
		TaskID:        taskID,
		Notified:      notified,
		Summary:       finalSum,
		Artifact:      finalArt,
		PromotedTasks: promoted,
	}, nil
}

// PromoteReadyTasks atomically claims queued tasks whose DAG dependencies AND external
// concurrency slots are satisfied under tree.lock BEFORE launching conversations/workers.
func (o *Orchestrator) PromoteReadyTasks(ctx context.Context, treeID string) ([]string, error) {
	type claimed struct {
		node         store.Node
		task         store.Task
		treeSnap     store.Tree
		artifactsDir string
		isExternal   bool
		prevProvider string
		prevModel    string
		prevProvMod  string
		prevCKey     string
	}
	var claims []claimed
	_, err := o.Store.UpdateTree(treeID, func(t *store.Tree) error {
		ready := store.ReadyQueuedTasks(t)
		now := time.Now().UTC()
		for _, tk := range ready {
			n, ok := t.Nodes[tk.AssigneeID]
			if !ok || n.State != store.NodeQueued || tk.State != store.TaskQueued {
				continue
			}
			isExt := n.Provider != "" || n.Pool != ""
			prevProv, prevMod, prevProvMod, prevCKey := n.Provider, n.Model, n.ProviderModel, n.ConcurrencyKey
			if isExt {
				if o.GatewayLaunch == nil {
					continue
				}
				res, avail, cerr := o.providersCfg().CanStartNode(n, t.Nodes)
				if cerr != nil || !avail {
					continue
				}
				if res.IsExternal {
					n.Provider = res.ProviderName
					n.ProviderModel = res.Model
					n.Model = res.DisplayModel
					n.ConcurrencyKey = res.ConcurrencyKey
					if n.WorkerLog == "" {
						n.WorkerLog = o.Store.GatewayLogPath(t.ID, n.ID)
					}
					if n.IsolatedHome == "" {
						n.IsolatedHome = o.Store.GatewayHomeDir(t.ID, n.ID)
					}
				}
			}
			// Atomic claim under tree.lock so no concurrent Done/Watchdog can launch a duplicate
			// and subsequent loop iterations immediately see the occupied ConcurrencyKey slot.
			n.State = store.NodeWorking
			n.UpdatedAt = now
			tk.State = store.TaskRunning
			tk.UpdatedAt = now
			claims = append(claims, claimed{
				node:         *n,
				task:         *tk,
				treeSnap:     *t,
				artifactsDir: o.Store.ArtifactsDir(t.ID, n.ID),
				isExternal:   isExt,
				prevProvider: prevProv,
				prevModel:    prevMod,
				prevProvMod:  prevProvMod,
				prevCKey:     prevCKey,
			})
		}
		return nil
	})
	if err != nil || len(claims) == 0 {
		return nil, err
	}

	var launched []string
	for _, c := range claims {
		prompt := BuildSpawnPrompt(&c.treeSnap, &c.node, &c.task, c.artifactsDir)
		if c.isExternal {
			if lerr := o.GatewayLaunch(gateway.LaunchRequest{
				TreeID:        treeID,
				NodeID:        c.node.ID,
				TaskID:        c.task.ID,
				Prompt:        prompt,
				ProvidersFile: o.ProvidersFile,
			}); lerr != nil {
				_, _ = o.Store.UpdateTree(treeID, func(tr *store.Tree) error {
					now := time.Now().UTC()
					if node, ok := tr.Nodes[c.node.ID]; ok && node.State == store.NodeWorking && node.ConversationID == "" {
						node.State = store.NodeQueued
						node.Provider = c.prevProvider
						node.Model = c.prevModel
						node.ProviderModel = c.prevProvMod
						node.ConcurrencyKey = c.prevCKey
						node.UpdatedAt = now
					}
					if task, ok := tr.Tasks[c.task.ID]; ok && task.State == store.TaskRunning {
						task.State = store.TaskQueued
						task.UpdatedAt = now
					}
					return nil
				})
				continue
			}
			o.mirrorEvent(treeID, c.task.CreatedBy, c.node.ID, "assign", c.task.ID, c.task.Goal, c.task.SpecFile)
			launched = append(launched, c.task.ID)
			continue
		}

		title := fmt.Sprintf("[bswarm] %s (%s)", c.node.ID, c.node.Role)
		convID, err := o.API.NewConversation(ctx, agentapi.NewConversationOpts{
			Model:  c.node.Model,
			Title:  title,
			Prompt: prompt,
		})
		if err != nil {
			// Roll back claim to queued so next pass can retry cleanly.
			_, _ = o.Store.UpdateTree(treeID, func(tr *store.Tree) error {
				now := time.Now().UTC()
				if node, ok := tr.Nodes[c.node.ID]; ok && node.State == store.NodeWorking && node.ConversationID == "" {
					node.State = store.NodeQueued
					node.UpdatedAt = now
				}
				if task, ok := tr.Tasks[c.task.ID]; ok && task.State == store.TaskRunning {
					task.State = store.TaskQueued
					task.UpdatedAt = now
				}
				return nil
			})
			continue
		}
		stillActive := false
		_, uerr := o.Store.UpdateTree(treeID, func(tr *store.Tree) error {
			if node, ok := tr.Nodes[c.node.ID]; ok && node.State == store.NodeWorking {
				node.ConversationID = convID
				node.UpdatedAt = time.Now().UTC()
				stillActive = true
			}
			return nil
		})
		if uerr == nil && stillActive {
			if berr := o.Store.BindConversation(convID, treeID, c.node.ID); berr == nil {
				o.mirrorEvent(treeID, c.task.CreatedBy, c.node.ID, "assign", c.task.ID, c.task.Goal, c.task.SpecFile)
				launched = append(launched, c.task.ID)
			}
		}
	}
	return launched, nil
}

// CompleteGatewayTurn is invoked by the detached `_gw-run` supervisor when a gateway `agy` worker exits.
// If the worker already called `bswarm done` or `bswarm ask`, it merely clears WorkerPID and promotes queued tasks.
// Otherwise it auto-closes the task with the captured gateway output or error so slots never leak.
func (o *Orchestrator) CompleteGatewayTurn(ctx context.Context, treeID, nodeID, taskID string, outcome gateway.TurnOutcome) error {
	var (
		needAutoClose bool
		artifactsDir  string
	)
	_, err := o.Store.UpdateTree(treeID, func(t *store.Tree) error {
		n, ok := t.Nodes[nodeID]
		if !ok {
			return nil
		}
		n.WorkerPID = 0
		if outcome.ConversationID != "" && n.ConversationID == "" {
			n.ConversationID = outcome.ConversationID
		}
		n.UpdatedAt = time.Now().UTC()
		if n.State == store.NodeWorking && (taskID == "" || n.CurrentTaskID == taskID) {
			needAutoClose = true
			artifactsDir = o.Store.ArtifactsDir(t.ID, n.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if outcome.ConversationID != "" {
		_ = o.Store.BindConversation(outcome.ConversationID, treeID, nodeID)
	}
	if !needAutoClose {
		_, _ = o.PromoteReadyTasks(ctx, treeID)
		return nil
	}

	if outcome.ExitErr == nil && (outcome.Status == "" || strings.EqualFold(outcome.Status, "SUCCESS")) &&
		len(outcome.DeniedActions) == 0 && strings.TrimSpace(outcome.Response) != "" {
		artPath := filepath.Join(artifactsDir, "gateway_response_"+store.SafeFileKey(taskID)+".md")
		_ = store.WriteAtomic(artPath, []byte(outcome.Response), 0o644)
		sum, _ := store.TruncateRunes("[gateway] "+firstLine(outcome.Response), store.MaxSummaryRunes)
		_, derr := o.Done(ctx, DoneOpts{
			TreeID:      treeID,
			ActorNodeID: nodeID,
			Status:      "done",
			Summary:     sum,
			Artifact:    artPath,
		})
		return derr
	}

	var parts []string
	if outcome.EarlyStopReason != "" {
		parts = append(parts, outcome.EarlyStopReason)
	}
	if len(outcome.DeniedActions) > 0 {
		parts = append(parts, "denied_actions="+strings.Join(outcome.DeniedActions, ","))
	}
	if outcome.ResultError != "" && outcome.ResultError != outcome.EarlyStopReason {
		parts = append(parts, outcome.ResultError)
	}
	if outcome.Stderr != "" {
		parts = append(parts, firstLine(outcome.Stderr))
	}
	if outcome.ExitErr != nil {
		parts = append(parts, outcome.ExitErr.Error())
	}
	if len(parts) == 0 {
		parts = append(parts, "gateway worker exited without output or bswarm done")
	}
	errReason := strings.Join(parts, "; ")

	// Automatic Pool Failover: if this node came from a pool, has failover budget remaining (<=2),
	// and another healthy candidate exists in the pool, seamlessly switch to the backup target.
	const maxPoolFailovers = 2
	if o.GatewayLaunch != nil {
		var (
			failoverLaunch bool
			failoverQueued bool
			failoverFrom   string
			failoverTo     string
			failoverNode   store.Node
			failoverTask   store.Task
			failoverTree   store.Tree
			failoverArtDir string
		)
		_, ferr := o.Store.UpdateTree(treeID, func(t *store.Tree) error {
			n, ok := t.Nodes[nodeID]
			if !ok || n.State != store.NodeWorking || n.Pool == "" || n.FailoverCount >= maxPoolFailovers {
				return nil
			}
			tk, ok := t.Tasks[n.CurrentTaskID]
			if !ok {
				return nil
			}
			curTarget := ""
			if n.Provider != "" && n.ProviderModel != "" {
				curTarget = n.Provider + ":" + n.ProviderModel
			}
			if curTarget != "" {
				already := false
				for _, f := range n.FailedTargets {
					if f == curTarget {
						already = true
						break
					}
				}
				if !already {
					n.FailedTargets = append(n.FailedTargets, curTarget)
				}
			}

			// Temporarily release current slot so exclusion selection sees accurate occupancy.
			origState, origProv, origMod, origDisp, origCKey, origConv := n.State, n.Provider, n.ProviderModel, n.Model, n.ConcurrencyKey, n.ConversationID
			n.State = store.NodeQueued
			n.Provider = ""
			n.ProviderModel = ""
			n.ConcurrencyKey = ""

			next, serr := o.providersCfg().SelectFromPoolExcluding(n.Pool, t.Nodes, n.FailedTargets)
			if serr != nil {
				// No healthy candidates remain in pool -> restore fields and report failure normally.
				n.State = origState
				n.Provider = origProv
				n.ProviderModel = origMod
				n.Model = origDisp
				n.ConcurrencyKey = origCKey
				n.ConversationID = origConv
				return nil
			}

			now := time.Now().UTC()
			failoverFrom = curTarget
			n.FailoverCount++
			n.ConversationID = "" // fresh conversation on backup provider
			n.LastError = fmt.Sprintf("failover #%d from %s: %s", n.FailoverCount, curTarget, errReason)
			n.UpdatedAt = now

			if next.NeedsQueue {
				n.State = store.NodeQueued
				n.Model = "pool:" + n.Pool
				tk.State = store.TaskQueued
				tk.UpdatedAt = now
				failoverQueued = true
				failoverTo = "queued(pool:" + n.Pool + ")"
				return nil
			}

			n.State = store.NodeWorking
			n.Provider = next.ProviderName
			n.ProviderModel = next.Model
			n.Model = next.DisplayModel
			n.ConcurrencyKey = next.ConcurrencyKey
			if n.WorkerLog == "" {
				n.WorkerLog = o.Store.GatewayLogPath(t.ID, n.ID)
			}
			if n.IsolatedHome == "" {
				n.IsolatedHome = o.Store.GatewayHomeDir(t.ID, n.ID)
			}
			tk.State = store.TaskRunning
			tk.UpdatedAt = now

			failoverLaunch = true
			failoverTo = next.DisplayModel
			failoverNode = *n
			failoverTask = *tk
			failoverTree = *t
			failoverArtDir = o.Store.ArtifactsDir(t.ID, n.ID)
			return nil
		})
		if ferr == nil && (failoverLaunch || failoverQueued) {
			o.mirrorEvent(treeID, nodeID, failoverNode.ParentID, "failover", taskID,
				fmt.Sprintf("auto-failover %s -> %s (%s)", failoverFrom, failoverTo, errReason), "")
			if failoverLaunch {
				prompt := BuildSpawnPrompt(&failoverTree, &failoverNode, &failoverTask, failoverArtDir)
				if lerr := o.GatewayLaunch(gateway.LaunchRequest{
					TreeID:        treeID,
					NodeID:        nodeID,
					TaskID:        failoverTask.ID,
					Prompt:        prompt,
					ProvidersFile: o.ProvidersFile,
				}); lerr == nil {
					_, _ = o.PromoteReadyTasks(ctx, treeID)
					return nil
				}
			} else {
				_, _ = o.PromoteReadyTasks(ctx, treeID)
				return nil
			}
		}
	}

	errSum, _ := store.TruncateRunes("gateway worker failed: "+errReason, store.MaxSummaryRunes)
	_, derr := o.Done(ctx, DoneOpts{
		TreeID:      treeID,
		ActorNodeID: nodeID,
		Status:      "failed",
		Summary:     errSum,
	})
	return derr
}

type MailOpts struct {
	CallerConvID string
	TreeID       string
	Target       string // node ID or "parent"
	Kind         string // "ask" | "reply"
	Summary      string
	Artifact     string
}

// SendMail sends a point-to-point message strictly between parent and direct child (or root->any active node).
func (o *Orchestrator) SendMail(ctx context.Context, opts MailOpts) (*store.Mail, error) {
	if strings.TrimSpace(opts.Summary) == "" {
		return nil, errors.New("--summary is required")
	}
	ref, err := o.resolveActor(opts.CallerConvID, opts.TreeID)
	if err != nil {
		return nil, err
	}
	kind := opts.Kind
	if kind == "" {
		kind = "ask"
	}

	var (
		targetConv string
		targetID   string
		targetExt  bool
		targetTask string
		sent       store.Mail
	)
	_, err = o.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
		self, ok := t.Nodes[ref.NodeID]
		if !ok {
			return fmt.Errorf("caller node %q not found", ref.NodeID)
		}
		tid := strings.TrimSpace(opts.Target)
		if tid == "" || tid == "parent" {
			if self.ParentID == "" {
				return errors.New("root node has no parent; specify a child node id")
			}
			tid = self.ParentID
		}
		target, ok := t.Nodes[tid]
		if !ok {
			return fmt.Errorf("target node %q not found in active tree", tid)
		}
		// Enforce strict hierarchical communication boundary (no cross-branch broadcast spam).
		allowed := self.ParentID == target.ID || target.ParentID == self.ID || self.ID == "root"
		if !allowed {
			return fmt.Errorf("direct mail denied: %q can only message its parent (%q) or direct children, not %q",
				self.ID, self.ParentID, target.ID)
		}
		if kind == "ask" && target.ID == self.ParentID {
			self.State = store.NodeBlocked
			self.UpdatedAt = time.Now().UTC()
		}
		if kind == "reply" && (self.ID == target.ParentID || self.ID == "root") && target.State == store.NodeBlocked {
			target.State = store.NodeWorking
			target.UpdatedAt = time.Now().UTC()
		}
		m, err := o.Store.AppendMailUnlocked(t, target.ID, store.Mail{
			From:     self.ID,
			Kind:     kind,
			TaskID:   self.CurrentTaskID,
			Summary:  opts.Summary,
			Artifact: opts.Artifact,
		})
		if err != nil {
			return err
		}
		sent = m
		targetID = target.ID
		targetConv = target.ConversationID
		targetExt = target.Provider != ""
		targetTask = target.CurrentTaskID
		return nil
	})
	if err != nil {
		return nil, err
	}
	if targetExt && o.GatewayLaunch != nil {
		if err := o.GatewayLaunch(gateway.LaunchRequest{
			TreeID:        ref.TreeID,
			NodeID:        targetID,
			TaskID:        targetTask,
			Prompt:        BuildMailWakeNotice(sent),
			ProvidersFile: o.ProvidersFile,
		}); err == nil {
			o.markDelivered(ref.TreeID, sent.To, sent.Seq)
		}
	} else if targetConv != "" {
		if err := o.API.SendMessage(ctx, targetConv, fmt.Sprintf("[bswarm] %s from %s", kind, ref.NodeID), BuildMailWakeNotice(sent)); err == nil {
			o.markDelivered(ref.TreeID, sent.To, sent.Seq)
		}
	}
	o.mirrorEvent(ref.TreeID, sent.From, sent.To, sent.Kind, sent.TaskID, sent.Summary, sent.Artifact)
	return &sent, nil
}

type InboxResult struct {
	NodeID   string       `json:"node_id"`
	Messages []store.Mail `json:"messages"`
}

// Inbox atomically reads the caller's private mailbox (unread only by default) and advances InboxReadSeq under lock.
func (o *Orchestrator) Inbox(callerConvID, treeID string, includeAll bool) (*InboxResult, error) {
	ref, err := o.resolveActor(callerConvID, treeID)
	if err != nil {
		return nil, err
	}
	var mails []store.Mail
	_, err = o.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
		self, ok := t.Nodes[ref.NodeID]
		if !ok {
			return fmt.Errorf("node %q not found", ref.NodeID)
		}
		after := self.InboxReadSeq
		if includeAll {
			after = 0
		}
		var rerr error
		mails, rerr = o.Store.ReadInbox(ref.TreeID, ref.NodeID, after)
		if rerr != nil {
			return rerr
		}
		if len(mails) > 0 {
			lastSeq := mails[len(mails)-1].Seq
			if lastSeq > self.InboxReadSeq {
				self.InboxReadSeq = lastSeq
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &InboxResult{NodeID: ref.NodeID, Messages: mails}, nil
}

type RetireResult struct {
	TreeID           string   `json:"tree_id"`
	ArchivedNodes    []string `json:"archived_nodes"`
	RemovedWorktrees []string `json:"removed_worktrees,omitempty"`
}

// Retire archives nodeID (and optionally its entire descendant subtree) out of active.json
// and cleans up all associated git worktrees so dead records never bloat future status calls.
func (o *Orchestrator) Retire(callerConvID, treeID, nodeID string, cascade bool, cancel bool) (*RetireResult, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, errors.New("node id is required")
	}
	if nodeID == "root" {
		return nil, errors.New("cannot retire root node")
	}
	ref, err := o.resolveActor(callerConvID, treeID)
	if err != nil {
		return nil, err
	}

	var (
		toArchive    []string
		wtToRemove   []struct{ repo, path, branch string }
		pidsToKill   []int
		freedSlot    bool
		unbound      []string
		targetState  = store.NodeRetired
	)
	if cancel {
		targetState = store.NodeCanceled
		cascade = true
	}

	_, err = o.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
		target, ok := t.Nodes[nodeID]
		if !ok {
			return fmt.Errorf("active node %q not found", nodeID)
		}
		// Permission: root can retire anyone; non-root can only retire within its own subtree.
		if ref.NodeID != "root" {
			allowed := false
			for _, id := range store.Descendants(t, ref.NodeID) {
				if id == target.ID {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("node %q cannot retire %q (not in your subtree)", ref.NodeID, target.ID)
			}
		}
		desc := store.Descendants(t, target.ID)
		if len(desc) > 0 && !cascade {
			return fmt.Errorf("node %q has %d active descendant(s) (%s); pass --cascade to retire the whole subtree cleanly",
				target.ID, len(desc), strings.Join(desc, ", "))
		}
		// Archive leaves first, then target.
		toArchive = append(desc, target.ID)
		for _, id := range toArchive {
			if n, ok := t.Nodes[id]; ok {
				if n.WorkspaceMode == store.WorkspaceWorktree && n.WorkspacePath != "" {
					wtToRemove = append(wtToRemove, struct{ repo, path, branch string }{
						repo: t.RepoRoot, path: n.WorkspacePath, branch: n.Branch,
					})
				}
				if n.WorkerPID > 0 {
					pidsToKill = append(pidsToKill, n.WorkerPID)
				}
				if n.ConcurrencyKey != "" {
					freedSlot = true
				}
			}
		}
		var aerr error
		unbound, aerr = o.Store.ArchiveNodesUnlocked(t, toArchive, targetState)
		return aerr
	})
	if err != nil {
		return nil, err
	}
	_ = o.Store.UnbindConversations(unbound)

	for _, pid := range pidsToKill {
		_ = gateway.KillProcessGroup(pid)
	}

	var removed []string
	for _, w := range wtToRemove {
		_ = worktree.Remove(w.repo, w.path, w.branch, cancel)
		removed = append(removed, w.path)
	}
	if freedSlot && os.Getenv("BSWARM_IN_GATEWAY_WORKER") != "1" {
		_, _ = o.PromoteReadyTasks(context.Background(), ref.TreeID)
	}
	o.mirrorRoster(ref.TreeID)
	return &RetireResult{
		TreeID:           ref.TreeID,
		ArchivedNodes:    toArchive,
		RemovedWorktrees: removed,
	}, nil
}

type MergeOpts struct {
	CallerConvID string
	TreeID       string
	Target       string // nodeID or "all"
	Strategy     string // merge | squash | ff-only
	VerifyCmd    string
}

type MergeItem struct {
	NodeID      string `json:"node_id"`
	Branch      string `json:"branch"`
	StatSummary string `json:"stat_summary,omitempty"`
	Result      string `json:"result"`
}

// Merge inspects and merges worktree branches back into RepoRoot cleanly under a tree-level lock
// with strict subtree authorization.
func (o *Orchestrator) Merge(ctx context.Context, opts MergeOpts) ([]MergeItem, error) {
	ref, err := o.resolveActor(opts.CallerConvID, opts.TreeID)
	if err != nil {
		return nil, err
	}
	var items []MergeItem
	_, err = o.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
		subtree := store.SubtreeIDs(t, ref.NodeID)
		inSubtree := make(map[string]bool, len(subtree))
		for _, id := range subtree {
			inSubtree[id] = true
		}

		var targets []*store.Node
		if opts.Target == "" || opts.Target == "all" {
			for _, id := range subtree {
				n := t.Nodes[id]
				if n != nil && n.ID != ref.NodeID && n.WorkspaceMode == store.WorkspaceWorktree && n.Branch != "" {
					targets = append(targets, n)
				}
			}
		} else {
			n, ok := t.Nodes[opts.Target]
			if !ok {
				return fmt.Errorf("active node %q not found", opts.Target)
			}
			if ref.NodeID != "root" && !inSubtree[n.ID] {
				return fmt.Errorf("node %q cannot merge %q (not in your subtree)", ref.NodeID, n.ID)
			}
			if n.WorkspaceMode != store.WorkspaceWorktree || n.Branch == "" {
				return fmt.Errorf("node %q does not use a git worktree branch", n.ID)
			}
			targets = append(targets, n)
		}
		if len(targets) == 0 {
			return errors.New("no active worktree branches found to merge")
		}

		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
		preHead, _ := worktree.CurrentHead(t.RepoRoot)
		mergedAny := false
		rollback := func() {
			if mergedAny && preHead != "" {
				_ = worktree.ResetHard(t.RepoRoot, preHead)
			}
		}
		for _, n := range targets {
			rep, err := worktree.Inspect(t.RepoRoot, n.WorkspacePath, n.Branch)
			if err != nil {
				rollback()
				return fmt.Errorf("inspect %s: %w", n.ID, err)
			}
			if rep.Dirty {
				rollback()
				return fmt.Errorf("node %q worktree (%s) has uncommitted changes; ask it to commit before merging", n.ID, n.WorkspacePath)
			}
			if rep.AheadCommits == 0 {
				items = append(items, MergeItem{NodeID: n.ID, Branch: n.Branch, Result: "skipped (no new commits)"})
				continue
			}
			out, err := worktree.Merge(t.RepoRoot, n.Branch, opts.Strategy, fmt.Sprintf("bswarm: merge %s (%s)", n.ID, n.Role))
			if err != nil {
				rollback()
				return fmt.Errorf("merge %s (%s): %w", n.ID, n.Branch, err)
			}
			mergedAny = true
			items = append(items, MergeItem{
				NodeID:      n.ID,
				Branch:      n.Branch,
				StatSummary: rep.StatSummary,
				Result:      firstLine(out),
			})
		}
		if strings.TrimSpace(opts.VerifyCmd) != "" && o.RunVerify != nil {
			out, err := o.RunVerify(ctx, t.RepoRoot, opts.VerifyCmd)
			if err != nil {
				rollback()
				return fmt.Errorf("post-merge verify failed (`%s`): %v\n%s", opts.VerifyCmd, err, out)
			}
		}
		return nil
	})
	return items, err
}

// RenderStatus renders a strictly bounded (<= 1800 B) tree view of active nodes only.
func (o *Orchestrator) RenderStatus(callerConvID, treeID string, showArchived bool) (string, error) {
	ref, err := o.resolveActor(callerConvID, treeID)
	if err != nil {
		return "", err
	}
	t, err := o.Store.LoadTree(ref.TreeID)
	if err != nil {
		return "", err
	}
	ids := store.SubtreeIDs(t, ref.NodeID)
	var b strings.Builder
	fmt.Fprintf(&b, "Tree %s | active=%d/%d | archived=%d | max_depth=%d\n",
		t.ID, len(t.Nodes), t.MaxActive, t.ArchivedCount, t.MaxDepth)
	if t.Goal != "" {
		g, _ := store.TruncateRunes(t.Goal, 100)
		fmt.Fprintf(&b, "Goal: %s\n", g)
	}
	baseDepth := 0
	if self, ok := t.Nodes[ref.NodeID]; ok {
		baseDepth = self.Depth
	}
	for _, id := range ids {
		n := t.Nodes[id]
		indent := strings.Repeat("  ", max(0, n.Depth-baseDepth))
		unread := max(0, n.InboxTotalSeq-n.InboxReadSeq)
		taskPart := "-"
		if n.CurrentTaskID != "" {
			if tk, ok := t.Tasks[n.CurrentTaskID]; ok {
				g, _ := store.TruncateRunes(tk.Goal, 48)
				taskPart = fmt.Sprintf("%s(%s): %s", tk.ID, tk.State, g)
			} else {
				taskPart = n.CurrentTaskID
			}
		}
		extras := ""
		if n.Model != "" && (n.Provider != "" || n.Pool != "") {
			mShort, _ := store.TruncateRunes(n.Model, 32)
			extras += " model=" + mShort
		}
		if n.State == store.NodeQueued && n.ConcurrencyKey != "" {
			extras += " slot=" + n.ConcurrencyKey
		}
		if n.Branch != "" {
			extras += " wt=" + filepath.Base(n.Branch)
		}
		if unread > 0 {
			extras += fmt.Sprintf(" unread=%d", unread)
		}
		if n.LastError != "" {
			e, _ := store.TruncateRunes(n.LastError, 40)
			extras += " err=" + e
		}
		fmt.Fprintf(&b, "%s- %s [%s|%s]%s | %s\n", indent, n.ID, n.State, n.Role, extras, taskPart)
	}
	if showArchived && t.ArchivedCount > 0 {
		recs, _ := o.Store.ReadArchive(t.ID, 20)
		b.WriteString("\nArchived (latest 20):\n")
		for _, r := range recs {
			if r.Kind == "node" && r.Node != nil {
				fmt.Fprintf(&b, "  x %s [%s|%s] %s\n", r.Node.ID, r.Node.State, r.Node.Role, r.ArchivedAt.Format("15:04:05"))
			}
		}
		return store.TruncateBytes(b.String(), store.MaxStdoutBytes, "\n...(truncated)\n"), nil
	}
	return store.TruncateBytes(b.String(), store.MaxStatusBytes, "\n...(status capped)\n"), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	if s == "" {
		return "ok"
	}
	return s
}
