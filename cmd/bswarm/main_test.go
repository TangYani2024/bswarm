package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionAndNoUISidecar(t *testing.T) {
	if version != "0.5.0" {
		t.Fatalf("expected version 0.5.0, got %q", version)
	}

	// Capture printUsage stdout and verify ui-sidecar was removed.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	printUsage()
	_ = w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	if strings.Contains(buf.String(), "ui-sidecar") {
		t.Fatalf("printUsage still mentions ui-sidecar:\n%s", buf.String())
	}

	// Verify plugin.json version matches 0.5.0.
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var plug struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &plug); err != nil {
		t.Fatalf("parse plugin.json: %v", err)
	}
	if plug.Version != "0.5.0" {
		t.Fatalf("expected plugin.json version 0.5.0, got %q", plug.Version)
	}
}

func TestParseDurationDaysAndReorderFlags(t *testing.T) {
	if d, err := parseDurationDays("7d"); err != nil || d != 7*24*time.Hour {
		t.Fatalf("7d: got %v, err=%v", d, err)
	}
	if d, err := parseDurationDays("24h"); err != nil || d != 24*time.Hour {
		t.Fatalf("24h: got %v, err=%v", d, err)
	}
	if d, err := parseDurationDays("0"); err != nil || d != 0 {
		t.Fatalf("0: got %v, err=%v", d, err)
	}
	if _, err := parseDurationDays("-3d"); err == nil {
		t.Fatalf("expected error on negative days")
	}
	if _, err := parseDurationDays("-5h"); err == nil {
		t.Fatalf("expected error on negative hours")
	}

	got := reorderFlagsFirst([]string{"--strategy", "squash", "w1", "--verify", "go test ./..."}, map[string]bool{"json": true})
	want := []string{"--strategy", "squash", "--verify", "go test ./...", "w1"}
	if len(got) != len(want) {
		t.Fatalf("reorder len mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reorder[%d]=%q want %q (all=%v)", i, got[i], want[i], got)
		}
	}

	got2 := reorderFlagsFirst([]string{"--cascade", "lead-a"}, map[string]bool{"cascade": true, "json": true})
	if len(got2) != 2 || got2[0] != "--cascade" || got2[1] != "lead-a" {
		t.Fatalf("bool flag reorder mismatch: %v", got2)
	}
}
