package provider

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReloaderHotLoadUpdateResilienceAndDelete(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "providers.json")
	t.Setenv("BSWARM_PROVIDERS_FILE", cfgPath)

	rel := NewReloader("", tmp)

	// 1. Before file exists -> empty official-only config, no error.
	cfg, p, changed, err := rel.Current()
	if err != nil || p != "" || changed || len(cfg.Providers) != 0 {
		t.Fatalf("step1: cfg=%+v p=%q changed=%v err=%v", cfg, p, changed, err)
	}

	// 2. Hot-create providers.json -> picked up immediately.
	v1 := `{"providers":{"ds":{"url":"https://api.deepseek.com/v1","models":["deepseek-chat"]}}}`
	if err := os.WriteFile(cfgPath, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, p, changed, err = rel.Current()
	if err != nil || !changed || p != cfgPath || cfg.Providers["ds"] == nil {
		t.Fatalf("step2 hot-create failed: cfg=%+v p=%q changed=%v err=%v", cfg, p, changed, err)
	}

	// 3. Unchanged check -> no unnecessary reload.
	_, _, changed, err = rel.Current()
	if err != nil || changed {
		t.Fatalf("step3 expected cache hit, changed=%v err=%v", changed, err)
	}

	// 4. Corrupt JSON mid-edit -> keeps previous valid config resiliently!
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(cfgPath, []byte(`{"providers": { BROKEN`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, changed, err = rel.Current()
	if err == nil || changed || cfg.Providers["ds"] == nil {
		t.Fatalf("step4 expected error with preserved good config, cfg=%+v changed=%v err=%v", cfg, changed, err)
	}

	// 5. Hot-update with a second provider -> reloaded immediately.
	time.Sleep(15 * time.Millisecond)
	v2 := `{"providers":{"ds":{"url":"https://api.deepseek.com/v1","models":["deepseek-chat"]},"glm":{"url":"https://api.glm.example/v1","models":["glm-5"]}}}`
	if err := os.WriteFile(cfgPath, []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, changed, err = rel.Current()
	if err != nil || !changed || cfg.Providers["glm"] == nil || len(cfg.Providers) != 2 {
		t.Fatalf("step5 hot-update failed: cfg=%+v changed=%v err=%v", cfg, changed, err)
	}

	// 6. Remove file -> gracefully falls back to empty official-only mode.
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	cfg, p, changed, err = rel.Current()
	if err != nil || !changed || p != "" || len(cfg.Providers) != 0 {
		t.Fatalf("step6 delete fallback failed: cfg=%+v p=%q changed=%v err=%v", cfg, p, changed, err)
	}
}
