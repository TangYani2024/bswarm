package sidecarui

import (
	"bufio"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TangYani2024/bswarm/internal/agentapi"
	"github.com/TangYani2024/bswarm/internal/orchestrator"
	"github.com/TangYani2024/bswarm/internal/provider"
	"github.com/TangYani2024/bswarm/internal/store"
)

//go:embed index.html
var indexHTML []byte

// Config configures the bswarm Sidecar HTTP UI server.
type Config struct {
	Host       string
	Port       int
	UIToken    string
	AppDataDir string
	StoreDir   string
}

// Server serves the native Antigravity AUX_PANE Sidecar Web UI for bswarm.
type Server struct {
	Cfg      Config
	Store    *store.Store
	AppData  string
	API      agentapi.Transport
	listener net.Listener
	httpSrv  *http.Server
}

// ResolveAppDataFromSidecarEnv derives <appData> from ANTIGRAVITY_EXECUTABLE_DATA_DIR
// (format: <appData>/sidecar_data/<plugin>/<sidecar>/data), falling back to ANTIGRAVITY_APP_DATA_DIR
// and store.ProbeAppDataDir.
func ResolveAppDataFromSidecarEnv() string {
	if execDir := strings.TrimSpace(os.Getenv("ANTIGRAVITY_EXECUTABLE_DATA_DIR")); execDir != "" {
		cur := filepath.Clean(execDir)
		for i := 0; i < 6; i++ {
			if strings.EqualFold(filepath.Base(cur), "sidecar_data") {
				parent := filepath.Dir(cur)
				if parent != "" && parent != cur {
					return parent
				}
				break
			}
			next := filepath.Dir(cur)
			if next == cur {
				break
			}
			cur = next
		}
	}
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR")); v != "" {
		return v
	}
	return store.ProbeAppDataDir(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
}

// New creates a new Sidecar UI server.
func New(cfg Config) *Server {
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port <= 0 {
		if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT"))); err == nil && p > 0 {
			cfg.Port = p
		}
	}
	if cfg.UIToken == "" {
		cfg.UIToken = strings.TrimSpace(os.Getenv("ANTIGRAVITY_SIDECAR_UI_TOKEN"))
	}
	appData := strings.TrimSpace(cfg.AppDataDir)
	if appData == "" {
		appData = ResolveAppDataFromSidecarEnv()
	}
	storeDir := strings.TrimSpace(cfg.StoreDir)
	if storeDir == "" {
		if v := strings.TrimSpace(os.Getenv("BSWARM_DATA_DIR")); v != "" {
			storeDir = v
		} else if appData != "" {
			storeDir = filepath.Join(appData, "plugin_data", "bswarm")
		} else {
			storeDir = store.ResolveDataDir()
		}
	}
	return &Server{
		Cfg:     cfg,
		Store:   store.New(storeDir),
		AppData: appData,
		API:     agentapi.NewCLITransport(),
	}
}

// Handler returns the HTTP handler for serving and testing.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/node-log", s.handleNodeLog)
	mux.HandleFunc("/api/artifact", s.handleArtifact)
	mux.HandleFunc("/api/action", s.handleAction)
	return s.withAuth(mux)
}

// Start binds to loopback and begins serving until ctx is canceled.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.Cfg.Host, s.Cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.listener = ln
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	actualAddr := ln.Addr().String()
	fmt.Printf("[sidecar] listening on %s\n", actualAddr)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutCtx)
	}()

	err = s.httpSrv.Serve(ln)
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror official SidecarApp contract: GET routes are read-only and open on loopback;
		// state-mutating requests (POST/PUT/DELETE) require ANTIGRAVITY_SIDECAR_UI_TOKEN when set.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && s.Cfg.UIToken != "" {
			if !s.validToken(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validToken(r *http.Request) bool {
	want := s.Cfg.UIToken
	if want == "" {
		return true
	}
	cands := []string{
		strings.TrimSpace(r.Header.Get("X-Sidecar-Token")),
		strings.TrimSpace(r.Header.Get("X-Antigravity-Sidecar-Token")),
		strings.TrimSpace(r.URL.Query().Get("token")),
	}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		cands = append(cands, strings.TrimSpace(auth[7:]))
	}
	for _, got := range cands {
		if len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

type treeSummaryItem struct {
	ID            string    `json:"id"`
	Goal          string    `json:"goal"`
	RepoRoot      string    `json:"repo_root"`
	ActiveNodes   int       `json:"active_nodes"`
	ArchivedCount int       `json:"archived_count"`
	UpdatedAt     time.Time `json:"updated_at"`
	MatchesConv   bool      `json:"matches_conv,omitempty"`
}

type uiNode struct {
	ID             string       `json:"id"`
	Role           string       `json:"role"`
	ParentID       string       `json:"parent_id,omitempty"`
	Depth          int          `json:"depth"`
	State          string       `json:"state"`
	ConversationID string       `json:"conversation_id,omitempty"`
	Model          string       `json:"model,omitempty"`
	Provider       string       `json:"provider,omitempty"`
	ProviderModel  string       `json:"provider_model,omitempty"`
	Pool           string       `json:"pool,omitempty"`
	ConcurrencyKey string       `json:"concurrency_key,omitempty"`
	WorkerPID      int          `json:"worker_pid,omitempty"`
	HasLog         bool         `json:"has_log"`
	WorkspaceMode  string       `json:"workspace_mode"`
	WorkspacePath  string       `json:"workspace_path,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	Owns           []string     `json:"owns,omitempty"`
	TouchedFiles   []string     `json:"touched_files,omitempty"`
	CurrentTaskID  string       `json:"current_task_id,omitempty"`
	CurrentGoal    string       `json:"current_goal,omitempty"`
	TaskState      string       `json:"task_state,omitempty"`
	DependsOn      []string     `json:"depends_on,omitempty"`
	LastSummary    string       `json:"last_summary,omitempty"`
	LastError      string       `json:"last_error,omitempty"`
	InboxUnread    int          `json:"inbox_unread"`
	Artifacts      []string     `json:"artifacts,omitempty"`
	UpdatedAt      time.Time    `json:"updated_at"`
	Children       []*uiNode    `json:"children,omitempty"`
}

type uiFeedItem struct {
	Seq      int       `json:"seq"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Kind     string    `json:"kind"`
	TaskID   string    `json:"task_id,omitempty"`
	Summary  string    `json:"summary"`
	Artifact string    `json:"artifact,omitempty"`
	At       time.Time `json:"at"`
}

type uiProviderItem struct {
	Name             string   `json:"name"`
	URL              string   `json:"url"`
	WireProtocol     string   `json:"wire_protocol"`
	Models           []string `json:"models,omitempty"`
	MaxConcurrency   int      `json:"max_concurrency"`
	ConcurrencyScope string   `json:"concurrency_scope"`
	ActiveSlots      int      `json:"active_slots"`
	IdleTimeout      string   `json:"idle_timeout,omitempty"`
}

type uiPoolItem struct {
	Name    string   `json:"name"`
	Entries []string `json:"entries"`
}

type snapshotResponse struct {
	ServerTime     time.Time          `json:"server_time"`
	StoreDir       string             `json:"store_dir"`
	AppDataDir     string             `json:"app_data_dir"`
	ConversationID string             `json:"conversation_id,omitempty"`
	Trees          []treeSummaryItem  `json:"trees"`
	ActiveTree     *activeTreePayload `json:"active_tree,omitempty"`
	ProvidersFile  string             `json:"providers_file,omitempty"`
	Providers      []uiProviderItem   `json:"providers"`
	Pools          []uiPoolItem       `json:"pools"`
}

type activeTreePayload struct {
	ID            string          `json:"id"`
	Goal          string          `json:"goal"`
	RepoRoot      string          `json:"repo_root"`
	MaxDepth      int             `json:"max_depth"`
	MaxActive     int             `json:"max_active"`
	ActiveCount   int             `json:"active_count"`
	ArchivedCount int             `json:"archived_count"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Roots         []*uiNode       `json:"roots"`
	FlatNodes     []*uiNode       `json:"flat_nodes"`
	Tasks         []*store.Task   `json:"tasks"`
	Feed          []uiFeedItem    `json:"feed"`
	ArchivedNodes []*store.Node   `json:"archived_nodes,omitempty"`
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	convID := strings.TrimSpace(r.URL.Query().Get("conversationId"))
	reqTreeID := strings.TrimSpace(r.URL.Query().Get("treeId"))

	trees := s.listAllTrees(convID)
	selectedID := reqTreeID
	if selectedID == "" && convID != "" {
		if ref, err := s.Store.ResolveConversation(convID); err == nil && ref != nil {
			selectedID = ref.TreeID
		}
	}
	if selectedID == "" {
		for _, item := range trees {
			if item.MatchesConv {
				selectedID = item.ID
				break
			}
		}
	}
	if selectedID == "" && len(trees) > 0 {
		selectedID = trees[0].ID
	}

	resp := snapshotResponse{
		ServerTime:     time.Now().UTC(),
		StoreDir:       s.Store.RootDir,
		AppDataDir:     s.AppData,
		ConversationID: convID,
		Trees:          trees,
		Providers:      []uiProviderItem{},
		Pools:          []uiPoolItem{},
	}

	var activeTree *store.Tree
	if selectedID != "" {
		if t, err := s.Store.LoadTree(selectedID); err == nil {
			activeTree = t
			resp.ActiveTree = s.buildActiveTreePayload(t)
		}
	}

	repoRoot := ""
	if activeTree != nil {
		repoRoot = activeTree.RepoRoot
	}
	if cfg, cfgPath, err := provider.Load(repoRoot, s.Store.RootDir); err == nil && cfg != nil {
		resp.ProvidersFile = cfgPath
		resp.Providers, resp.Pools = buildProvidersView(cfg, activeTree)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) listAllTrees(convID string) []treeSummaryItem {
	treesDir := filepath.Join(s.Store.RootDir, "trees")
	entries, err := os.ReadDir(treesDir)
	if err != nil {
		return []treeSummaryItem{}
	}
	var out []treeSummaryItem
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.Store.LoadTree(e.Name())
		if err != nil || t == nil {
			continue
		}
		matches := false
		if convID != "" {
			for _, n := range t.Nodes {
				if n.ConversationID == convID {
					matches = true
					break
				}
			}
		}
		out = append(out, treeSummaryItem{
			ID:            t.ID,
			Goal:          t.Goal,
			RepoRoot:      t.RepoRoot,
			ActiveNodes:   len(t.Nodes),
			ArchivedCount: t.ArchivedCount,
			UpdatedAt:     t.UpdatedAt,
			MatchesConv:   matches,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MatchesConv != out[j].MatchesConv {
			return out[i].MatchesConv
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func (s *Server) buildActiveTreePayload(t *store.Tree) *activeTreePayload {
	nodeMap := make(map[string]*uiNode, len(t.Nodes))
	ids := make([]string, 0, len(t.Nodes))
	for id := range t.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		n := t.Nodes[id]
		un := &uiNode{
			ID:             n.ID,
			Role:           n.Role,
			ParentID:       n.ParentID,
			Depth:          n.Depth,
			State:          string(n.State),
			ConversationID: n.ConversationID,
			Model:          n.Model,
			Provider:       n.Provider,
			ProviderModel:  n.ProviderModel,
			Pool:           n.Pool,
			ConcurrencyKey: n.ConcurrencyKey,
			WorkerPID:      n.WorkerPID,
			HasLog:         s.resolveNodeLogPath(t, n) != "",
			WorkspaceMode:  string(n.WorkspaceMode),
			WorkspacePath:  n.WorkspacePath,
			Branch:         n.Branch,
			Owns:           n.Owns,
			TouchedFiles:   n.TouchedFiles,
			CurrentTaskID:  n.CurrentTaskID,
			LastError:      n.LastError,
			InboxUnread:    maxInt(0, n.InboxTotalSeq-n.InboxReadSeq),
			Artifacts:      s.listNodeArtifacts(t.ID, n.ID),
			UpdatedAt:      n.UpdatedAt,
		}
		if tk, ok := t.Tasks[n.CurrentTaskID]; ok && tk != nil {
			un.CurrentGoal = tk.Goal
			un.TaskState = string(tk.State)
			un.DependsOn = tk.DependsOn
			un.LastSummary = tk.Summary
		} else {
			// Pick most recent completed task for idle context display.
			var latest *store.Task
			for _, tk := range t.Tasks {
				if tk.AssigneeID == n.ID && (latest == nil || tk.UpdatedAt.After(latest.UpdatedAt)) {
					latest = tk
				}
			}
			if latest != nil {
				un.CurrentGoal = latest.Goal
				un.TaskState = string(latest.State)
				un.LastSummary = latest.Summary
			}
		}
		nodeMap[id] = un
	}

	var roots []*uiNode
	for _, id := range ids {
		un := nodeMap[id]
		if un.ParentID != "" {
			if p, ok := nodeMap[un.ParentID]; ok {
				p.Children = append(p.Children, un)
				continue
			}
		}
		roots = append(roots, un)
	}
	var sortChildren func(list []*uiNode)
	sortChildren = func(list []*uiNode) {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Depth != list[j].Depth {
				return list[i].Depth < list[j].Depth
			}
			if list[i].ID == "root" {
				return true
			}
			if list[j].ID == "root" {
				return false
			}
			return list[i].ID < list[j].ID
		})
		for _, c := range list {
			if len(c.Children) > 0 {
				sortChildren(c.Children)
			}
		}
	}
	sortChildren(roots)

	flat := make([]*uiNode, 0, len(ids))
	for _, id := range ids {
		flat = append(flat, nodeMap[id])
	}

	tasks := make([]*store.Task, 0, len(t.Tasks))
	for _, tk := range t.Tasks {
		tasks = append(tasks, tk)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
	})

	feed := s.collectTreeFeed(t)
	archived := s.collectArchivedNodes(t.ID, 24)

	return &activeTreePayload{
		ID:            t.ID,
		Goal:          t.Goal,
		RepoRoot:      t.RepoRoot,
		MaxDepth:      t.MaxDepth,
		MaxActive:     t.MaxActive,
		ActiveCount:   len(t.Nodes),
		ArchivedCount: t.ArchivedCount,
		UpdatedAt:     t.UpdatedAt,
		Roots:         roots,
		FlatNodes:     flat,
		Tasks:         tasks,
		Feed:          feed,
		ArchivedNodes: archived,
	}
}

func (s *Server) collectTreeFeed(t *store.Tree) []uiFeedItem {
	inboxDir := filepath.Join(s.Store.TreeDir(t.ID), "inbox")
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		return []uiFeedItem{}
	}
	var items []uiFeedItem
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(inboxDir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 16*1024), 512*1024)
		for sc.Scan() {
			var m store.Mail
			if json.Unmarshal(sc.Bytes(), &m) == nil && m.Seq > 0 {
				items = append(items, uiFeedItem{
					Seq:      m.Seq,
					From:     m.From,
					To:       m.To,
					Kind:     m.Kind,
					TaskID:   m.TaskID,
					Summary:  m.Summary,
					Artifact: m.Artifact,
					At:       m.At,
				})
			}
		}
		_ = f.Close()
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].At.Equal(items[j].At) {
			return items[i].At.After(items[j].At)
		}
		return items[i].Seq > items[j].Seq
	})
	if len(items) > 120 {
		items = items[:120]
	}
	return items
}

func (s *Server) collectArchivedNodes(treeID string, limit int) []*store.Node {
	recs, err := s.Store.ReadArchive(treeID, limit*3)
	if err != nil {
		return nil
	}
	var nodes []*store.Node
	for i := len(recs) - 1; i >= 0 && len(nodes) < limit; i-- {
		if recs[i].Kind == "node" && recs[i].Node != nil {
			nodes = append(nodes, recs[i].Node)
		}
	}
	return nodes
}

func (s *Server) listNodeArtifacts(treeID, nodeID string) []string {
	dir := s.Store.ArtifactsDir(treeID, nodeID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func (s *Server) resolveNodeLogPath(t *store.Tree, n *store.Node) string {
	if n == nil {
		return ""
	}
	if n.Provider != "" {
		if n.WorkerLog != "" {
			if _, err := os.Stat(n.WorkerLog); err == nil {
				return n.WorkerLog
			}
		}
		cand := s.Store.GatewayLogPath(t.ID, n.ID)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	if n.ConversationID != "" {
		appData := s.AppData
		if appData == "" {
			appData = t.AppDataDir
		}
		if appData == "" {
			appData = store.ProbeAppDataDir(n.ConversationID)
		}
		if appData != "" {
			cand := filepath.Join(appData, "brain", n.ConversationID, ".system_generated", "logs", "transcript.jsonl")
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
	}
	return ""
}

func (s *Server) handleNodeLog(w http.ResponseWriter, r *http.Request) {
	treeID := strings.TrimSpace(r.URL.Query().Get("treeId"))
	nodeID := strings.TrimSpace(r.URL.Query().Get("nodeId"))
	if treeID == "" || nodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "treeId and nodeId are required"})
		return
	}
	t, err := s.Store.LoadTree(treeID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	n, ok := t.Nodes[nodeID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("node %q not found", nodeID)})
		return
	}
	logPath := s.resolveNodeLogPath(t, n)
	if logPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "source": "none", "lines": []string{}})
		return
	}
	lines, err := readTailLines(logPath, 60, 128*1024)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	source := "transcript"
	if n.Provider != "" {
		source = "gateway_log"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id": nodeID,
		"source":  source,
		"path":    logPath,
		"lines":   lines,
	})
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	treeID := strings.TrimSpace(r.URL.Query().Get("treeId"))
	nodeID := strings.TrimSpace(r.URL.Query().Get("nodeId"))
	name := strings.TrimSpace(r.URL.Query().Get("file"))
	if treeID == "" || nodeID == "" || name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "treeId, nodeId, and file are required"})
		return
	}
	// Strictly prevent path traversal outside the node's artifacts dir.
	cleanName := filepath.Base(name)
	if cleanName != name || cleanName == "." || cleanName == ".." || strings.ContainsAny(name, `/\`) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid artifact filename"})
		return
	}
	baseDir := s.Store.ArtifactsDir(treeID, nodeID)
	target := filepath.Join(baseDir, cleanName)
	raw, err := os.ReadFile(target)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	truncated := false
	const maxBytes = 64 * 1024
	if len(raw) > maxBytes {
		raw = raw[:maxBytes]
		truncated = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":   nodeID,
		"file":      cleanName,
		"path":      target,
		"truncated": truncated,
		"content":   string(raw),
	})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req struct {
		Action  string `json:"action"`
		TreeID  string `json:"treeId"`
		NodeID  string `json:"nodeId"`
		Cascade bool   `json:"cascade"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	tid := strings.TrimSpace(req.TreeID)
	nid := strings.TrimSpace(req.NodeID)
	if tid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "treeId is required"})
		return
	}
	t, err := s.Store.LoadTree(tid)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	rootConv := ""
	if root := t.Nodes["root"]; root != nil {
		rootConv = root.ConversationID
	}
	orch := orchestrator.New(s.Store, s.API, nil)

	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "cancel":
		if nid == "" || nid == "root" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot cancel root node"})
			return
		}
		res, err := orch.Retire(rootConv, tid, nid, true, true)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "archived": res.ArchivedNodes})
	case "retire":
		if nid == "" || nid == "root" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot retire root node"})
			return
		}
		res, err := orch.Retire(rootConv, tid, nid, req.Cascade, false)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "archived": res.ArchivedNodes})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unsupported action %q", req.Action)})
	}
}

func buildProvidersView(cfg *provider.Config, t *store.Tree) ([]uiProviderItem, []uiPoolItem) {
	activeByProv := map[string]int{}
	if t != nil {
		for _, n := range t.Nodes {
			if n.State == store.NodeWorking && n.Provider != "" {
				activeByProv[n.Provider]++
			}
		}
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	provs := make([]uiProviderItem, 0, len(names))
	for _, name := range names {
		sp := cfg.Providers[name]
		if sp == nil {
			continue
		}
		scope := sp.ConcurrencyScope
		if scope == "" {
			scope = "provider"
		}
		provs = append(provs, uiProviderItem{
			Name:             name,
			URL:              sp.URL,
			WireProtocol:     sp.WireProtocol,
			Models:           sp.Models,
			MaxConcurrency:   sp.MaxConcurrency,
			ConcurrencyScope: scope,
			ActiveSlots:      activeByProv[name],
			IdleTimeout:      sp.IdleTimeout,
		})
	}
	poolNames := make([]string, 0, len(cfg.Pools))
	for name := range cfg.Pools {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)
	pools := make([]uiPoolItem, 0, len(poolNames))
	for _, pn := range poolNames {
		pools = append(pools, uiPoolItem{Name: pn, Entries: cfg.Pools[pn]})
	}
	return provs, pools
}

func readTailLines(path string, maxLines int, maxBytes int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := fi.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	all := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	if start > 0 && len(all) > 1 {
		all = all[1:]
	}
	var nonEmpty []string
	for _, ln := range all {
		if t := strings.TrimSpace(ln); t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}
	if len(nonEmpty) > maxLines {
		nonEmpty = nonEmpty[len(nonEmpty)-maxLines:]
	}
	return nonEmpty, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
