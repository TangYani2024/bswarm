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
	"runtime"
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

// BackendKind selects how bswarm executes an isolated external API worker turn.
type BackendKind string

const (
	BackendAgyCLI    BackendKind = "agy-cli"
	BackendDesktopLS BackendKind = "desktop-ls"
)

// WorkerBackend describes the resolved runtime binary for external API workers.
type WorkerBackend struct {
	Kind   BackendKind
	Binary string
}

// TurnOpts configures one headless external gateway worker turn.
type TurnOpts struct {
	AgyBin               string
	IsoHome              string
	HostAppDataDir       string
	MirrorTranscriptPath string
	WorkDir              string
	Env                  []string
	Model                string
	ConvID               string
	Mode                 string
	Timeout              time.Duration
	IdleTimeout          time.Duration
	ErrorStallGrace      time.Duration
	Prompt               string
	LogPath              string
	OnConvID             func(string)
}

// isLanguageServerBinary detects Desktop's bundled language_server binary, which supports
// `agentapi` subcommands but lacks CLI print/stream-json flags required by external gateway turns.
func isLanguageServerBinary(p string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(p)))
	base = strings.TrimSuffix(base, ".exe")
	return base == "language_server" || strings.HasPrefix(base, "language_server_") || strings.HasPrefix(base, "language_server-")
}

// ResolveAgyBinary finds the `agy` CLI binary to run headless external gateway workers.
// Real `agy` installations are checked ahead of ANTIGRAVITY_AGENTAPI_EXE because Antigravity Desktop
// injects ANTIGRAVITY_AGENTAPI_EXE pointing to `resources/bin/language_server`, which cannot run
// `--output-format stream-json --print`.
func ResolveAgyBinary(spec *provider.ProviderSpec) (string, error) {
	if spec != nil && strings.TrimSpace(spec.AgyBin) != "" {
		return strings.TrimSpace(spec.AgyBin), nil
	}
	if v := strings.TrimSpace(os.Getenv("BSWARM_AGY_BIN")); v != "" {
		return v, nil
	}
	if p, err := exec.LookPath("agy"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, name := range []string{"agy", "agy.exe"} {
			cand := filepath.Join(home, ".local", "bin", name)
			if fi, serr := os.Stat(cand); serr == nil && !fi.IsDir() {
				return cand, nil
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE")); v != "" {
		if isLanguageServerBinary(v) {
			return "", fmt.Errorf("external API gateway requires the `agy` CLI (ANTIGRAVITY_AGENTAPI_EXE=%q is Desktop's language_server and lacks --output-format stream-json); install `agy` CLI or set BSWARM_AGY_BIN / provider.agy_bin", v)
		}
		return v, nil
	}
	return "", errors.New("could not locate `agy` CLI binary required for external API gateway (install `agy` CLI or set BSWARM_AGY_BIN / provider.agy_bin)")
}

func defaultDesktopLSPaths() []string {
	var cands []string
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		cands = append(cands, "/Applications/Antigravity.app/Contents/Resources/bin/language_server")
		if home != "" {
			cands = append(cands, filepath.Join(home, "Applications", "Antigravity.app", "Contents", "Resources", "bin", "language_server"))
		}
	case "windows":
		if localApp := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localApp != "" {
			cands = append(cands, filepath.Join(localApp, "Programs", "Antigravity", "resources", "bin", "language_server.exe"))
		}
		if progFiles := strings.TrimSpace(os.Getenv("ProgramFiles")); progFiles != "" {
			cands = append(cands, filepath.Join(progFiles, "Antigravity", "resources", "bin", "language_server.exe"))
		}
	default:
		cands = append(cands,
			"/opt/Antigravity/resources/bin/language_server",
			"/usr/share/antigravity/resources/bin/language_server",
			"/usr/lib/antigravity/resources/bin/language_server",
		)
		if home != "" {
			cands = append(cands, filepath.Join(home, ".local", "share", "Antigravity", "resources", "bin", "language_server"))
		}
	}
	return cands
}

func findDesktopLSBinary(spec *provider.ProviderSpec) string {
	if spec != nil && strings.TrimSpace(spec.LSBin) != "" {
		return strings.TrimSpace(spec.LSBin)
	}
	if v := strings.TrimSpace(os.Getenv("BSWARM_LS_BIN")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE")); v != "" && isLanguageServerBinary(v) {
		if fi, err := os.Stat(v); err == nil && !fi.IsDir() {
			return v
		}
	}
	for _, cand := range defaultDesktopLSPaths() {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	return ""
}

// ResolveWorkerBackend selects the best available backend for external API gateway turns:
//   - Uses `agy` CLI when explicitly configured or installed.
//   - Automatically falls back to Antigravity Desktop's bundled `language_server` (zero CLI required!)
//     when running on Desktop-only machines or when BSWARM_GATEWAY_BACKEND=desktop-ls / provider.ls_bin is set.
func ResolveWorkerBackend(spec *provider.ProviderSpec) (WorkerBackend, error) {
	prefer := strings.ToLower(strings.TrimSpace(os.Getenv("BSWARM_GATEWAY_BACKEND")))
	if prefer == string(BackendDesktopLS) || prefer == "ls" || prefer == "desktop" {
		if ls := findDesktopLSBinary(spec); ls != "" {
			return WorkerBackend{Kind: BackendDesktopLS, Binary: ls}, nil
		}
		return WorkerBackend{}, errors.New("BSWARM_GATEWAY_BACKEND=desktop-ls requested, but could not locate Antigravity Desktop language_server (set BSWARM_LS_BIN or provider.ls_bin)")
	}
	if prefer == string(BackendAgyCLI) || prefer == "agy" || prefer == "cli" {
		bin, err := ResolveAgyBinary(spec)
		if err != nil {
			return WorkerBackend{}, err
		}
		return WorkerBackend{Kind: BackendAgyCLI, Binary: bin}, nil
	}

	// Explicit provider/env binary overrides first.
	if spec != nil && strings.TrimSpace(spec.AgyBin) != "" {
		return WorkerBackend{Kind: BackendAgyCLI, Binary: strings.TrimSpace(spec.AgyBin)}, nil
	}
	if v := strings.TrimSpace(os.Getenv("BSWARM_AGY_BIN")); v != "" {
		return WorkerBackend{Kind: BackendAgyCLI, Binary: v}, nil
	}
	if spec != nil && strings.TrimSpace(spec.LSBin) != "" {
		return WorkerBackend{Kind: BackendDesktopLS, Binary: strings.TrimSpace(spec.LSBin)}, nil
	}
	if v := strings.TrimSpace(os.Getenv("BSWARM_LS_BIN")); v != "" {
		return WorkerBackend{Kind: BackendDesktopLS, Binary: v}, nil
	}

	// Prefer `agy` CLI when present; otherwise seamlessly use Desktop's bundled `language_server`.
	if bin, err := ResolveAgyBinary(spec); err == nil {
		return WorkerBackend{Kind: BackendAgyCLI, Binary: bin}, nil
	}
	if ls := findDesktopLSBinary(spec); ls != "" {
		return WorkerBackend{Kind: BackendDesktopLS, Binary: ls}, nil
	}
	return WorkerBackend{}, errors.New("could not locate `agy` CLI or Antigravity Desktop `language_server` (install `agy` CLI, install Antigravity Desktop, or set BSWARM_AGY_BIN / BSWARM_LS_BIN)")
}

// RunTurn dispatches an external gateway turn to either the `agy` CLI or Desktop `language_server` backend.
func RunTurn(ctx context.Context, backend WorkerBackend, opts TurnOpts) TurnOutcome {
	if opts.AgyBin == "" {
		opts.AgyBin = backend.Binary
	}
	if backend.Kind == BackendDesktopLS {
		return RunDesktopLSTurn(ctx, opts)
	}
	return RunAgyTurn(ctx, opts)
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
	// On Windows without Developer Mode, directory symlinks fail with privilege errors,
	// so fall back to copying the lightweight plugin manifest + hooks launcher files.
	mountedPluginName := ""
	if strings.TrimSpace(pluginDir) != "" {
		plugParent := filepath.Join(isoHome, ".gemini", "config", "plugins")
		if err := os.MkdirAll(plugParent, 0o700); err == nil {
			mountedPluginName = filepath.Base(pluginDir)
			dst := filepath.Join(plugParent, mountedPluginName)
			if _, lerr := os.Lstat(dst); os.IsNotExist(lerr) {
				if serr := os.Symlink(pluginDir, dst); serr != nil {
					_ = copyPluginHooksDir(pluginDir, dst)
				}
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
	if err := store.WriteAtomic(filepath.Join(cliDir, "settings.json"), raw, 0o600); err != nil {
		return err
	}

	// Also write Desktop language_server's config.json so zero-CLI Desktop gateway turns
	// auto-execute workspace/command tools and enable the mounted bswarm hooks plugin.
	cfgDir := filepath.Join(isoHome, ".gemini", "config")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return fmt.Errorf("create isolated config dir: %w", err)
	}
	pluginsCfg := map[string]any{}
	if mountedPluginName != "" {
		pluginsCfg[mountedPluginName] = map[string]any{"enabled": true}
	}
	desktopCfg := map[string]any{
		"plugins": pluginsCfg,
		"userSettings": map[string]any{
			"autoExecutionPolicy":          "CASCADE_COMMANDS_AUTO_EXECUTION_EAGER",
			"permissionPreset":             "AGENT_PERMISSION_PRESET_TURBO",
			"nonWorkspaceFileAccessPolicy": "AGENT_SETTING_POLICY_ALLOW",
			"enableTerminalSandbox":        spec.EffectiveSandbox(),
			"sandboxAllowNetwork":          spec.SandboxAllowNet,
			"trustedWorkspaces":            trusted,
			"permissionGrantsV2Migrated":  true,
		},
	}
	deskRaw, err := json.MarshalIndent(desktopCfg, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(filepath.Join(cfgDir, "config.json"), deskRaw, 0o600)
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
		ku := strings.ToUpper(k)
		if k == "HOME" ||
			ku == "USERPROFILE" ||
			strings.HasPrefix(ku, "ANTIGRAVITY_") ||
			strings.HasPrefix(ku, "AGY_") ||
			strings.HasPrefix(ku, "JETSKI_") ||
			strings.HasPrefix(ku, "BSWARM_") {
			continue
		}
		filtered = append(filtered, kv)
	}

	models := uniqueNonEmpty(append([]string{model}, spec.Models...)...)
	filtered = append(filtered,
		"HOME="+isoHome,
		"USERPROFILE="+isoHome,
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

	outcome := TurnOutcome{ConversationID: opts.ConvID}
	mirror := newTranscriptMirror(opts.HostAppDataDir, opts.MirrorTranscriptPath, opts.ConvID)
	failEarly := func(err error) TurnOutcome {
		out := TurnOutcome{ConversationID: opts.ConvID, ExitErr: err}
		mirror.Finish(out)
		return out
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failEarly(err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 8192}

	if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
		return failEarly(err)
	}
	logFile, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return failEarly(err)
	}
	defer logFile.Close()

	if err := cmd.Start(); err != nil {
		return failEarly(err)
	}
	if strings.TrimSpace(opts.ConvID) != "" {
		mirror.WriteUserInput(opts.Prompt)
	}

	var (
		mu              sync.Mutex
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
			evKind, step := parseStreamLine(line, &outcome, opts.OnConvID)
			curConvID := outcome.ConversationID
			switch evKind {
			case streamEventErrorStep:
				if errorStepAt.IsZero() {
					errorStepAt = now
				}
			case streamEventProgress:
				errorStepAt = time.Time{}
			}
			mu.Unlock()

			if curConvID != "" {
				mirror.SetConvID(curConvID)
				mirror.WriteUserInput(opts.Prompt)
			}
			if step != nil {
				mirror.WriteStreamStep(step.stepType, step.state, step.errText, step.toolName, step.summary)
			}
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
	mirror.SetConvID(res.ConversationID)
	mirror.Finish(res)
	return res
}

type streamEventKind int

const (
	streamEventNeutral streamEventKind = iota
	streamEventProgress
	streamEventErrorStep
)

type parsedStreamStep struct {
	stepType string
	state    string
	errText  string
	toolName string
	summary  string
}

func parseStreamLine(line []byte, outcome *TurnOutcome, onConvID func(string)) (streamEventKind, *parsedStreamStep) {
	var ev struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		StepUpdate     *struct {
			ConversationID string `json:"conversation_id"`
			StepType       string `json:"step_type"`
			State          string `json:"state"`
			Error          string `json:"error"`
			ToolName       string `json:"tool_name"`
			ToolSummary    string `json:"tool_summary"`
			Summary        string `json:"summary"`
			Content        string `json:"content"`
			Thinking       string `json:"thinking"`
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
		return streamEventNeutral, nil
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
		summary := firstNonEmpty(
			ev.StepUpdate.ToolSummary,
			ev.StepUpdate.Summary,
			ev.StepUpdate.Thinking,
			ev.StepUpdate.Content,
		)
		step := &parsedStreamStep{
			stepType: ev.StepUpdate.StepType,
			state:    ev.StepUpdate.State,
			errText:  ev.StepUpdate.Error,
			toolName: ev.StepUpdate.ToolName,
			summary:  summary,
		}
		if strings.EqualFold(ev.StepUpdate.StepType, "error_message") {
			if msg := strings.TrimSpace(ev.StepUpdate.Error); msg != "" && outcome.ResultError == "" {
				outcome.ResultError = msg
			}
			return streamEventErrorStep, step
		}
		return streamEventProgress, step
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
		return streamEventProgress, nil
	}
	return streamEventNeutral, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
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

func copyPluginHooksDir(srcDir, dstDir string) error {
	for _, rel := range []string{"plugin.json", "hooks.json", filepath.Join("scripts", "hook.mjs")} {
		src := filepath.Join(srcDir, rel)
		raw, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		dst := filepath.Join(dstDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}
