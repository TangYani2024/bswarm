package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func readJSONLLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(ln), &doc); err != nil {
			t.Fatalf("unmarshal line %q: %v", ln, err)
		}
		out = append(out, doc)
	}
	return out
}

func TestTranscriptMirrorLifecycleAndFinishStates(t *testing.T) {
	tmp := t.TempDir()
	hostAppData := filepath.Join(tmp, "host-appdata")
	convID := "conv-mirror-1"

	// 1. Lazy initialization until convID is known.
	m := newTranscriptMirror(hostAppData, "", "")
	if m.Path() != "" {
		t.Fatalf("expected empty path before convID is set, got %q", m.Path())
	}
	m.WriteUserInput("ignored before convID")

	m.SetConvID(convID)
	wantPath := filepath.Join(hostAppData, "brain", convID, ".system_generated", "logs", "transcript.jsonl")
	if m.Path() != wantPath {
		t.Fatalf("Path()=%q want %q", m.Path(), wantPath)
	}

	m.WriteUserInput("Implement feature X")
	m.WriteUserInput("Duplicate user input must be skipped")
	m.WriteStreamStep("run_command", "RUNNING", "", "run_command", "Running go test")
	m.WriteStreamStep("run_command", "DONE", "", "run_command", "Running go test")
	// Last step has tool_calls, so Finish(success) MUST write a final tool-less PLANNER_RESPONSE DONE.
	m.Finish(TurnOutcome{ConversationID: convID, Status: "success", Response: "Feature X complete"})

	lines := readJSONLLines(t, wantPath)
	if len(lines) != 4 {
		t.Fatalf("expected 4 transcript lines, got %d: %+v", len(lines), lines)
	}
	if lines[0]["type"] != "USER_INPUT" || lines[0]["status"] != "DONE" || int(lines[0]["step_index"].(float64)) != 0 {
		t.Fatalf("unexpected USER_INPUT line: %+v", lines[0])
	}
	if lines[1]["type"] != "PLANNER_RESPONSE" || lines[1]["status"] != "RUNNING" || lines[1]["thinking"] != "**Running go test**" {
		t.Fatalf("unexpected running tool line: %+v", lines[1])
	}
	tc, _ := lines[1]["tool_calls"].([]any)
	if len(tc) != 1 {
		t.Fatalf("expected tool_calls on line 1, got %+v", lines[1])
	}
	if lines[3]["type"] != "PLANNER_RESPONSE" || lines[3]["status"] != "DONE" || lines[3]["content"] != "Feature X complete" {
		t.Fatalf("unexpected final PLANNER_RESPONSE: %+v", lines[3])
	}
	if _, hasTools := lines[3]["tool_calls"]; hasTools {
		t.Fatalf("final PLANNER_RESPONSE must be tool-less, got %+v", lines[3])
	}

	// 2. Resuming an existing transcript continues step_index cleanly and handles error finish.
	m2 := newTranscriptMirror(hostAppData, "", convID)
	m2.WriteUserInput("Follow up turn")
	m2.Finish(TurnOutcome{ConversationID: convID, Status: "error", ResultError: "upstream 503"})
	// Second Finish call must not duplicate ERROR_MESSAGE.
	m2.Finish(TurnOutcome{ConversationID: convID, Status: "error", ResultError: "upstream 503"})

	lines2 := readJSONLLines(t, wantPath)
	if len(lines2) != 6 {
		t.Fatalf("expected 6 lines after resume + error finish, got %d: %+v", len(lines2), lines2)
	}
	if int(lines2[4]["step_index"].(float64)) != 4 || lines2[4]["type"] != "USER_INPUT" {
		t.Fatalf("expected resumed USER_INPUT at step_index 4, got %+v", lines2[4])
	}
	if int(lines2[5]["step_index"].(float64)) != 5 || lines2[5]["type"] != "ERROR_MESSAGE" || lines2[5]["error"] != "upstream 503" {
		t.Fatalf("expected ERROR_MESSAGE at step_index 5, got %+v", lines2[5])
	}

	// 3. Same-path deduplication disables mirroring cleanly.
	mSkip := newTranscriptMirror(hostAppData, "", convID)
	mSkip.SetSkipPath(wantPath)
	if mSkip.Path() != "" {
		t.Fatalf("expected SetSkipPath to disable mirroring on identical path, got %q", mSkip.Path())
	}
	mSkip.WriteRawLine(`{"step_index":99,"type":"PLANNER_RESPONSE","status":"DONE"}`)
	if after := readJSONLLines(t, wantPath); len(after) != 6 {
		t.Fatalf("expected no writes when skipPath matches mirror path, got %d lines", len(after))
	}
}

func TestRunAgyTurnMirrorsStreamJSONToHostTranscript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell mock script test runs on unix")
	}
	tmp := t.TempDir()
	hostAppData := filepath.Join(tmp, "host-appdata")

	okScript := filepath.Join(tmp, "mock-agy-ok.sh")
	scriptBody := `#!/bin/sh
echo '{"event":"init","conversation_id":"conv-agy-live"}'
echo '{"event":"step_update","step_update":{"step_type":"run_command","state":"RUNNING","tool_name":"run_command","tool_summary":"Run go test"}}'
echo '{"event":"step_update","step_update":{"step_type":"planner_response","state":"RUNNING","thinking":"Checking results"}}'
echo '{"event":"result","result":{"conversation_id":"conv-agy-live","status":"success","response":"All tests green"}}'
`
	if err := os.WriteFile(okScript, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}

	out := RunAgyTurn(context.Background(), TurnOpts{
		AgyBin:         okScript,
		HostAppDataDir: hostAppData,
		WorkDir:        tmp,
		Env:            os.Environ(),
		Model:          "mock-model",
		Timeout:        10 * time.Second,
		Prompt:         "Verify package",
		LogPath:        filepath.Join(tmp, "agy.jsonl"),
	})
	if out.ExitErr != nil || out.Status != "success" || out.ConversationID != "conv-agy-live" {
		t.Fatalf("RunAgyTurn failed: %+v", out)
	}

	trPath := filepath.Join(hostAppData, "brain", "conv-agy-live", ".system_generated", "logs", "transcript.jsonl")
	lines := readJSONLLines(t, trPath)
	if len(lines) != 4 {
		t.Fatalf("expected 4 mirrored lines (USER_INPUT + 2 steps + final DONE), got %d: %+v", len(lines), lines)
	}
	if lines[0]["type"] != "USER_INPUT" || lines[0]["content"] != "Verify package" {
		t.Fatalf("unexpected line 0: %+v", lines[0])
	}
	if lines[1]["type"] != "PLANNER_RESPONSE" || lines[1]["status"] != "RUNNING" || lines[1]["thinking"] != "**Run go test**" {
		t.Fatalf("unexpected line 1: %+v", lines[1])
	}
	if lines[2]["type"] != "PLANNER_RESPONSE" || lines[2]["thinking"] != "**Checking results**" {
		t.Fatalf("unexpected line 2: %+v", lines[2])
	}
	if lines[3]["type"] != "PLANNER_RESPONSE" || lines[3]["status"] != "DONE" || lines[3]["content"] != "All tests green" {
		t.Fatalf("unexpected final line 3: %+v", lines[3])
	}

	// Error turn mirrors ERROR_MESSAGE.
	errScript := filepath.Join(tmp, "mock-agy-err.sh")
	errBody := `#!/bin/sh
echo '{"event":"init","conversation_id":"conv-agy-err"}'
echo '{"event":"step_update","step_update":{"step_type":"error_message","state":"ERROR","error":"rate limited"}}'
exit 1
`
	if err := os.WriteFile(errScript, []byte(errBody), 0o755); err != nil {
		t.Fatal(err)
	}
	outErr := RunAgyTurn(context.Background(), TurnOpts{
		AgyBin:         errScript,
		HostAppDataDir: hostAppData,
		WorkDir:        tmp,
		Env:            os.Environ(),
		Model:          "mock-model",
		Timeout:        10 * time.Second,
		Prompt:         "Run fail",
		LogPath:        filepath.Join(tmp, "agy-err.jsonl"),
	})
	if outErr.ExitErr == nil {
		t.Fatalf("expected non-zero exit error")
	}
	errTrPath := filepath.Join(hostAppData, "brain", "conv-agy-err", ".system_generated", "logs", "transcript.jsonl")
	errLines := readJSONLLines(t, errTrPath)
	last := errLines[len(errLines)-1]
	if last["type"] != "ERROR_MESSAGE" || last["status"] != "ERROR" || last["error"] != "rate limited" {
		t.Fatalf("expected ERROR_MESSAGE last line, got %+v", last)
	}
}

func TestRunDesktopLSTurnMirrorsIsolatedTranscriptToHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell mock script test runs on unix")
	}
	tmp := t.TempDir()
	isoHome := filepath.Join(tmp, "iso-home")
	hostAppData := filepath.Join(tmp, "host-appdata")
	convID := "conv-desk-mirror-1"

	// Mock ConnectRPC server returning RUNNING once, then IDLE.
	var statusCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statusCalls++
		status := "CASCADE_RUN_STATUS_RUNNING"
		if statusCalls >= 2 {
			status = "CASCADE_RUN_STATUS_IDLE"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"trajectorySummaries": map[string]any{
				convID: map[string]any{
					"status":    status,
					"stepCount": 3,
				},
			},
		})
	}))
	defer srv.Close()

	// Parse srv port to bind our fake language_server HTTP listener or have fake language_server proxy/serve.
	// Simpler: fake language_server starts a tiny python3/socat? Even simpler: let fake language_server
	// serve HTTP via nc/python3 OR just use python3 if available, else skip. Let's check with Go or shell:
	// Actually we can write a 20-line python3 or go? Wait: in a Go test, we can have fake `language_server`
	// exec a tiny helper OR write transcript lines while listening on `--http_server_port` using python3!
	// Wait: let's avoid assuming python3 exists — we can compile our own test binary or use `os.Executable()`
	// with a test-helper env var! Using `os.Executable()` re-exec pattern in Go is 100% pure Go and dependency-free!
	fakeLS := os.Args[0]
	isoTrPath := filepath.Join(isoHome, ".gemini", "antigravity", "brain", convID, ".system_generated", "logs", "transcript.jsonl")

	env := append(os.Environ(),
		"BSWARM_TEST_FAKE_LS=1",
		"BSWARM_TEST_LS_CONV_ID="+convID,
		"BSWARM_TEST_LS_TRANSCRIPT="+isoTrPath,
	)
	_ = srv // keep import clean if unused; wait - fake LS serves ConnectRPC directly!
	srv.Close()

	out := RunDesktopLSTurn(context.Background(), TurnOpts{
		AgyBin:         fakeLS,
		IsoHome:        isoHome,
		HostAppDataDir: hostAppData,
		WorkDir:        tmp,
		Env:            env,
		Model:          "mock-ls-model",
		Timeout:        15 * time.Second,
		IdleTimeout:    5 * time.Second,
		Prompt:         "Run desktop LS turn",
		LogPath:        filepath.Join(tmp, "ls-worker.jsonl"),
	})
	if out.ExitErr != nil || out.Status != "success" || out.Response != "DESKTOP_LS_MIRROR_OK" {
		t.Fatalf("RunDesktopLSTurn failed: %+v", out)
	}

	hostTrPath := filepath.Join(hostAppData, "brain", convID, ".system_generated", "logs", "transcript.jsonl")
	lines := readJSONLLines(t, hostTrPath)
	if len(lines) != 3 {
		t.Fatalf("expected 3 mirrored lines verbatim (USER_INPUT, running tool, final DONE), got %d: %+v", len(lines), lines)
	}
	if lines[0]["type"] != "USER_INPUT" || lines[1]["type"] != "PLANNER_RESPONSE" || lines[2]["type"] != "PLANNER_RESPONSE" || lines[2]["status"] != "DONE" || lines[2]["content"] != "DESKTOP_LS_MIRROR_OK" {
		t.Fatalf("unexpected mirrored lines: %+v", lines)
	}

	// Same-path deduplication: if HostAppDataDir points at isoHome/.gemini/antigravity, never double-append.
	sameAppData := filepath.Join(isoHome, ".gemini", "antigravity")
	out2 := RunDesktopLSTurn(context.Background(), TurnOpts{
		AgyBin:         fakeLS,
		IsoHome:        isoHome,
		HostAppDataDir: sameAppData,
		WorkDir:        tmp,
		Env:            env,
		Model:          "mock-ls-model",
		Timeout:        15 * time.Second,
		IdleTimeout:    5 * time.Second,
		Prompt:         "Same path check",
		LogPath:        filepath.Join(tmp, "ls-worker-2.jsonl"),
	})
	if out2.ExitErr != nil || out2.Status != "success" {
		t.Fatalf("RunDesktopLSTurn same-path failed: %+v", out2)
	}
	sameLines := readJSONLLines(t, isoTrPath)
	if len(sameLines) != 3 {
		t.Fatalf("expected 3 lines without double-append when mirrorPath==trPath, got %d: %+v", len(sameLines), sameLines)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("BSWARM_TEST_FAKE_LS") == "1" {
		runFakeLSProcess()
		return
	}
	os.Exit(m.Run())
}

func runFakeLSProcess() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "agentapi" {
		convID := os.Getenv("BSWARM_TEST_LS_CONV_ID")
		trPath := os.Getenv("BSWARM_TEST_LS_TRANSCRIPT")
		_ = os.MkdirAll(filepath.Dir(trPath), 0o755)
		content := strings.Join([]string{
			`{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"Run desktop LS turn"}`,
			`{"step_index":1,"type":"PLANNER_RESPONSE","status":"RUNNING","thinking":"**Working**","tool_calls":[{"name":"read_file","args":{"toolSummary":"Read file"}}]}`,
			`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"DESKTOP_LS_MIRROR_OK"}`,
		}, "\n") + "\n"
		_ = os.WriteFile(trPath, []byte(content), 0o600)
		fmt.Printf(`{"conversation_id":%q}`, convID)
		return
	}

	port := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--http_server_port" {
			port = args[i+1]
		}
	}
	mux := http.NewServeMux()
	convID := os.Getenv("BSWARM_TEST_LS_CONV_ID")
	var calls int
	mux.HandleFunc("/exa.language_server_pb.LanguageServerService/GetAllCascadeTrajectories", func(w http.ResponseWriter, r *http.Request) {
		calls++
		status := "CASCADE_RUN_STATUS_RUNNING"
		if calls >= 2 {
			status = "CASCADE_RUN_STATUS_IDLE"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"trajectorySummaries": map[string]any{
				convID: map[string]any{
					"status":    status,
					"stepCount": 3,
				},
			},
		})
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	go func() {
		_ = srv.ListenAndServe()
	}()
	// Exit cleanly when stdin closes (--exit_on_stdin_close).
	buf := make([]byte, 64)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			_ = srv.Close()
			return
		}
	}
}
