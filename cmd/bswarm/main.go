package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TangYani2024/bswarm/internal/agentapi"
	"github.com/TangYani2024/bswarm/internal/gateway"
	"github.com/TangYani2024/bswarm/internal/gc"
	"github.com/TangYani2024/bswarm/internal/hooks"
	"github.com/TangYani2024/bswarm/internal/orchestrator"
	"github.com/TangYani2024/bswarm/internal/provider"
	"github.com/TangYani2024/bswarm/internal/store"
	"github.com/TangYani2024/bswarm/internal/uimirror"
	"github.com/TangYani2024/bswarm/internal/watchdog"
)

const version = "0.5.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	var err error
	switch sub {
	case "init":
		err = cmdInit(args)
	case "spawn":
		err = cmdSpawn(args)
	case "assign":
		err = cmdAssign(args)
	case "done":
		err = cmdDone(args)
	case "ask":
		err = cmdMail("ask", args)
	case "reply":
		err = cmdMail("reply", args)
	case "inbox":
		err = cmdInbox(args)
	case "status", "tree":
		err = cmdStatus(args)
	case "retire":
		err = cmdRetire(false, args)
	case "cancel":
		err = cmdRetire(true, args)
	case "merge":
		err = cmdMerge(args)
	case "watch":
		err = cmdWatch(args)
	case "gc":
		err = cmdGC(args)
	case "sync-ui":
		err = cmdSyncUI(args)
	case "providers":
		err = cmdProviders(args)
	case "_gw-run":
		err = cmdGatewayRun(args)
	case "hook":
		cmdHook(args)
		return
	case "version", "--version", "-v":
		fmt.Printf("bswarm %s\n", version)
		return
	case "help", "-h", "--help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", sub)
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`bswarm — Zero-MCP hierarchical multi-agent orchestrator for Antigravity

Commands:
  init       [--goal "..."] [--max-depth 3] [--max-active 12] [--json]
  spawn      --goal "..." [--id name] [--role role] [--reuse] [--provider p] [--model m]
             [--worktree] [--owns "a/**,b/**"] [--depends-on t1,t2] [--verify "cmd"] [--spec-file p]
  assign     <node_id> --goal "..." [--verify "cmd"] [--spec-file p]
  done       --summary "..." [--artifact path] [--status done|failed]
  ask        <node_id|parent> --summary "..." [--artifact path]
  reply      <node_id|parent> --summary "..." [--artifact path]
  inbox      [--all] [--json]
  status     [--archived] [--tree id] [--json]
  retire     <node_id> [--cascade]
  cancel     <node_id>
  merge      <node_id|all> [--strategy merge|squash|ff-only] [--verify "cmd"]
  providers  [--tree id] [--json]
  gc         [--tree id] [--native-audit] [--apply] [--older-than 7d] [--json]
  sync-ui    [--tree id]
  watch      --tree <tree_id> [--poll 3s] [--stall 5m] [--nudge 2m30s] [--idle-exit 3m]
  hook       <pre-tool-use|post-tool-use|pre-invocation|stop>
`)
}

func defaultConv(override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	return strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
}

func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeOut(s string) {
	fmt.Print(store.TruncateBytes(s, store.MaxStdoutBytes, "\n...(truncated)\n"))
}

func writeJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func newOrch() (*orchestrator.Orchestrator, error) {
	st := store.New("")
	cwd, _ := os.Getwd()
	cfg, cfgPath, err := provider.Load(cwd, st.RootDir)
	if err != nil {
		return nil, err
	}
	o := orchestrator.New(st, agentapi.NewCLITransport(), startDetachedWatchdog)
	o.Providers = cfg
	o.ProvidersFile = cfgPath
	o.GatewayLaunch = func(req gateway.LaunchRequest) error {
		return gateway.LaunchDetached(st, req)
	}
	return o, nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	goal := fs.String("goal", "", "overall goal")
	maxDepth := fs.Int("max-depth", store.DefaultMaxDepth, "max nesting depth")
	maxActive := fs.Int("max-active", store.DefaultMaxActive, "max active nodes")
	repo := fs.String("repo", "", "repository root")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	t, created, err := orch.Init(orchestrator.InitOpts{
		ConversationID: defaultConv(*conv),
		Goal:           *goal,
		RepoRoot:       *repo,
		MaxDepth:       *maxDepth,
		MaxActive:      *maxActive,
		UpdateIfIdle:   true,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(map[string]any{"created": created, "tree": t})
		return nil
	}
	verb := "Initialized"
	if !created {
		verb = "Using existing"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s tree %s (max_depth=%d, max_active=%d, repo=%s)\n", verb, t.ID, t.MaxDepth, t.MaxActive, t.RepoRoot)
	if !created && len(t.Nodes) > 1 {
		if cwd, cerr := os.Getwd(); cerr == nil && cwd != "" && filepath.Clean(cwd) != filepath.Clean(t.RepoRoot) {
			fmt.Fprintf(&b, "Note: active tree %s still has %d active child node(s) bound to repo=%s (current cwd=%s).\n",
				t.ID, len(t.Nodes)-1, t.RepoRoot, cwd)
		}
	}
	writeOut(b.String())
	return nil
}

func cmdSpawn(args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	id := fs.String("id", "", "node short id")
	role := fs.String("role", "", "role label for reuse")
	goal := fs.String("goal", "", "task goal")
	specFile := fs.String("spec-file", "", "optional detailed spec file path")
	prov := fs.String("provider", "", "external gateway provider name (or official/pool)")
	model := fs.String("model", "", "model tier/ID, provider:model, or pool:name")
	profile := fs.String("profile", "", "agent profile")
	reuse := fs.Bool("reuse", false, "reuse an idle child node with the same role if available")
	wt := fs.Bool("worktree", false, "create isolated git worktree")
	owns := fs.String("owns", "", "comma-separated owned path globs (shared workspace lock)")
	deps := fs.String("depends-on", "", "comma-separated dependency task IDs")
	verify := fs.String("verify", "", "required verification command before bswarm done")
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	res, err := orch.Spawn(context.Background(), orchestrator.SpawnOpts{
		CallerConvID: defaultConv(*conv),
		TreeID:       *treeID,
		ID:           *id,
		Role:         *role,
		Goal:         *goal,
		SpecFile:     *specFile,
		Provider:     *prov,
		Model:        *model,
		Profile:      *profile,
		Reuse:        *reuse,
		UseWorktree:  *wt,
		Owns:         splitCSV(*owns),
		DependsOn:    splitCSV(*deps),
		VerifyCmd:    *verify,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(res)
		return nil
	}
	action := "Spawned"
	if res.Reused {
		action = "Reused idle"
	} else if res.State == store.NodeQueued {
		if res.ConcurrencyKey != "" || res.Pool != "" {
			action = "Queued (waiting on concurrency slot/DAG)"
		} else {
			action = "Queued (waiting on DAG deps)"
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s node %q | task=%s | state=%s | tree=%s", action, res.NodeID, res.TaskID, res.State, res.TreeID)
	if res.Model != "" {
		fmt.Fprintf(&b, " | model=%s", res.Model)
	}
	b.WriteByte('\n')
	if res.ConversationID != "" {
		fmt.Fprintf(&b, "conv=%s\n", res.ConversationID)
	}
	if res.Branch != "" {
		fmt.Fprintf(&b, "worktree=%s (branch %s)\n", res.WorkspacePath, res.Branch)
	}
	if res.SpecFile != "" {
		fmt.Fprintf(&b, "spec_file=%s\n", res.SpecFile)
	}
	writeOut(b.String())
	return nil
}

func cmdAssign(args []string) error {
	fs := flag.NewFlagSet("assign", flag.ContinueOnError)
	goal := fs.String("goal", "", "new task goal")
	specFile := fs.String("spec-file", "", "optional spec file")
	verify := fs.String("verify", "", "verify command")
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"cascade": true, "json": true})); err != nil {
		return err
	}
	nodeID := ""
	if fs.NArg() > 0 {
		nodeID = fs.Arg(0)
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	res, err := orch.Assign(context.Background(), orchestrator.AssignOpts{
		CallerConvID: defaultConv(*conv),
		TreeID:       *treeID,
		NodeID:       nodeID,
		Goal:         *goal,
		SpecFile:     *specFile,
		VerifyCmd:    *verify,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(res)
		return nil
	}
	writeOut(fmt.Sprintf("Assigned %s to idle node %q (conv=%s)\n", res.TaskID, res.NodeID, res.ConversationID))
	return nil
}

func cmdDone(args []string) error {
	fs := flag.NewFlagSet("done", flag.ContinueOnError)
	summary := fs.String("summary", "", "concise result summary (<=400 chars; longer spills to artifact)")
	artifact := fs.String("artifact", "", "optional artifact file path")
	status := fs.String("status", "done", "done or failed")
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	res, err := orch.Done(context.Background(), orchestrator.DoneOpts{
		CallerConvID: defaultConv(*conv),
		TreeID:       *treeID,
		Summary:      *summary,
		Artifact:     *artifact,
		Status:       *status,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(res)
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Completed %s on %q (now idle for reuse). Notified parent: %s\n", res.TaskID, res.NodeID, res.Notified)
	if res.Artifact != "" {
		fmt.Fprintf(&b, "artifact=%s\n", res.Artifact)
	}
	if len(res.PromotedTasks) > 0 {
		fmt.Fprintf(&b, "promoted_dag_tasks=%s\n", strings.Join(res.PromotedTasks, ", "))
	}
	writeOut(b.String())
	return nil
}

func cmdMail(kind string, args []string) error {
	fs := flag.NewFlagSet(kind, flag.ContinueOnError)
	summary := fs.String("summary", "", "concise message (<=400 chars)")
	artifact := fs.String("artifact", "", "optional artifact path")
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"cascade": true, "json": true})); err != nil {
		return err
	}
	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	m, err := orch.SendMail(context.Background(), orchestrator.MailOpts{
		CallerConvID: defaultConv(*conv),
		TreeID:       *treeID,
		Target:       target,
		Kind:         kind,
		Summary:      *summary,
		Artifact:     *artifact,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(m)
		return nil
	}
	writeOut(fmt.Sprintf("Sent %s #%d to %q\n", m.Kind, m.Seq, m.To))
	return nil
}

func cmdInbox(args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	all := fs.Bool("all", false, "include already-read messages")
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	res, err := orch.Inbox(defaultConv(*conv), *treeID, *all)
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(res)
		return nil
	}
	if len(res.Messages) == 0 {
		writeOut(fmt.Sprintf("Inbox (%s): no unread messages.\n", res.NodeID))
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Inbox (%s): %d message(s)\n", res.NodeID, len(res.Messages))
	for _, m := range res.Messages {
		fmt.Fprintf(&b, "#%d [%s] from=%s", m.Seq, m.Kind, m.From)
		if m.TaskID != "" {
			fmt.Fprintf(&b, " task=%s", m.TaskID)
		}
		fmt.Fprintf(&b, " (%s): %s\n", m.At.Format("15:04:05"), m.Summary)
		if m.Artifact != "" {
			fmt.Fprintf(&b, "   artifact: %s\n", m.Artifact)
		}
	}
	writeOut(b.String())
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	archived := fs.Bool("archived", false, "include latest 20 archived nodes")
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	if *asJSON {
		cid := defaultConv(*conv)
		tid := *treeID
		if tid == "" && cid != "" {
			if ref, _ := orch.Store.ResolveConversation(cid); ref != nil {
				tid = ref.TreeID
			}
		}
		if tid == "" {
			return fmt.Errorf("current conversation is not in any bswarm tree; run `bswarm init` first or pass --tree")
		}
		t, err := orch.Store.LoadTree(tid)
		if err != nil {
			return err
		}
		writeJSON(t)
		return nil
	}
	out, err := orch.RenderStatus(defaultConv(*conv), *treeID, *archived)
	if err != nil {
		return err
	}
	writeOut(out)
	return nil
}

func cmdRetire(cancel bool, args []string) error {
	name := "retire"
	if cancel {
		name = "cancel"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cascade := fs.Bool("cascade", cancel, "cascade to all descendant nodes")
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"cascade": true, "json": true})); err != nil {
		return err
	}
	nodeID := ""
	if fs.NArg() > 0 {
		nodeID = fs.Arg(0)
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	res, err := orch.Retire(defaultConv(*conv), *treeID, nodeID, *cascade, cancel)
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(res)
		return nil
	}
	writeOut(fmt.Sprintf("Archived %d node(s): %s (removed %d worktree(s))\n",
		len(res.ArchivedNodes), strings.Join(res.ArchivedNodes, ", "), len(res.RemovedWorktrees)))
	return nil
}

func cmdMerge(args []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	strategy := fs.String("strategy", "merge", "merge | squash | ff-only")
	verify := fs.String("verify", "", "optional verification command after merge")
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"cascade": true, "json": true})); err != nil {
		return err
	}
	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	orch, err := newOrch()
	if err != nil {
		return err
	}
	items, err := orch.Merge(context.Background(), orchestrator.MergeOpts{
		CallerConvID: defaultConv(*conv),
		TreeID:       *treeID,
		Target:       target,
		Strategy:     *strategy,
		VerifyCmd:    *verify,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(items)
		return nil
	}
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "- %s (%s): %s", it.NodeID, it.Branch, it.Result)
		if it.StatSummary != "" {
			fmt.Fprintf(&b, " [%s]", it.StatSummary)
		}
		b.WriteByte('\n')
	}
	writeOut(b.String())
	return nil
}

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	treeID := fs.String("tree", "", "tree id to watch")
	poll := fs.Duration("poll", 3*time.Second, "poll interval")
	stall := fs.Duration("stall", 0, "stall timeout (default 5m or BSWARM_STALL_TIMEOUT)")
	nudge := fs.Duration("nudge", 0, "mid-stall worker nudge timeout (default stall/2; negative disables)")
	idleExit := fs.Duration("idle-exit", 3*time.Minute, "auto-exit after idle duration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*treeID) == "" {
		return fmt.Errorf("--tree is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	st := store.New("")
	repoRoot, _ := os.Getwd()
	if tr, terr := st.LoadTree(*treeID); terr == nil && strings.TrimSpace(tr.RepoRoot) != "" {
		repoRoot = tr.RepoRoot
	}
	reloader := provider.NewReloader(repoRoot, st.RootDir)
	cfg, cfgPath, _, _ := reloader.Current()
	wd := watchdog.New(st, agentapi.NewCLITransport(), watchdog.Config{
		TreeID:       *treeID,
		PollInterval: *poll,
		StallTimeout: *stall,
		NudgeTimeout: *nudge,
		IdleExit:     *idleExit,
	})
	wd.ProviderReloader = reloader
	if cfg != nil {
		wd.Orch.Providers = cfg
		wd.Orch.ProvidersFile = cfgPath
	}
	wd.Orch.GatewayLaunch = func(req gateway.LaunchRequest) error {
		return gateway.LaunchDetached(st, req)
	}
	return wd.Run(ctx)
}

func resolveEditableProvidersPath(override string) (string, error) {
	if p := strings.TrimSpace(override); p != "" {
		return p, nil
	}
	if p := strings.TrimSpace(os.Getenv("BSWARM_PROVIDERS_FILE")); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "bswarm-providers.json"), nil
}

func cmdProviders(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "init":
			return cmdProvidersInit(args[1:])
		case "add":
			return cmdProvidersAdd(args[1:])
		case "remove", "rm":
			return cmdProvidersRemove(args[1:])
		}
	}
	fs := flag.NewFlagSet("providers", flag.ContinueOnError)
	treeID := fs.String("tree", "", "optional tree id to inspect live slot usage")
	conv := fs.String("conv", "", "override conversation id")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st := store.New("")
	cwd, _ := os.Getwd()
	cfg, cfgPath, err := provider.Load(cwd, st.RootDir)
	if err != nil {
		return err
	}
	activeSlots := map[string]int{}
	tid := strings.TrimSpace(*treeID)
	if tid == "" {
		if cid := defaultConv(*conv); cid != "" {
			if ref, _ := st.ResolveConversation(cid); ref != nil {
				tid = ref.TreeID
			}
		}
	}
	if tid != "" {
		if t, terr := st.LoadTree(tid); terr == nil {
			activeSlots = provider.WorkingCounts(t.Nodes)
		}
	}
	summaries := cfg.Summaries()
	if *asJSON {
		writeJSON(map[string]any{
			"config_path":  cfgPath,
			"providers":    summaries,
			"pools":        cfg.Pools,
			"active_slots": activeSlots,
		})
		return nil
	}
	var b strings.Builder
	if cfgPath == "" {
		b.WriteString("Providers config: (none found; using official Antigravity models only)\n")
		b.WriteString("Searched paths:\n")
		for _, p := range provider.DefaultConfigPaths(cwd, st.RootDir) {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		writeOut(b.String())
		return nil
	}
	fmt.Fprintf(&b, "Providers config: %s\n", cfgPath)
	for _, s := range summaries {
		lim := "unlimited"
		if s.MaxConcurrency > 0 {
			scope := s.ConcurrencyScope
			if scope == "" {
				scope = "provider"
			}
			lim = fmt.Sprintf("max=%d (%s)", s.MaxConcurrency, scope)
		}
		fmt.Fprintf(&b, "- %s [%s] %s | key=%s | conc=%s\n", s.Name, s.WireProtocol, s.URL, s.KeySource, lim)
		if len(s.Models) > 0 {
			fmt.Fprintf(&b, "    models: %s\n", strings.Join(s.Models, ", "))
		}
	}
	if len(cfg.Pools) > 0 {
		b.WriteString("Pools:\n")
		for pn, entries := range cfg.Pools {
			fmt.Fprintf(&b, "- pool:%s -> %s\n", pn, strings.Join(entries, ", "))
		}
	}
	if len(activeSlots) > 0 {
		b.WriteString("Active concurrency slots:\n")
		for k, v := range activeSlots {
			fmt.Fprintf(&b, "  %s = %d working\n", k, v)
		}
	}
	writeOut(b.String())
	return nil
}

func cmdProvidersInit(args []string) error {
	fs := flag.NewFlagSet("providers init", flag.ContinueOnError)
	outPath := fs.String("path", "", "target providers.json path (default ~/.gemini/bswarm-providers.json)")
	force := fs.Bool("force", false, "overwrite existing file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target, err := resolveEditableProvidersPath(*outPath)
	if err != nil {
		return err
	}
	if !*force {
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("%s already exists (pass --force to overwrite, or use `bswarm providers add`)", target)
		}
	}
	sample := &provider.Config{
		Providers: map[string]*provider.ProviderSpec{
			"deepseek": {
				URL:            "https://api.deepseek.com/v1",
				WireProtocol:   "openai",
				APIKeyEnv:      "DEEPSEEK_API_KEY",
				DefaultModel:   "deepseek-chat",
				Models:         []string{"deepseek-chat", "deepseek-reasoner"},
				MaxConcurrency: 0,
				Timeout:        "15m",
				IdleTimeout:    "90s",
			},
		},
		Pools: map[string][]string{},
	}
	raw, err := json.MarshalIndent(sample, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := store.WriteAtomic(target, raw, 0o600); err != nil {
		return err
	}
	writeOut(fmt.Sprintf("Initialized %s (hot-loaded automatically; no restart required).\n", target))
	return nil
}

func cmdProvidersAdd(args []string) error {
	fs := flag.NewFlagSet("providers add", flag.ContinueOnError)
	url := fs.String("url", "", "gateway base URL (required)")
	proto := fs.String("protocol", "openai", "wire protocol: openai | gemini")
	keyEnv := fs.String("key-env", "", "environment variable holding API key")
	keyFile := fs.String("key-file", "", "local file holding API key")
	keyLine := fs.Int("key-line", 1, "1-based non-empty line number in --key-file")
	keyInline := fs.String("key", "", "inline API key (prefer --key-env or --key-file)")
	models := fs.String("models", "", "comma-separated model IDs")
	defModel := fs.String("default-model", "", "default model when --model is omitted")
	maxConc := fs.Int("max-concurrency", 0, "optional max concurrent workers (0 = unlimited)")
	scope := fs.String("scope", "", "concurrency scope: provider | model | prefix:/")
	idleTimeout := fs.String("idle-timeout", "", "stream idle timeout (e.g. 90s, 0 to disable)")
	pool := fs.String("pool", "", "optional pool name to append these models into")
	outPath := fs.String("path", "", "target providers.json path")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("provider name is required: bswarm providers add <name> --url <url> [--models m1,m2]")
	}
	name := strings.TrimSpace(fs.Arg(0))
	if name == "" || name == "official" || name == "native" || name == "pool" {
		return fmt.Errorf("invalid provider name %q", name)
	}
	if strings.TrimSpace(*url) == "" {
		return fmt.Errorf("--url is required")
	}
	wp := strings.ToLower(strings.TrimSpace(*proto))
	if wp != "openai" && wp != "gemini" {
		return fmt.Errorf("--protocol must be openai or gemini, got %q", *proto)
	}

	target, err := resolveEditableProvidersPath(*outPath)
	if err != nil {
		return err
	}
	var cfg *provider.Config
	if _, serr := os.Stat(target); serr == nil {
		cfg, err = provider.LoadFile(target)
		if err != nil {
			return err
		}
	} else {
		cfg = &provider.Config{Providers: map[string]*provider.ProviderSpec{}, Pools: map[string][]string{}}
	}

	modelList := splitCSV(*models)
	dm := strings.TrimSpace(*defModel)
	if dm == "" && len(modelList) > 0 {
		dm = modelList[0]
	}
	spec := &provider.ProviderSpec{
		URL:              strings.TrimSpace(*url),
		WireProtocol:     wp,
		APIKey:           strings.TrimSpace(*keyInline),
		APIKeyEnv:        strings.TrimSpace(*keyEnv),
		APIKeyFile:       strings.TrimSpace(*keyFile),
		APIKeyLine:       *keyLine,
		Models:           modelList,
		DefaultModel:     dm,
		MaxConcurrency:   *maxConc,
		ConcurrencyScope: strings.TrimSpace(*scope),
		IdleTimeout:      strings.TrimSpace(*idleTimeout),
	}
	if spec.APIKeyFile == "" {
		spec.APIKeyLine = 0
	}
	cfg.Providers[name] = spec

	if pn := strings.TrimSpace(*pool); pn != "" && len(modelList) > 0 {
		existing := map[string]bool{}
		for _, e := range cfg.Pools[pn] {
			existing[e] = true
		}
		for _, m := range modelList {
			entry := name + ":" + m
			if !existing[entry] {
				cfg.Pools[pn] = append(cfg.Pools[pn], entry)
				existing[entry] = true
			}
		}
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := store.WriteAtomic(target, raw, 0o600); err != nil {
		return err
	}
	writeOut(fmt.Sprintf("Saved provider %q (%s) to %s — hot-loaded immediately (no restart needed).\n", name, spec.URL, target))
	return nil
}

func cmdProvidersRemove(args []string) error {
	fs := flag.NewFlagSet("providers remove", flag.ContinueOnError)
	outPath := fs.String("path", "", "target providers.json path")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("provider name is required: bswarm providers remove <name>")
	}
	name := strings.TrimSpace(fs.Arg(0))
	target, err := resolveEditableProvidersPath(*outPath)
	if err != nil {
		return err
	}
	cfg, err := provider.LoadFile(target)
	if err != nil {
		return err
	}
	if _, ok := cfg.Providers[name]; !ok {
		return fmt.Errorf("provider %q not found in %s", name, target)
	}
	delete(cfg.Providers, name)
	for pn, entries := range cfg.Pools {
		var kept []string
		for _, e := range entries {
			if !strings.HasPrefix(e, name+":") {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(cfg.Pools, pn)
		} else {
			cfg.Pools[pn] = kept
		}
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := store.WriteAtomic(target, raw, 0o600); err != nil {
		return err
	}
	writeOut(fmt.Sprintf("Removed provider %q from %s — hot-reloaded immediately.\n", name, target))
	return nil
}

func cmdGatewayRun(args []string) error {
	fs := flag.NewFlagSet("_gw-run", flag.ContinueOnError)
	treeID := fs.String("tree", "", "tree id")
	nodeID := fs.String("node", "", "node id")
	taskID := fs.String("task", "", "task id")
	promptFile := fs.String("prompt-file", "", "prompt file path")
	providersPath := fs.String("providers", "", "providers.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tid := strings.TrimSpace(*treeID)
	nid := strings.TrimSpace(*nodeID)
	tkid := strings.TrimSpace(*taskID)
	if tid == "" || nid == "" || strings.TrimSpace(*promptFile) == "" {
		return fmt.Errorf("_gw-run requires --tree, --node, and --prompt-file")
	}

	st := store.New("")
	t, err := st.LoadTree(tid)
	if err != nil {
		return err
	}
	n, ok := t.Nodes[nid]
	if !ok {
		return fmt.Errorf("node %q not found in tree %s", nid, tid)
	}

	var cfg *provider.Config
	cfgPath := strings.TrimSpace(*providersPath)
	if cfgPath != "" {
		cfg, err = provider.LoadFile(cfgPath)
	} else {
		cfg, cfgPath, err = provider.Load(t.RepoRoot, st.RootDir)
	}
	if err != nil {
		return err
	}

	orch := orchestrator.New(st, agentapi.NewCLITransport(), startDetachedWatchdog)
	orch.Providers = cfg
	orch.ProvidersFile = cfgPath
	orch.GatewayLaunch = func(req gateway.LaunchRequest) error {
		return gateway.LaunchDetached(st, req)
	}

	spec := cfg.Providers[n.Provider]
	if spec == nil {
		return orch.CompleteGatewayTurn(context.Background(), tid, nid, tkid, gateway.TurnOutcome{
			ExitErr: fmt.Errorf("provider %q not found in %s", n.Provider, cfgPath),
		})
	}
	promptBytes, err := os.ReadFile(*promptFile)
	if err != nil {
		return orch.CompleteGatewayTurn(context.Background(), tid, nid, tkid, gateway.TurnOutcome{
			ExitErr: fmt.Errorf("read prompt file: %w", err),
		})
	}
	_ = os.Remove(*promptFile)

	isoHome := n.IsolatedHome
	if isoHome == "" {
		isoHome = st.GatewayHomeDir(tid, nid)
	}
	logPath := n.WorkerLog
	if logPath == "" {
		logPath = st.GatewayLogPath(tid, nid)
	}
	model := n.ProviderModel
	if model == "" {
		model = n.Model
	}
	artDir := st.ArtifactsDir(tid, nid)
	_ = os.MkdirAll(artDir, 0o755)

	// Record supervisor PID before spawning agy so watchdog can track liveness.
	pid := os.Getpid()
	_, _ = st.UpdateTree(tid, func(tr *store.Tree) error {
		if node, ok := tr.Nodes[nid]; ok {
			node.WorkerPID = pid
			node.WorkerLog = logPath
			node.IsolatedHome = isoHome
			node.UpdatedAt = time.Now().UTC()
		}
		return nil
	})

	exe, _ := os.Executable()
	pluginDir := gateway.ResolvePluginDir(exe)
	if err := gateway.PrepareIsolatedHome(isoHome, n.WorkspacePath, artDir, st.RootDir, pluginDir, spec); err != nil {
		return orch.CompleteGatewayTurn(context.Background(), tid, nid, tkid, gateway.TurnOutcome{ExitErr: err})
	}
	backend, err := gateway.ResolveWorkerBackend(spec)
	if err != nil {
		return orch.CompleteGatewayTurn(context.Background(), tid, nid, tkid, gateway.TurnOutcome{ExitErr: err})
	}
	env, err := gateway.BuildAgyEnv(os.Environ(), isoHome, st.RootDir, tid, nid, exe, cfgPath, spec, model)
	if err != nil {
		return orch.CompleteGatewayTurn(context.Background(), tid, nid, tkid, gateway.TurnOutcome{ExitErr: err})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	outcome := gateway.RunTurn(ctx, backend, gateway.TurnOpts{
		AgyBin:          backend.Binary,
		IsoHome:         isoHome,
		HostAppDataDir:  uimirror.ResolveAppDataDir(t),
		WorkDir:         n.WorkspacePath,
		Env:             env,
		Model:           model,
		ConvID:          n.ConversationID,
		Mode:            spec.EffectiveMode(),
		Timeout:         spec.EffectiveTimeout(),
		IdleTimeout:     spec.EffectiveIdleTimeout(),
		ErrorStallGrace: spec.EffectiveErrorGrace(),
		Prompt:          string(promptBytes),
		LogPath:         logPath,
		OnConvID: func(cid string) {
			updatedTree, uerr := st.UpdateTree(tid, func(tr *store.Tree) error {
				if node, ok := tr.Nodes[nid]; ok && node.ConversationID == "" {
					node.ConversationID = cid
					node.UpdatedAt = time.Now().UTC()
				}
				return nil
			})
			_ = st.BindConversation(cid, tid, nid)
			if uerr == nil && updatedTree != nil {
				_ = uimirror.SyncRosterAndPlan(st, updatedTree)
			}
		},
	})
	return orch.CompleteGatewayTurn(context.Background(), tid, nid, tkid, outcome)
}

func cmdGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	treeID := fs.String("tree", "", "optional specific tree id")
	nativeAudit := fs.Bool("native-audit", false, "audit (and with --apply, clean) native Antigravity swarm killed zombies")
	apply := fs.Bool("apply", false, "actually delete reclaimable targets (default is read-only dry-run)")
	olderThan := fs.String("older-than", "0", "minimum age for native zombie cleanup (e.g. 7d, 24h)")
	asJSON := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st := store.New("")
	if *nativeAudit {
		dur, err := parseDurationDays(*olderThan)
		if err != nil {
			return err
		}
		rep, err := gc.AuditNativeSwarms(nil, dur, *apply)
		if err != nil {
			return err
		}
		if *asJSON {
			writeJSON(rep)
			return nil
		}
		writeOut(gc.FormatNativeAudit(rep))
		return nil
	}
	reps, err := gc.CleanOrphanWorktrees(st, *treeID, *apply)
	if err != nil {
		return err
	}
	if *asJSON {
		writeJSON(reps)
		return nil
	}
	if len(reps) == 0 {
		writeOut("GC: no orphan bswarm worktrees found.\n")
		return nil
	}
	var b strings.Builder
	for _, r := range reps {
		fmt.Fprintf(&b, "tree %s: %d orphan worktree(s) (removed=%d)\n", r.TreeID, len(r.OrphanWorktrees), r.RemovedCount)
	}
	if !*apply {
		b.WriteString("Pass --apply to remove orphan worktrees.\n")
	}
	writeOut(b.String())
	return nil
}

func cmdSyncUI(args []string) error {
	fs := flag.NewFlagSet("sync-ui", flag.ContinueOnError)
	treeID := fs.String("tree", "", "override tree id")
	conv := fs.String("conv", "", "override conversation id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st := store.New("")
	tid := strings.TrimSpace(*treeID)
	if tid == "" {
		if cid := defaultConv(*conv); cid != "" {
			if ref, _ := st.ResolveConversation(cid); ref != nil {
				tid = ref.TreeID
			}
		}
	}
	if tid == "" {
		return fmt.Errorf("no active bswarm tree found; pass --tree <id>")
	}
	t, err := st.UpdateTree(tid, func(tr *store.Tree) error {
		return uimirror.RebuildBoard(st, tr)
	})
	if err != nil {
		return err
	}
	writeOut(fmt.Sprintf("Synced tree %s to official Swarm UI dir: %s (seq=%d, members=%d)\n",
		t.ID, uimirror.SwarmDir(t), t.MirrorSeq, len(t.Nodes)))
	return nil
}

func hookFallback(event string) string {
	switch event {
	case "pre-tool-use":
		return `{"decision":"allow"}`
	case "stop":
		return `{"decision":"stop"}`
	default:
		return `{}`
	}
}

func cmdHook(args []string) {
	if len(args) == 0 {
		fmt.Println("{}")
		return
	}
	event := args[0]
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Println(hookFallback(event))
		return
	}
	var in hooks.HookInput
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			fmt.Println(hookFallback(event))
			return
		}
	}
	h := hooks.NewHandler(store.New(""))
	var out any = map[string]any{}
	switch event {
	case "pre-tool-use":
		out = h.HandlePreToolUse(&in)
	case "post-tool-use":
		out = h.HandlePostToolUse(&in)
	case "pre-invocation":
		out = h.HandlePreInvocation(&in)
	case "stop":
		out = h.HandleStop(&in)
	}
	b, err := json.Marshal(out)
	if err != nil {
		fmt.Println(hookFallback(event))
		return
	}
	fmt.Println(string(b))
}

// reorderFlagsFirst moves all flags ahead of positional arguments so Go's stdlib flag.FlagSet
// parses both `cmd <pos> --flag val` and `cmd --flag1 val1 <pos> --flag2 val2` without dropping trailing flags.
func reorderFlagsFirst(args []string, boolFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			if !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if !boolFlags[name] && i+1 < len(args) {
					i++
					flags = append(flags, args[i])
				}
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func parseDurationDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("duration must be non-negative: %q", s)
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must be non-negative: %q", s)
	}
	return d, nil
}
