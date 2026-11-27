package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TangYani2024/bswarm/internal/agentapi"
)

type lockedLimitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *lockedLimitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rem := b.limit - b.buf.Len()
	if rem > 0 {
		chunk := p
		if len(chunk) > rem {
			chunk = chunk[:rem]
		}
		_, _ = b.buf.Write(chunk)
	}
	return len(p), nil
}

func (b *lockedLimitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}

type lsTrajectorySummary struct {
	Status           string `json:"status"`
	StepCount        int    `json:"stepCount"`
	Summary          string `json:"summary"`
	LastModifiedTime string `json:"lastModifiedTime"`
}

type lsAllTrajectoriesResp struct {
	TrajectorySummaries map[string]lsTrajectorySummary `json:"trajectorySummaries"`
}

type lsTranscriptLine struct {
	StepIndex int    `json:"step_index"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Content   string `json:"content"`
	Error     string `json:"error"`
}

// RunDesktopLSTurn executes one headless external-API turn using Antigravity Desktop's
// bundled `resources/bin/language_server` directly — requiring zero `agy` CLI installation.
//
// Lifecycle:
//  1. Starts an isolated standalone headless language_server on a random loopback HTTP port
//     with AGY_LLM_GATEWAY_* env vars and --exit_on_stdin_close so it exits cleanly on pipe close.
//  2. Dispatches the prompt via `language_server agentapi new-conversation` (or `send-message`
//     when resuming an existing conversation ID).
//  3. Polls local ConnectRPC GetAllCascadeTrajectories and mirrors transcript.jsonl into LogPath,
//     enforcing Timeout, IdleTimeout, ErrorStallGrace, and WAITING_FOR_USER early-stop.
//  4. Closes stdin for graceful exit code 0 shutdown (with process-group kill fallback).
func RunDesktopLSTurn(ctx context.Context, opts TurnOpts) TurnOutcome {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	outcome := TurnOutcome{ConversationID: opts.ConvID}
	mirror := newTranscriptMirror(opts.HostAppDataDir, opts.MirrorTranscriptPath, opts.ConvID)
	defer func() {
		mirror.SetConvID(outcome.ConversationID)
		mirror.Finish(outcome)
	}()

	port, err := allocFreeLoopbackPort()
	if err != nil {
		outcome.ExitErr = fmt.Errorf("allocate loopback port for desktop language_server: %w", err)
		return outcome
	}
	csrf, err := randomHexToken(16)
	if err != nil {
		outcome.ExitErr = fmt.Errorf("generate csrf token: %w", err)
		return outcome
	}

	isoHome := strings.TrimSpace(opts.IsoHome)
	if isoHome == "" {
		isoHome = envValue(opts.Env, "HOME")
	}
	if isoHome == "" {
		isoHome = envValue(opts.Env, "USERPROFILE")
	}

	if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
		outcome.ExitErr = err
		return outcome
	}
	logFile, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		outcome.ExitErr = err
		return outcome
	}
	defer logFile.Close()

	lsEnv := append([]string{}, opts.Env...)
	lsEnv = append(lsEnv, "ANTIGRAVITY_SERVER_CSRF_TOKEN="+csrf)

	lsArgs := []string{
		"--standalone",
		"--headless",
		"--exit_on_stdin_close",
		"--disable_telemetry",
		"--override_ide_name", "antigravity",
		"--subclient_type", "hub",
		"--override_ide_version", "2.35.2",
		"--override_user_agent_name", "antigravity",
		"--app_data_dir", "antigravity",
		"--http_server_port", strconv.Itoa(port),
		"--https_server_port", "0",
	}
	if isoHome != "" {
		lsArgs = append(lsArgs, "--gemini_dir", filepath.Join(isoHome, ".gemini"))
	}
	if strings.TrimSpace(opts.WorkDir) != "" {
		lsArgs = append(lsArgs, "--workspace_id", strings.TrimSpace(opts.WorkDir))
	}

	cmd := exec.CommandContext(runCtx, opts.AgyBin, lsArgs...)
	cmd.Dir = opts.WorkDir
	cmd.Env = lsEnv
	setProcessGroup(cmd)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		outcome.ExitErr = fmt.Errorf("open language_server stdin: %w", err)
		return outcome
	}
	lsOut := &lockedLimitedBuffer{limit: 8192}
	cmd.Stdout = lsOut
	cmd.Stderr = lsOut

	if err := cmd.Start(); err != nil {
		_ = stdinPipe.Close()
		outcome.ExitErr = fmt.Errorf("start desktop language_server: %w", err)
		return outcome
	}

	lsExited := make(chan error, 1)
	go func() {
		lsExited <- cmd.Wait()
	}()

	shutdownLS := func() {
		_ = stdinPipe.Close()
		select {
		case <-lsExited:
			return
		case <-time.After(3 * time.Second):
		}
		if cmd.Process != nil {
			_ = KillProcessGroup(cmd.Process.Pid)
			select {
			case <-lsExited:
				return
			case <-time.After(1500 * time.Millisecond):
			}
			_ = ForceKillProcessGroup(cmd.Process.Pid)
			select {
			case <-lsExited:
			case <-time.After(1 * time.Second):
			}
		}
	}
	defer shutdownLS()

	lsAddr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := waitTCPReady(runCtx, lsAddr, 15*time.Second, lsExited); err != nil {
		outcome.Stderr = lsOut.String()
		outcome.ExitErr = fmt.Errorf("desktop language_server failed to start on %s: %w", lsAddr, err)
		return outcome
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}
	apiEnv := append([]string{}, lsEnv...)
	apiEnv = append(apiEnv,
		"ANTIGRAVITY_LS_ADDRESS="+lsAddr,
		"ANTIGRAVITY_CSRF_TOKEN="+csrf,
	)

	convID := strings.TrimSpace(opts.ConvID)
	baseSteps := 0
	if convID == "" {
		newArgs := []string{"agentapi", "new-conversation"}
		if strings.TrimSpace(opts.Model) != "" {
			newArgs = append(newArgs, "--model="+strings.TrimSpace(opts.Model))
		}
		newArgs = append(newArgs, "--title=bswarm-worker", "--", opts.Prompt)
		out, cerr := runLSAgentAPI(runCtx, opts.AgyBin, opts.WorkDir, apiEnv, newArgs...)
		if cerr != nil {
			outcome.Stderr = lsOut.String()
			outcome.ExitErr = fmt.Errorf("desktop language_server agentapi new-conversation: %w", cerr)
			return outcome
		}
		convID = agentapi.ParseConversationID(out)
		if convID == "" {
			outcome.Stderr = lsOut.String()
			outcome.ExitErr = fmt.Errorf("desktop language_server returned no conversation_id: %s", strings.TrimSpace(string(out)))
			return outcome
		}
		outcome.ConversationID = convID
		if opts.OnConvID != nil {
			opts.OnConvID(convID)
		}
	} else {
		if sum, ok := fetchLSTrajectory(runCtx, httpClient, lsAddr, csrf, convID); ok {
			baseSteps = sum.StepCount
		}
		out, cerr := runLSAgentAPI(runCtx, opts.AgyBin, opts.WorkDir, apiEnv,
			"agentapi", "send-message", "--title=bswarm-assign", "--", convID, opts.Prompt)
		if cerr != nil {
			outcome.Stderr = lsOut.String()
			outcome.ExitErr = fmt.Errorf("desktop language_server agentapi send-message: %w (%s)", cerr, strings.TrimSpace(string(out)))
			return outcome
		}
	}

	initEvt, _ := json.Marshal(map[string]any{
		"event":           "init",
		"conversation_id": convID,
		"backend":         string(BackendDesktopLS),
		"model":           opts.Model,
	})
	_, _ = logFile.Write(append(initEvt, '\n'))

	trPath := ""
	if isoHome != "" {
		trPath = filepath.Join(isoHome, ".gemini", "antigravity", "brain", convID, ".system_generated", "logs", "transcript.jsonl")
	}
	mirror.SetSkipPath(trPath)
	mirror.SetConvID(convID)

	var (
		trOffset       int64
		lastActivityAt = time.Now()
		lastStepCount  = baseSteps
		seenRunning    = false
		idleChecks     = 0
		waitingSince   time.Time
		errorStepAt    time.Time
		lastErrText    string
		hasFinalResp   bool
	)

	// Start transcript offset past existing bytes when resuming an existing conversation.
	if opts.ConvID != "" && trPath != "" {
		if fi, serr := os.Stat(trPath); serr == nil {
			trOffset = fi.Size()
		}
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-runCtx.Done():
			outcome.ExitErr = runCtx.Err()
			outcome.Stderr = lsOut.String()
			return outcome
		case exitErr := <-lsExited:
			outcome.Stderr = lsOut.String()
			if exitErr != nil {
				outcome.ExitErr = fmt.Errorf("desktop language_server exited prematurely: %w", exitErr)
			} else if !hasFinalResp {
				outcome.ExitErr = fmt.Errorf("desktop language_server exited before turn completed")
			}
			return outcome
		case <-ticker.C:
			now := time.Now()

			// 1. Mirror newly appended transcript.jsonl lines into LogPath and inspect events.
			if trPath != "" {
				if lines, newOff := readAppendedLines(trPath, trOffset); newOff > trOffset {
					trOffset = newOff
					lastActivityAt = now
					for _, rawLn := range lines {
						_, _ = logFile.WriteString(rawLn + "\n")
						mirror.WriteRawLine(rawLn)
						var st lsTranscriptLine
						if err := json.Unmarshal([]byte(rawLn), &st); err != nil {
							continue
						}
						switch st.Type {
						case "PLANNER_RESPONSE":
							if strings.TrimSpace(st.Content) != "" {
								outcome.Response = strings.TrimSpace(st.Content)
								hasFinalResp = true
							}
							if strings.EqualFold(st.Status, "DONE") {
								errorStepAt = time.Time{}
							}
						case "ERROR_MESSAGE":
							errMsg := strings.TrimSpace(st.Error)
							if errMsg == "" {
								errMsg = strings.TrimSpace(st.Content)
							}
							if errMsg == "" {
								errMsg = "upstream model error"
							}
							lastErrText = errMsg
							if errorStepAt.IsZero() {
								errorStepAt = now
							}
						default:
							if strings.EqualFold(st.Status, "ERROR") && strings.TrimSpace(st.Error) != "" {
								lastErrText = strings.TrimSpace(st.Error)
							}
						}
					}
				}
			}

			// 2. Poll ConnectRPC trajectory status.
			if sum, ok := fetchLSTrajectory(runCtx, httpClient, lsAddr, csrf, convID); ok {
				if sum.StepCount > lastStepCount {
					lastStepCount = sum.StepCount
					lastActivityAt = now
				}
				switch sum.Status {
				case "CASCADE_RUN_STATUS_RUNNING":
					seenRunning = true
					idleChecks = 0
					waitingSince = time.Time{}
				case "CASCADE_RUN_STATUS_IDLE":
					waitingSince = time.Time{}
					if seenRunning || sum.StepCount > baseSteps || hasFinalResp {
						idleChecks++
						if idleChecks >= 2 || hasFinalResp {
							// One final transcript drain so the last PLANNER_RESPONSE is never missed.
							if trPath != "" {
								if lines, newOff := readAppendedLines(trPath, trOffset); newOff > trOffset {
									trOffset = newOff
									for _, rawLn := range lines {
										_, _ = logFile.WriteString(rawLn + "\n")
										mirror.WriteRawLine(rawLn)
										var st lsTranscriptLine
										if json.Unmarshal([]byte(rawLn), &st) == nil && st.Type == "PLANNER_RESPONSE" && strings.TrimSpace(st.Content) != "" {
											outcome.Response = strings.TrimSpace(st.Content)
										}
									}
								}
							}
							if lastErrText != "" && outcome.Response == "" {
								outcome.Status = "error"
								outcome.ResultError = lastErrText
							} else {
								outcome.Status = "success"
							}
							return outcome
						}
					}
				case "CASCADE_RUN_STATUS_WAITING_FOR_USER":
					if waitingSince.IsZero() {
						waitingSince = now
					} else if now.Sub(waitingSince) >= 10*time.Second {
						outcome.Status = "error"
						outcome.EarlyStopReason = "desktop language_server waiting for interactive approval"
						outcome.ResultError = outcome.EarlyStopReason
						return outcome
					}
				}
			}

			// 3. Guard against upstream error_message hangs.
			if !errorStepAt.IsZero() && opts.ErrorStallGrace > 0 && now.Sub(errorStepAt) >= opts.ErrorStallGrace {
				outcome.Status = "error"
				outcome.EarlyStopReason = fmt.Sprintf("upstream error_message (%s)", lastErrText)
				outcome.ResultError = lastErrText
				return outcome
			}

			// 4. Guard against silent stream/model stalls.
			if opts.IdleTimeout > 0 && now.Sub(lastActivityAt) >= opts.IdleTimeout {
				outcome.Status = "error"
				outcome.EarlyStopReason = fmt.Sprintf("stream idle timeout (%s without progress)", opts.IdleTimeout)
				outcome.ResultError = outcome.EarlyStopReason
				return outcome
			}
		}
	}
}

func allocFreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener addr %T", ln.Addr())
	}
	return addr.Port, nil
}

func randomHexToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func waitTCPReady(ctx context.Context, addr string, timeout time.Duration, exited <-chan error) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-exited:
			if err != nil {
				return fmt.Errorf("process exited early: %w", err)
			}
			return fmt.Errorf("process exited early with status 0")
		default:
		}
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s waiting for listener", timeout)
}

func runLSAgentAPI(ctx context.Context, lsBin, workDir string, env []string, args ...string) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(callCtx, lsBin, args...)
	cmd.Dir = workDir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s", msg)
	}
	return stdout.Bytes(), nil
}

func fetchLSTrajectory(ctx context.Context, client *http.Client, lsAddr, csrf, convID string) (lsTrajectorySummary, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+lsAddr+"/exa.language_server_pb.LanguageServerService/GetAllCascadeTrajectories",
		strings.NewReader("{}"))
	if err != nil {
		return lsTrajectorySummary{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-codeium-csrf-token", csrf)
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := client.Do(req)
	if err != nil {
		return lsTrajectorySummary{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return lsTrajectorySummary{}, false
	}
	var doc lsAllTrajectoriesResp
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return lsTrajectorySummary{}, false
	}
	sum, ok := doc.TrajectorySummaries[convID]
	return sum, ok
}

func readAppendedLines(path string, offset int64) ([]string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= offset {
		return nil, offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil, offset
	}
	// Only consume up to the last complete newline so partial writes are never torn.
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		return nil, offset
	}
	consumed := data[:lastNL+1]
	rawLines := strings.Split(strings.TrimRight(string(consumed), "\n"), "\n")
	var out []string
	for _, ln := range rawLines {
		if t := strings.TrimSpace(ln); t != "" {
			out = append(out, t)
		}
	}
	return out, offset + int64(len(consumed))
}
