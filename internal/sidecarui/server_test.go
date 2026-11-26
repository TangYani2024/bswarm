package sidecarui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TangYani2024/bswarm/internal/store"
)

func TestSidecarUISnapshotTreeLogsArtifactAndAuth(t *testing.T) {
	dir := t.TempDir()
	appData := filepath.Join(dir, "antigravity")
	stDir := filepath.Join(appData, "plugin_data", "bswarm")
	st := store.New(stDir)

	now := time.Now().UTC()
	tr := &store.Tree{
		Version: 1, ID: "tree-ui-1", Goal: "Build 3-level hierarchy", RepoRoot: dir, AppDataDir: appData,
		MaxDepth: 3, MaxActive: 12,
		Nodes: map[string]*store.Node{
			"root":   {ID: "root", Role: "coordinator", ConversationID: "conv-root", Depth: 0, State: store.NodeWorking, WorkspaceMode: store.WorkspaceShared, WorkspacePath: dir, CreatedAt: now, UpdatedAt: now},
			"lead-a": {ID: "lead-a", Role: "lead", ParentID: "root", ConversationID: "conv-lead", Depth: 1, State: store.NodeWorking, CurrentTaskID: "t1", WorkspaceMode: store.WorkspaceShared, WorkspacePath: dir, CreatedAt: now, UpdatedAt: now},
			"w-ext":  {ID: "w-ext", Role: "worker", ParentID: "lead-a", Provider: "deepseek", ProviderModel: "deepseek-chat", Model: "deepseek:deepseek-chat", Pool: "fast", Depth: 2, State: store.NodeIdle, CurrentTaskID: "t2", WorkspaceMode: store.WorkspaceShared, WorkspacePath: dir, Owns: []string{"docs/**"}, CreatedAt: now, UpdatedAt: now},
		},
		Tasks: map[string]*store.Task{
			"t1": {ID: "t1", AssigneeID: "lead-a", CreatedBy: "root", Goal: "Coordinate module", State: store.TaskRunning, CreatedAt: now, UpdatedAt: now},
			"t2": {ID: "t2", AssigneeID: "w-ext", CreatedBy: "lead-a", Goal: "Write docs", DependsOn: []string{"t1"}, State: store.TaskDone, Summary: "docs written", CreatedAt: now, UpdatedAt: now},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateTree(tr); err != nil {
		t.Fatal(err)
	}
	// Write a gateway worker log and an artifact file.
	gwLog := st.GatewayLogPath("tree-ui-1", "w-ext")
	_ = os.MkdirAll(filepath.Dir(gwLog), 0o755)
	_ = os.WriteFile(gwLog, []byte("{\"event\":\"init\"}\n{\"type\":\"PLANNER_RESPONSE\",\"content\":\"hello\"}\n"), 0o600)
	artDir := st.ArtifactsDir("tree-ui-1", "w-ext")
	_ = os.MkdirAll(artDir, 0o755)
	_ = os.WriteFile(filepath.Join(artDir, "report.md"), []byte("# Report OK\n"), 0o600)

	srv := New(Config{
		StoreDir:   stDir,
		AppDataDir: appData,
		UIToken:    "secret-ui-tok",
	})
	h := srv.Handler()

	// 1. GET / serves HTML panel.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bswarm") {
		t.Fatalf("GET / failed: code=%d", rec.Code)
	}

	// 2. GET /api/snapshot returns nested 3-level tree (root -> lead-a -> w-ext).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot?conversationId=conv-root", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/snapshot code=%d body=%s", rec.Code, rec.Body.String())
	}
	var snap snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.ActiveTree == nil || len(snap.ActiveTree.Roots) != 1 || snap.ActiveTree.Roots[0].ID != "root" {
		t.Fatalf("expected root node, got %+v", snap.ActiveTree)
	}
	lead := snap.ActiveTree.Roots[0].Children[0]
	if lead.ID != "lead-a" || len(lead.Children) != 1 || lead.Children[0].ID != "w-ext" || !lead.Children[0].HasLog {
		t.Fatalf("expected 3-level hierarchy root->lead-a->w-ext with log, got %+v", lead)
	}

	// 3. GET /api/node-log reads isolated gateway worker log.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/node-log?treeId=tree-ui-1&nodeId=w-ext", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gateway_log") || !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("unexpected node-log: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 4. GET /api/artifact reads valid artifact and blocks path traversal.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifact?treeId=tree-ui-1&nodeId=w-ext&file=report.md", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Report OK") {
		t.Fatalf("unexpected artifact resp: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifact?treeId=tree-ui-1&nodeId=w-ext&file=../active.json", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on path traversal, got %d", rec.Code)
	}

	// 5. POST /api/action enforces UIToken and retires node when authorized.
	body := `{"action":"retire","treeId":"tree-ui-1","nodeId":"w-ext","cascade":true}`
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/action", bytes.NewBufferString(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/action", bytes.NewBufferString(body))
	req.Header.Set("X-Sidecar-Token", "secret-ui-tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d body=%s", rec.Code, rec.Body.String())
	}
	after, _ := st.LoadTree("tree-ui-1")
	if _, exists := after.Nodes["w-ext"]; exists {
		t.Fatalf("expected w-ext to be retired")
	}
}
