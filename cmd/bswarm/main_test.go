package main

import (
	"testing"
	"time"
)

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
