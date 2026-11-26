package gc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeAuditDryRunAndApply(t *testing.T) {
	appData := filepath.Join(t.TempDir(), "antigravity")
	coordID := "11111111-1111-1111-1111-111111111111"
	killedID := "22222222-2222-2222-2222-222222222222"
	liveID := "33333333-3333-3333-3333-333333333333"

	swarmDir := filepath.Join(appData, "brain", coordID, ".system_generated", "swarm")
	_ = os.MkdirAll(swarmDir, 0o755)
	_ = os.WriteFile(filepath.Join(swarmDir, "board.jsonl"), []byte(`{"seq":1}`+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(swarmDir, "members.json"), []byte(`{
		"title": "Test Swarm",
		"members": {
			"coordinator": {"conversation_id": "`+coordID+`", "role": "coordinator", "killed": false},
			"dead-w": {"conversation_id": "`+killedID+`", "role": "worker", "killed": true},
			"live-w": {"conversation_id": "`+liveID+`", "role": "worker", "killed": false}
		}
	}`), 0o644)

	// Create leftover brain dirs and .pb files.
	for _, id := range []string{killedID, liveID} {
		bd := filepath.Join(appData, "brain", id)
		_ = os.MkdirAll(bd, 0o755)
		_ = os.WriteFile(filepath.Join(bd, "data.bin"), make([]byte, 4096), 0o644)
		old := time.Now().Add(-10 * 24 * time.Hour)
		_ = os.Chtimes(bd, old, old)
		_ = os.MkdirAll(filepath.Join(appData, "conversations"), 0o755)
		pb := filepath.Join(appData, "conversations", id+".pb")
		_ = os.WriteFile(pb, make([]byte, 2048), 0o644)
		_ = os.Chtimes(pb, old, old)
	}

	// Dry-run must NOT delete anything.
	dry, err := AuditNativeSwarms([]string{appData}, 7*24*time.Hour, false)
	if err != nil {
		t.Fatalf("dry audit: %v", err)
	}
	if dry.TotalKilledDirs != 1 || dry.TotalKilledPBs != 1 || dry.TotalReclaimableBytes != 6144 {
		t.Fatalf("unexpected dry report: %+v", dry)
	}
	if _, err := os.Stat(filepath.Join(appData, "brain", killedID)); err != nil {
		t.Fatalf("dry-run deleted killed dir!")
	}

	// Apply must delete ONLY killedID, keeping liveID and coordinator intact.
	applied, err := AuditNativeSwarms([]string{appData}, 7*24*time.Hour, true)
	if err != nil {
		t.Fatalf("apply audit: %v", err)
	}
	if applied.TotalRemovedBytes != 6144 {
		t.Fatalf("expected 6144 removed bytes, got %d", applied.TotalRemovedBytes)
	}
	if _, err := os.Stat(filepath.Join(appData, "brain", killedID)); !os.IsNotExist(err) {
		t.Fatalf("expected killed brain dir removed")
	}
	if _, err := os.Stat(filepath.Join(appData, "brain", liveID)); err != nil {
		t.Fatalf("live worker brain dir was wrongly removed!")
	}
	if _, err := os.Stat(filepath.Join(appData, "brain", coordID)); err != nil {
		t.Fatalf("coordinator brain dir was wrongly removed!")
	}
}
