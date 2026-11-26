package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TangYani2024/bswarm/internal/store"
)

func TestProviderResolveAndConcurrency(t *testing.T) {
	cfg := &Config{
		Providers: map[string]*ProviderSpec{
			"deepseek": {
				URL:            "https://api.deepseek.com/v1",
				WireProtocol:   "openai",
				DefaultModel:   "deepseek-chat",
				Models:         []string{"deepseek-chat", "deepseek-reasoner"},
				MaxConcurrency: 0, // unlimited normal API
			},
			"hub": {
				URL:              "https://api.example.com/v1",
				WireProtocol:     "openai",
				Models:           []string{"vendor-a/ds", "vendor-b/ds", "vendor-c/df"},
				MaxConcurrency:   1,
				ConcurrencyScope: "prefix:/",
			},
		},
		Pools: map[string][]string{
			"free": {
				"hub:vendor-a/ds",
				"hub:vendor-b/ds",
				"hub:vendor-c/df",
			},
		},
	}

	// 1. Official model passes through untouched.
	res, err := cfg.ResolveTarget("", "gemini-4-hydrogen-high", nil)
	if err != nil {
		t.Fatalf("official resolve: %v", err)
	}
	if res.IsExternal || res.Model != "gemini-4-hydrogen-high" {
		t.Fatalf("unexpected official resolve: %+v", res)
	}

	// 2. Normal unlimited provider (deepseek) never queues.
	active := map[string]*store.Node{
		"w1": {ID: "w1", State: store.NodeWorking, Provider: "deepseek", ProviderModel: "deepseek-chat"},
	}
	res, err = cfg.ResolveTarget("deepseek", "", active)
	if err != nil {
		t.Fatalf("deepseek resolve: %v", err)
	}
	if !res.IsExternal || res.ProviderName != "deepseek" || res.Model != "deepseek-chat" || res.NeedsQueue || res.ConcurrencyKey != "" {
		t.Fatalf("unexpected deepseek resolve: %+v", res)
	}

	// 3. Per-platform prefix:/ concurrency=1 blocks same platform, allows different platform.
	active = map[string]*store.Node{
		"w1": {ID: "w1", State: store.NodeWorking, Provider: "hub", ProviderModel: "vendor-a/ds", ConcurrencyKey: "hub:vendor-a"},
	}
	samePlat, err := cfg.ResolveTarget("", "hub:vendor-a/glm", active)
	if err != nil {
		t.Fatalf("samePlat: %v", err)
	}
	if !samePlat.NeedsQueue || samePlat.ConcurrencyKey != "hub:vendor-a" {
		t.Fatalf("expected same platform to queue, got %+v", samePlat)
	}

	otherPlat, err := cfg.ResolveTarget("", "hub:vendor-b/ds", active)
	if err != nil {
		t.Fatalf("otherPlat: %v", err)
	}
	if otherPlat.NeedsQueue || otherPlat.ConcurrencyKey != "hub:vendor-b" {
		t.Fatalf("expected different platform to run immediately, got %+v", otherPlat)
	}

	// 4. Pool dynamically skips busy vendor-a/ and selects idle vendor-b/.
	poolPick, err := cfg.ResolveTarget("", "pool:free", active)
	if err != nil {
		t.Fatalf("poolPick: %v", err)
	}
	if poolPick.NeedsQueue || poolPick.Model != "vendor-b/ds" || poolPick.PoolName != "free" {
		t.Fatalf("expected pool to pick vendor-b/ds, got %+v", poolPick)
	}

	// 5. Pool queues when every platform is busy.
	active["w2"] = &store.Node{ID: "w2", State: store.NodeWorking, Provider: "hub", ProviderModel: "vendor-b/ds", ConcurrencyKey: "hub:vendor-b"}
	active["w3"] = &store.Node{ID: "w3", State: store.NodeWorking, Provider: "hub", ProviderModel: "vendor-c/df", ConcurrencyKey: "hub:vendor-c"}
	poolFull, err := cfg.ResolveTarget("", "pool:free", active)
	if err != nil {
		t.Fatalf("poolFull: %v", err)
	}
	if !poolFull.NeedsQueue || poolFull.PoolName != "free" {
		t.Fatalf("expected full pool to queue, got %+v", poolFull)
	}
}

func TestResolveAPIKeyFromFileLine(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "api")
	if err := os.WriteFile(fp, []byte("https://example.com/v1\nexample-key-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &ProviderSpec{
		URL:        "https://example.com/v1",
		APIKeyFile: fp,
		APIKeyLine: 2,
	}
	k, err := spec.ResolveAPIKey()
	if err != nil {
		t.Fatalf("ResolveAPIKey: %v", err)
	}
	if k != "example-key-123" {
		t.Fatalf("got %q, want example-key-123", k)
	}
}
