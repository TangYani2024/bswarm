package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TangYani2024/bswarm/internal/provider"
	"github.com/TangYani2024/bswarm/internal/store"
)

// LaunchRequest carries everything needed to launch or resume an external gateway turn.
type LaunchRequest struct {
	TreeID        string
	NodeID        string
	TaskID        string
	Prompt        string
	ProvidersFile string
}

// TurnOutcome summarizes one completed headless `agy --print --output-format stream-json` turn.
type TurnOutcome struct {
	ConversationID  string
	Status          string
	Response        string
	ResultError     string
	EarlyStopReason string
	DeniedActions   []string
	Stderr          string
	ExitErr         error
}

// TurnOpts configures one headless `agy` gateway worker turn.
type TurnOpts struct {
	AgyBin          string
	WorkDir         string
	Env             []string
	Model           string
	ConvID          string
	Mode            string
	Timeout         time.Duration
	IdleTimeout     time.Duration
	ErrorStallGrace time.Duration
	Prompt          string
	LogPath         string
	OnConvID        func(string)
}

// ResolveAgyBinary finds the agy binary to run for headless gateway workers.
func ResolveAgyBinary(spec *provider.ProviderSpec) (string, error) {
	if spec != nil && strings.TrimSpace(spec.AgyBin) != "" {
		return strings.TrimSpace(spec.AgyBin), nil
	}
	if v := strings.TrimSpace(os.Getenv("BSWARM_AGY_BIN")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE")); v != "" {
		return v, nil
	}
	if p, err := exec.LookPath("agy"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		cand := filepath.Join(home, ".local", "bin", "agy")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return "", errors.New("could not locate `agy` binary (set BSWARM_AGY_BIN or provider.agy_bin)")
}

// ResolvePluginDir locates the bswarm plugin directory containing plugin.json + hooks.json
// so isolated gateway workers can mount the exact same PreToolUse (--owns) and Stop hooks.
func ResolvePluginDir(exePath string) string {
	candidates := []string{}
	if strings.TrimSpace(exePath) != "" {
		resolved := exePath
		if r, err := filepath.EvalSymlinks(exePath); err == nil {
			resolved = r
		}
		candidates = append(candidates, filepath.Dir(filepath.Dir(resolved)))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".gemini", "config", "plugins", "bswarm"))
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if _, err1 := os.Stat(filepath.Join(dir, "plugin.json")); err1 == nil {
			if _, err2 := os.Stat(filepath.Join(dir, "hooks.json")); err2 == nil {
				return dir
			}
		}
	}
	return ""
}

// PrepareIsolatedHome creates a per-node HOME directory so the gateway `agy` subprocess
// has its own `.gemini/antigravity-cli` state and never collides with the host session.
// If pluginDir is non-empty, it symlinks the plugin into `.gemini/config/plugins/` so the
// isolated gateway worker enforces the same `--owns` and `Stop` hooks as official workers.
func PrepareIsolatedHome(isoHome, workspacePath, artifactsDir, storeRoot, pluginDir string, spec *provider.ProviderSpec) error {
	cliDir := filepath.Join(isoHome, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o700); err != nil {
		return fmt.Errorf("create isolated agy dir: %w", err)
	}

	// Best-effort read-only symlink of host .gitconfig so worktree commits preserve git identity.
	if realHome, err := os.UserHomeDir(); err == nil && realHome != "" && realHome != isoHome {
		hostGitCfg := filepath.Join(realHome, ".gitconfig")
		dstGitCfg := filepath.Join(isoHome, ".gitconfig")
		if _, serr := os.Stat(hostGitCfg); serr == nil {
			if _, lerr := os.Lstat(dstGitCfg); os.IsNotExist(lerr) {
				_ = os.Symlink(hostGitCfg, dstGitCfg)
			}
		}
	}

	// Mount bswarm plugin into isolated HOME so PreToolUse (--owns) & Stop hooks fire.
	if strings.TrimSpace(pluginDir) != "" {
		plugParent := filepath.Join(isoHome, ".gemini", "config", "plugins")
		if err := os.MkdirAll(plugParent, 0o700); err == nil {
			dst := filepath.Join(plugParent, filepath.Base(pluginDir))
			if _, lerr := os.Lstat(dst); os.IsNotExist(lerr) {
				_ = os.Symlink(pluginDir, dst)
			}
		}
	}

	trusted := uniqueNonEmpty(workspacePath, artifactsDir, storeRoot)
	settings := map[string]any{
		"trustedWorkspaces":       trusted,
		"enableTerminalSandbox":   spec.EffectiveSandbox(),
		"sandboxAllowNetwork":     spec.SandboxAllowNet,
		"allowNonWorkspaceAccess": true,
		"permissions": map[string]any{
			"allow": spec.EffectivePermissions(),
		},
	}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(filepath.Join(cliDir, "settings.json"), raw, 0o600)
}

// BuildAgyEnv constructs a clean isolated environment for the gateway `agy` subprocess.
// Host ANTIGRAVITY_LS_* credentials are stripped so the worker uses its own gateway backend,
// while BSWARM_* routing vars point back to the shared bswarm store.
func BuildAgyEnv(baseEnv []string, isoHome, storeRoot, treeID, nodeID, bswarmBin, providersFile string, spec *provider.ProviderSpec, model string) ([]string, error) {
	apiKey, err := spec.ResolveAPIKey()
	if err != nil {
		return nil, fmt.Errorf("resolve api key: %w", err)
	}
	filtered := make([]string, 0, len(baseEnv)+16)
	for _, kv := range baseEnv {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := kv[:eq]
		if k == "HOME" ||
			strings.HasPrefix(k, "ANTIGRAVITY_") ||
			strings.HasPrefix(k, "AGY_") ||
			strings.HasPrefix(k, "JETSKI_") ||
			strings.HasPrefix(k, "BSWARM_") {
			continue
		}
		filtered = append(filtered, kv)
	}

	models := uniqueNonEmpty(append([]string{model}, spec.Models...)...)
	filtered = append(filtered,
		"HOME="+isoHome,
		"BSWARM_DATA_DIR="+storeRoot,
		"BSWARM_TREE_ID="+treeID,
		"BSWARM_NODE_ID="+nodeID,
		"BSWARM_BIN="+bswarmBin,
		"BSWARM_IN_GATEWAY_WORKER=1",
		"AGY_LLM_GATEWAY_URL="+strings.TrimSpace(spec.URL),
		"AGY_LLM_GATEWAY_WIRE_PROTOCOL="+spec.WireProtocol,
		"AGY_LLM_GATEWAY_MODELS="+strings.Join(models, ","),
	)
	if apiKey != "" {
		filtered = append(filtered, "AGY_LLM_GATEWAY_API_KEY="+apiKey)
	}
	if strings.TrimSpace(providersFile) != "" {
		filtered = append(filtered, "BSWARM_PROVIDERS_FILE="+strings.TrimSpace(providersFile))
	}
	if strings.TrimSpace(spec.ProxyURL) != "" {
		filtered = append(filtered, "AGY_LLM_GATEWAY_PROXY_URL="+strings.TrimSpace(spec.ProxyURL))
	}
	if strings.TrimSpace(spec.CACert) != "" {
		filtered = append(filtered, "AGY_LLM_GATEWAY_CA_CERT="+strings.TrimSpace(spec.CACert))
	}
	if len(spec.Headers) > 0 {
		if hb, herr := json.Marshal(spec.Headers); herr == nil {
			filtered = append(filtered, "AGY_LLM_GATEWAY_HEADERS="+string(hb))
		}
	}
	return filtered, nil
}

// LaunchDetached writes the prompt to disk and starts a detached `bswarm _gw-run` supervisor
// so `spawn` and `assign` return immediately without blocking the caller.
func LaunchDetached(st *store.Store, req LaunchRequest) error {
	if st == nil {
		return errors.New("store is nil")
	}
	promptPath := st.GatewayPromptPath(req.TreeID, req.NodeID, req.TaskID)
	if err := store.WriteAtomic(promptPath, []byte(req.Prompt), 0o600); err != nil {
		return fmt.Errorf("write gateway prompt: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve bswarm executable: %w", err)
	}
	args := []string{
		"_gw-run",
		"--tree", req.TreeID,
		"--node", req.NodeID,
		"--task", req.TaskID,
		"--prompt-file", promptPath,
	}
	if strings.TrimSpace(req.ProvidersFile) != "" {
		args = append(args, "--providers", strings.TrimSpace(req.ProvidersFile))
	}
	cmd := exec.Command(exe, args...)
	// Supervisor keeps host env in memory (so it can ring the parent's doorbell on completion),
	// while stripping host LS creds before spawning the actual gateway `agy` worker.
	env := os.Environ()
	env = append(env, "BSWARM_DATA_DIR="+st.RootDir, "BSWARM_BIN="+exe)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	setDetachedAttr(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start gateway supervisor: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}

// RunAgyTurn executes one headless `agy --output-format stream-json --print` turn,
// streaming raw NDJSON to logPath, firing OnConvID as soon as the init event arrives,
// and actively guarding against stream idle stalls and fatal upstream error_message hangs.
func RunAgyTurn(ctx context.Context, opts TurnOpts) TurnOutcome {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	args := []string{
		"--output-format", "stream-json",
		"--model", opts.Model,
		"--print-timeout", timeout.String(),
	}
	if strings.TrimSpace(opts.Mode) != "" {
		args = append(args, "--mode", strings.TrimSpace(opts.Mode))
	}
	if strings.TrimSpace(opts.ConvID) != "" {
		args = append(args, "--conversation", strings.TrimSpace(opts.ConvID))
	}
	args = append(args, "--print", opts.Prompt)

	cmd := exec.CommandContext(runCtx, opts.AgyBin, args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = opts.Env
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return TurnOutcome{ExitErr: err}
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 8192}

	if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
		return TurnOutcome{ExitErr: err}
	}
	logFile, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return TurnOutcome{ExitErr: err}
	}
	defer logFile.Close()

	if err := cmd.Start(); err != nil {
		return TurnOutcome{ExitErr: err}
	}

	var (
		mu              sync.Mutex
		outcome         = TurnOutcome{ConversationID: opts.ConvID}
		lastEventAt     = time.Now()
		errorStepAt     time.Time
		earlyStopReason string
		stoppedEarly    bool
	)

	// Reader goroutine consumes stream-json lines and updates heartbeat/error state.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			_, _ = logFile.Write(line)
			_, _ = logFile.Write([]byte("\n"))
			now := time.Now()

			mu.Lock()
			lastEventAt = now
			evKind := parseStreamLine(line, &outcome, opts.OnConvID)
			switch evKind {
			case streamEventErrorStep:
				if errorStepAt.IsZero() {
					errorStepAt = now
				}
			case streamEventProgress:
				errorStepAt = time.Time{}
			}
			mu.Unlock()
		}
	}()

	// Stream watchdog goroutine detects idle stalls and hung error_message steps early.
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		interval := time.Second
		if opts.IdleTimeout > 0 && opts.IdleTimeout/4 < interval {
			interval = opts.IdleTimeout / 4
		}
		if opts.ErrorStallGrace > 0 && opts.ErrorStallGrace/4 < interval {
			interval = opts.ErrorStallGrace / 4
		}
		if interval < 25*time.Millisecond {
			interval = 25 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-watchStop:
				return
			case <-runCtx.Done():
				if cmd.Process != nil {
					_ = KillProcessGroup(cmd.Process.Pid)
				}
				return
			case now := <-ticker.C:
				var reason string
				mu.Lock()
				if opts.ErrorStallGrace > 0 && !errorStepAt.IsZero() && now.Sub(errorStepAt) >= opts.ErrorStallGrace {
					reason = fmt.Sprintf("upstream error_message (stalled %s after error step)", opts.ErrorStallGrace)
				} else if opts.IdleTimeout > 0 && now.Sub(lastEventAt) >= opts.IdleTimeout {
					reason = fmt.Sprintf("stream idle timeout (no output for %s)", opts.IdleTimeout)
				}
				if reason != "" && !stoppedEarly {
					stoppedEarly = true
					earlyStopReason = reason
				}
				mu.Unlock()

				if reason != "" && cmd.Process != nil {
					_ = KillProcessGroup(cmd.Process.Pid)
					// Grace window before SIGKILL so subprocess cannot hang indefinitely.
					select {
					case <-watchStop:
					case <-time.After(2 * time.Second):
						_ = ForceKillProcessGroup(cmd.Process.Pid)
					}
					return
				}
			}
		}
	}()

	<-readDone
	waitErr := cmd.Wait()
	close(watchStop)
	<-watchDone

	mu.Lock()
	outcome.Stderr = strings.TrimSpace(stderrBuf.String())
	outcome.ExitErr = waitErr
	if earlyStopReason != "" {
		outcome.EarlyStopReason = earlyStopReason
		if outcome.ResultError == "" {
			outcome.ResultError = earlyStopReason
		}
	}
	res := outcome
	mu.Unlock()
	return res
}

type streamEventKind int

const (
	streamEventNeutral streamEventKind = iota
	streamEventProgress
	streamEventErrorStep
)

func parseStreamLine(line []byte, outcome *TurnOutcome, onConvID func(string)) streamEventKind {
	var ev struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		StepUpdate     *struct {
			ConversationID string `json:"conversation_id"`
			StepType       string `json:"step_type"`
			State          string `json:"state"`
			Error          string `json:"error"`
		} `json:"step_update"`
		Result *struct {
			ConversationID string `json:"conversation_id"`
			Status         string `json:"status"`
			Response       string `json:"response"`
			Error          string `json:"error"`
			DeniedActions  []struct {
				Action      string `json:"action"`
				DisplayName string `json:"display_name"`
			} `json:"denied_actions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return streamEventNeutral
	}
	if cid := strings.TrimSpace(ev.ConversationID); cid != "" && outcome.ConversationID != cid {
		outcome.ConversationID = cid
		if onConvID != nil {
			onConvID(cid)
		}
	}
	if ev.StepUpdate != nil {
		if cid := strings.TrimSpace(ev.StepUpdate.ConversationID); cid != "" && outcome.ConversationID != cid {
			outcome.ConversationID = cid
			if onConvID != nil {
				onConvID(cid)
			}
		}
		if strings.EqualFold(ev.StepUpdate.StepType, "error_message") {
			if msg := strings.TrimSpace(ev.StepUpdate.Error); msg != "" && outcome.ResultError == "" {
				outcome.ResultError = msg
			}
			return streamEventErrorStep
		}
		return streamEventProgress
	}
	if ev.Event == "result" && ev.Result != nil {
		if cid := strings.TrimSpace(ev.Result.ConversationID); cid != "" && outcome.ConversationID != cid {
			outcome.ConversationID = cid
			if onConvID != nil {
				onConvID(cid)
			}
		}
		outcome.Status = strings.TrimSpace(ev.Result.Status)
		outcome.Response = ev.Result.Response
		if msg := strings.TrimSpace(ev.Result.Error); msg != "" {
			outcome.ResultError = msg
		}
		outcome.DeniedActions = nil
		for _, da := range ev.Result.DeniedActions {
			label := strings.TrimSpace(da.Action)
			if label == "" {
				label = strings.TrimSpace(da.DisplayName)
			}
			if label != "" {
				outcome.DeniedActions = append(outcome.DeniedActions, label)
			}
		}
		return streamEventProgress
	}
	return streamEventNeutral
}

type limitedWriter struct {
	w       io.Writer
	limit   int
	written int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.written >= l.limit {
		return len(p), nil
	}
	rem := l.limit - l.written
	chunk := p
	if len(chunk) > rem {
		chunk = chunk[:rem]
	}
	n, err := l.w.Write(chunk)
	l.written += n
	if err != nil {
		return n, err
	}
	return len(p), nil
}

func uniqueNonEmpty(items ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, it := range items {
		t := strings.TrimSpace(it)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
