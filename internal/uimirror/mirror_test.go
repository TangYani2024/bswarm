package uimirror

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TangYani2024/bswarm/internal/store"
)

func TestMirrorWritesOfficialFilesAndRespectsNativeSwarm(t *testing.T) {
	root := t.TempDir()
	appData := filepath.Join(root, "appdata")
	st := store.New(filepath.Join(root, "data"))

	now := time.Now().UTC()
	tr := &store.Tree{
		Version: 1, ID: "tm1", Goal: "Audit repo", RepoRoot: root, AppDataDir: appData, MaxDepth: 3, MaxActive: 8,
		Nodes: map[string]*store.Node{
			"root":            {ID: "root", Role: "coordinator", ConversationID: "conv-root-11111111", Depth: 0, State: store.NodeWorking, CreatedAt: now, UpdatedAt: now},
			"lead-audit":      {ID: "lead-audit", Role: "lead", ParentID: "root", ConversationID: "conv-lead-22222222", Depth: 1, Model: "gemini-4-hydrogen-high", State: store.NodeWorking, CurrentTaskID: "t1", CreatedAt: now, UpdatedAt: now},
			"lead-audit/core": {ID: "lead-audit/core", Role: "coder", ParentID: "lead-audit", ConversationID: "conv-core-33333333", Depth: 2, Model: "gemini-4-hydrogen-high", State: store.NodeWorking, CurrentTaskID: "t2", CreatedAt: now, UpdatedAt: now},
		},
		Tasks: map[string]*store.Task{
			"t1": {ID: "t1", AssigneeID: "lead-audit", CreatedBy: "root", Goal: "Lead audit", State: store.TaskRunning, CreatedAt: now, UpdatedAt: now},
			"t2": {ID: "t2", AssigneeID: "lead-audit/core", CreatedBy: "lead-audit", Goal: "Audit core", State: store.TaskRunning, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)},
		},
	}

	if err := RebuildBoard(st, tr); err != nil {
		t.Fatalf("RebuildBoard: %v", err)
	}
	sdir := SwarmDir(tr)
	mRaw, err := os.ReadFile(filepath.Join(sdir, "members.json"))
	if err != nil {
		t.Fatalf("read members.json: %v", err)
	}
	var roster officialRoster
	if err := json.Unmarshal(mRaw, &roster); err != nil {
		t.Fatalf("unmarshal members.json: %v", err)
	}
	if _, ok := roster.Members["coordinator"]; !ok {
		t.Fatalf("missing coordinator in mirrored members.json")
	}
	if _, ok := roster.Members["lead-audit-core"]; !ok {
		t.Fatalf("expected sanitized member name lead-audit-core, got keys: %+v", roster.Members)
	}

	bRaw, err := os.ReadFile(filepath.Join(sdir, "board.jsonl"))
	if err != nil || !strings.Contains(string(bRaw), "@lead-audit-core") {
		t.Fatalf("expected @lead-audit-core in board.jsonl, got %q err=%v", string(bRaw), err)
	}

	// Safety check: must NEVER overwrite a native /swarm directory that lacks .bswarm_mirror!
	nativeConv := "conv-native-99999999"
	nativeDir := filepath.Join(appData, "brain", nativeConv, ".system_generated", "swarm")
	_ = os.MkdirAll(nativeDir, 0o755)
	_ = os.WriteFile(filepath.Join(nativeDir, "members.json"), []byte(`{"title":"REAL NATIVE"}`), 0o644)

	trNative := &store.Tree{
		ID: "t-nat", Goal: "Should not overwrite", AppDataDir: appData,
		Nodes: map[string]*store.Node{"root": {ID: "root", ConversationID: nativeConv}},
	}
	if err := SyncRosterAndPlan(st, trNative); err != nil {
		t.Fatalf("SyncRosterAndPlan native: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(nativeDir, "members.json"))
	if string(after) != `{"title":"REAL NATIVE"}` {
		t.Fatalf("mirror overwrote a native swarm members.json!")
	}
}
