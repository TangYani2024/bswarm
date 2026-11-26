package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TangYani2024/bswarm/internal/provider"
)

func TestBuildAgyEnvStripsHostLSAndInjectsGateway(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/tmp/original-home",
		"ANTIGRAVITY_LS_ADDRESS=localhost:12345",
		"ANTIGRAVITY_CSRF_TOKEN=secret-csrf",
		"ANTIGRAVITY_CONVERSATION_ID=parent-conv",
		"AGY_SOMETHING=old",
		"BSWARM_NODE_ID=root",
	}
	spec := &provider.ProviderSpec{
		URL:          "https://api.example.com/v1",
		WireProtocol: "openai",
		APIKey:       "example-key-unit",
		Models:       []string{"m1", "m2"},
	}
	env, err := BuildAgyEnv(base, "/tmp/iso-home", "/tmp/store", "tree-1", "w1", "/bin/bswarm", "/tmp/providers.json", spec, "m1")
	if err != nil {
		t.Fatalf("BuildAgyEnv: %v", err)
	}
	kv := map[string]string{}
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		kv[e[:i]] = e[i+1:]
	}
	for _, forbidden := range []string{"ANTIGRAVITY_LS_ADDRESS", "ANTIGRAVITY_CSRF_TOKEN", "ANTIGRAVITY_CONVERSATION_ID", "AGY_SOMETHING"} {
		if _, ok := kv[forbidden]; ok {
			t.Fatalf("expected %s to be stripped", forbidden)
		}
	}
	if kv["HOME"] != "/tmp/iso-home" {
		t.Fatalf("HOME=%q", kv["HOME"])
	}
	if kv["BSWARM_TREE_ID"] != "tree-1" || kv["BSWARM_NODE_ID"] != "w1" || kv["BSWARM_DATA_DIR"] != "/tmp/store" || kv["BSWARM_IN_GATEWAY_WORKER"] != "1" {
		t.Fatalf("missing BSWARM routing env: %+v", kv)
	}
	if kv["AGY_LLM_GATEWAY_URL"] != "https://api.example.com/v1" || kv["AGY_LLM_GATEWAY_WIRE_PROTOCOL"] != "openai" {
		t.Fatalf("missing gateway env: %+v", kv)
	}
}

func TestPrepareIsolatedHomeMountsPluginHooks(t *testing.T) {
	tmp := t.TempDir()
	iso := filepath.Join(tmp, "iso_home")
	fakePlug := filepath.Join(tmp, "bswarm")
	if err := os.MkdirAll(fakePlug, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(fakePlug, "plugin.json"), []byte(`{"name":"bswarm"}`), 0o644)
	_ = os.WriteFile(filepath.Join(fakePlug, "hooks.json"), []byte(`{}`), 0o644)

	spec := &provider.ProviderSpec{
		URL:          "https://api.example.com/v1",
		WireProtocol: "openai",
	}
	if err := PrepareIsolatedHome(iso, "/ws/proj", "/ws/art", "/ws/store", fakePlug, spec); err != nil {
		t.Fatalf("PrepareIsolatedHome: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(iso, ".gemini", "antigravity-cli", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	if doc["enableTerminalSandbox"] != true {
		t.Fatalf("expected enableTerminalSandbox=true, got %v", doc["enableTerminalSandbox"])
	}
	// Verify plugin symlink exists inside isolated HOME.
	mounted := filepath.Join(iso, ".gemini", "config", "plugins", "bswarm", "hooks.json")
	if _, err := os.Stat(mounted); err != nil {
		t.Fatalf("expected plugin hooks mounted at %s: %v", mounted, err)
	}
	// Verify Desktop language_server config.json is also generated with plugin enabled & auto-execution policy.
	deskRaw, err := os.ReadFile(filepath.Join(iso, ".gemini", "config", "config.json"))
	if err != nil {
		t.Fatalf("read desktop config.json: %v", err)
	}
	var deskDoc map[string]any
	if err := json.Unmarshal(deskRaw, &deskDoc); err != nil {
		t.Fatalf("parse desktop config.json: %v", err)
	}
	us, _ := deskDoc["userSettings"].(map[string]any)
	if us["autoExecutionPolicy"] != "CASCADE_COMMANDS_AUTO_EXECUTION_EAGER" {
		t.Fatalf("expected CASCADE_COMMANDS_AUTO_EXECUTION_EAGER in desktop config.json, got %#v", us)
	}
	plugs, _ := deskDoc["plugins"].(map[string]any)
	if plugs["bswarm"] == nil {
		t.Fatalf("expected bswarm enabled in desktop config.json, got %#v", plugs)
	}
}

func TestRunAgyTurnIdleTimeoutAndErrorEarlyStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell mock script test runs on unix")
	}
	tmp := t.TempDir()

	// 1. Mock agy that prints init and then hangs silently -> must be killed by IdleTimeout.
	hangScript := filepath.Join(tmp, "mock-agy-hang.sh")
	_ = os.WriteFile(hangScript, []byte("#!/bin/sh\necho '{\"event\":\"init\",\"conversation_id\":\"conv-hang-1\"}'\nsleep 10\n"), 0o755)

	t0 := time.Now()
	var gotCID string
	out := RunAgyTurn(context.Background(), TurnOpts{
		AgyBin:          hangScript,
		WorkDir:         tmp,
		Env:             os.Environ(),
		Model:           "test-m",
		Timeout:         30 * time.Second,
		IdleTimeout:     250 * time.Millisecond,
		ErrorStallGrace: 200 * time.Millisecond,
		Prompt:          "hi",
		LogPath:         filepath.Join(tmp, "hang.jsonl"),
		OnConvID:        func(c string) { gotCID = c },
	})
	elapsed := time.Since(t0)
	if gotCID != "conv-hang-1" {
		t.Fatalf("expected conv-hang-1 callback, got %q", gotCID)
	}
	if !strings.Contains(out.EarlyStopReason, "stream idle timeout") {
		t.Fatalf("expected stream idle timeout early stop, got %+v", out)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("idle timeout took too long: %v", elapsed)
	}

	// 2. Mock agy that emits step_type=error_message and hangs -> must be early-stopped by ErrorStallGrace.
	errScript := filepath.Join(tmp, "mock-agy-err.sh")
	_ = os.WriteFile(errScript, []byte("#!/bin/sh\necho '{\"event\":\"init\",\"conversation_id\":\"conv-err-1\"}'\necho '{\"event\":\"step_update\",\"step_update\":{\"step_type\":\"error_message\",\"state\":\"DONE\",\"error\":\"upstream 502\"}}'\nsleep 10\n"), 0o755)

	t1 := time.Now()
	out2 := RunAgyTurn(context.Background(), TurnOpts{
		AgyBin:          errScript,
		WorkDir:         tmp,
		Env:             os.Environ(),
		Model:           "test-m",
		Timeout:         30 * time.Second,
		IdleTimeout:     5 * time.Second,
		ErrorStallGrace: 200 * time.Millisecond,
		Prompt:          "hi",
		LogPath:         filepath.Join(tmp, "err.jsonl"),
	})
	elapsed2 := time.Since(t1)
	if !strings.Contains(out2.EarlyStopReason, "upstream error_message") {
		t.Fatalf("expected upstream error_message early stop, got %+v", out2)
	}
	if elapsed2 > 4*time.Second {
		t.Fatalf("error early-stop took too long: %v", elapsed2)
	}
}

func TestResolveAgyBinaryPrefersAgyOverDesktopLanguageServer(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("BSWARM_AGY_BIN", "")
	t.Setenv("HOME", tmp)
	t.Setenv("ANTIGRAVITY_AGENTAPI_EXE", filepath.Join(tmp, "resources", "bin", "language_server"))

	// 1. Desktop-only (no `agy` on PATH or ~/.local/bin): must reject language_server with clear guidance.
	t.Setenv("PATH", tmp)
	if got, err := ResolveAgyBinary(nil); err == nil {
		t.Fatalf("expected error when only Desktop language_server exists, got %q", got)
	} else if !strings.Contains(err.Error(), "language_server") || !strings.Contains(err.Error(), "agy") {
		t.Fatalf("expected actionable Desktop error, got: %v", err)
	}

	// 2. Desktop + CLI installed: must pick real `agy` ahead of ANTIGRAVITY_AGENTAPI_EXE.
	fakeAgy := filepath.Join(tmp, "agy")
	if runtime.GOOS == "windows" {
		fakeAgy += ".exe"
	}
	if err := os.WriteFile(fakeAgy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveAgyBinary(nil)
	if err != nil {
		t.Fatalf("ResolveAgyBinary failed when agy on PATH: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(fakeAgy) {
		t.Fatalf("expected %q, got %q", fakeAgy, got)
	}
}

func TestResolveWorkerBackendFallsBackToDesktopLS(t *testing.T) {
	tmp := t.TempDir()
	fakeLS := filepath.Join(tmp, "language_server")
	if runtime.GOOS == "windows" {
		fakeLS += ".exe"
	}
	if err := os.WriteFile(fakeLS, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BSWARM_AGY_BIN", "")
	t.Setenv("BSWARM_LS_BIN", "")
	t.Setenv("BSWARM_GATEWAY_BACKEND", "")
	t.Setenv("HOME", tmp)
	t.Setenv("PATH", tmp)
	t.Setenv("ANTIGRAVITY_AGENTAPI_EXE", fakeLS)

	// 1. Desktop-only (no `agy` installed): ResolveWorkerBackend seamlessly selects desktop-ls!
	b, err := ResolveWorkerBackend(nil)
	if err != nil {
		t.Fatalf("ResolveWorkerBackend failed on Desktop-only env: %v", err)
	}
	if b.Kind != BackendDesktopLS || filepath.Clean(b.Binary) != filepath.Clean(fakeLS) {
		t.Fatalf("expected desktop-ls %q, got %+v", fakeLS, b)
	}

	// 2. Even when `agy` exists, BSWARM_GATEWAY_BACKEND=desktop-ls forces Desktop LS backend.
	fakeAgy := filepath.Join(tmp, "agy")
	if runtime.GOOS == "windows" {
		fakeAgy += ".exe"
	}
	_ = os.WriteFile(fakeAgy, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("BSWARM_GATEWAY_BACKEND", "desktop-ls")
	b2, err := ResolveWorkerBackend(nil)
	if err != nil || b2.Kind != BackendDesktopLS {
		t.Fatalf("expected forced desktop-ls backend, got %+v err=%v", b2, err)
	}
}
