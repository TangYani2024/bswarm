package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TangYani2024/bswarm/internal/provider"
)

func TestReadAppendedLinesNoTornWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte("{\"step_index\":0}\n{\"step_index\":1"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, off := readAppendedLines(p, 0)
	if len(lines) != 1 || lines[0] != `{"step_index":0}` {
		t.Fatalf("expected only complete line, got lines=%v off=%d", lines, off)
	}
	// Finish writing line 2 and confirm offset advances cleanly.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("}\n")
	_ = f.Close()

	lines2, off2 := readAppendedLines(p, off)
	if len(lines2) != 1 || lines2[0] != `{"step_index":1}` || off2 <= off {
		t.Fatalf("expected completed second line, got lines=%v off=%d->%d", lines2, off, off2)
	}
}

// TestRunDesktopLSTurnLiveOptional exercises RunDesktopLSTurn end-to-end against a real
// Antigravity Desktop `language_server` binary pointed to by BSWARM_E2E_DESKTOP_LS
// using a local mock OpenAI SSE server (no network or API keys required).
func TestRunDesktopLSTurnLiveOptional(t *testing.T) {
	lsBin := strings.TrimSpace(os.Getenv("BSWARM_E2E_DESKTOP_LS"))
	if lsBin == "" {
		t.Skip("set BSWARM_E2E_DESKTOP_LS=/path/to/Antigravity/resources/bin/language_server to run live Desktop LS E2E")
	}
	if _, err := os.Stat(lsBin); err != nil {
		t.Fatalf("BSWARM_E2E_DESKTOP_LS not found: %v", err)
	}

	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	isoHome := filepath.Join(tmp, "iso_home")
	artDir := filepath.Join(tmp, "artifacts")
	storeRoot := filepath.Join(tmp, "store")
	_ = os.MkdirAll(ws, 0o755)
	_ = os.MkdirAll(artDir, 0o755)
	_ = os.MkdirAll(storeRoot, 0o755)

	targetFile := filepath.Join(ws, "desktop_e2e_ok.txt")
	var agentTurns atomic.Int32

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []any `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		writeChunk := func(payload map[string]any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}

		// Title generation call (no tools).
		if len(req.Tools) == 0 {
			writeChunk(map[string]any{
				"id": "t1", "object": "chat.completion.chunk", "model": "mock-desktop-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Desktop E2E"}, "finish_reason": nil}},
			})
			writeChunk(map[string]any{
				"id": "t1", "object": "chat.completion.chunk", "model": "mock-desktop-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			})
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}

		n := agentTurns.Add(1)
		switch n {
		case 1:
			argsJSON, _ := json.Marshal(map[string]any{
				"Path":      targetFile,
				"Content":   "desktop-zero-cli-success\n",
				"Overwrite": true,
			})
			writeChunk(map[string]any{
				"id": "c1", "object": "chat.completion.chunk", "model": "mock-desktop-model",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{
						"role": "assistant",
						"tool_calls": []any{map[string]any{
							"index": 0, "id": "call_write_1", "type": "function",
							"function": map[string]any{"name": "write_file", "arguments": string(argsJSON)},
						}},
					},
					"finish_reason": nil,
				}},
			})
			writeChunk(map[string]any{
				"id": "c1", "object": "chat.completion.chunk", "model": "mock-desktop-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			})
		case 2:
			writeChunk(map[string]any{
				"id": "c2", "object": "chat.completion.chunk", "model": "mock-desktop-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "TURN1_DESKTOP_LS_OK"}, "finish_reason": nil}},
			})
			writeChunk(map[string]any{
				"id": "c2", "object": "chat.completion.chunk", "model": "mock-desktop-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			})
		default:
			writeChunk(map[string]any{
				"id": "c3", "object": "chat.completion.chunk", "model": "mock-desktop-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "TURN2_DESKTOP_LS_RESUMED"}, "finish_reason": nil}},
			})
			writeChunk(map[string]any{
				"id": "c3", "object": "chat.completion.chunk", "model": "mock-desktop-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			})
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer mockSrv.Close()

	sandboxOff := false
	spec := &provider.ProviderSpec{
		URL:           mockSrv.URL + "/v1",
		WireProtocol:  "openai",
		APIKey:        "local-test-key",
		Models:        []string{"mock-desktop-model"},
		EnableSandbox: &sandboxOff,
		LSBin:         lsBin,
	}

	if err := PrepareIsolatedHome(isoHome, ws, artDir, storeRoot, "", spec); err != nil {
		t.Fatalf("PrepareIsolatedHome: %v", err)
	}
	env, err := BuildAgyEnv(os.Environ(), isoHome, storeRoot, "t-desk", "w1", "/bin/true", "", spec, "mock-desktop-model")
	if err != nil {
		t.Fatalf("BuildAgyEnv: %v", err)
	}
	backend, err := ResolveWorkerBackend(spec)
	if err != nil {
		t.Fatalf("ResolveWorkerBackend: %v", err)
	}
	if backend.Kind != BackendDesktopLS {
		t.Fatalf("expected BackendDesktopLS when spec.LSBin set, got %+v", backend)
	}

	logPath := filepath.Join(tmp, "worker.jsonl")
	var gotConvID string
	out1 := RunTurn(context.Background(), backend, TurnOpts{
		IsoHome:         isoHome,
		WorkDir:         ws,
		Env:             env,
		Model:           "mock-desktop-model",
		Timeout:         45 * time.Second,
		IdleTimeout:     20 * time.Second,
		ErrorStallGrace: 5 * time.Second,
		Prompt:          "Write desktop_e2e_ok.txt",
		LogPath:         logPath,
		OnConvID:        func(cid string) { gotConvID = cid },
	})
	if out1.ExitErr != nil || out1.Status != "success" || out1.Response != "TURN1_DESKTOP_LS_OK" {
		t.Fatalf("Turn 1 failed: %+v", out1)
	}
	if gotConvID == "" || gotConvID != out1.ConversationID {
		t.Fatalf("expected OnConvID callback, got %q vs %q", gotConvID, out1.ConversationID)
	}
	written, err := os.ReadFile(targetFile)
	if err != nil || strings.TrimSpace(string(written)) != "desktop-zero-cli-success" {
		t.Fatalf("expected write_file tool output at %s, got %q err=%v", targetFile, string(written), err)
	}

	// Turn 2: resume the same conversation ID via send-message!
	out2 := RunTurn(context.Background(), backend, TurnOpts{
		IsoHome:         isoHome,
		WorkDir:         ws,
		Env:             env,
		Model:           "mock-desktop-model",
		ConvID:          gotConvID,
		Timeout:         45 * time.Second,
		IdleTimeout:     20 * time.Second,
		ErrorStallGrace: 5 * time.Second,
		Prompt:          "Follow up turn.",
		LogPath:         logPath,
	})
	if out2.ExitErr != nil || out2.Status != "success" || out2.Response != "TURN2_DESKTOP_LS_RESUMED" {
		t.Fatalf("Turn 2 resume failed: %+v", out2)
	}
}
