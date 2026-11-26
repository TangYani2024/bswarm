package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TangYani2024/bswarm/internal/store"
)

// Config declares external gateway LLM providers and optional routing pools.
// Any OpenAI-compatible or Gemini-compatible API can be registered here;
// providers without MaxConcurrency run unlimited parallel workers by default.
type Config struct {
	Providers map[string]*ProviderSpec `json:"providers,omitempty"`
	Pools     map[string][]string      `json:"pools,omitempty"`
}

// ProviderSpec configures one external gateway API provider.
type ProviderSpec struct {
	URL                string            `json:"url"`
	WireProtocol       string            `json:"wire_protocol,omitempty"`       // "openai" (default) | "gemini"
	APIKey             string            `json:"api_key,omitempty"`             // optional inline key
	APIKeyEnv          string            `json:"api_key_env,omitempty"`         // preferred: env var name
	APIKeyFile         string            `json:"api_key_file,omitempty"`        // read key from local file
	APIKeyLine         int               `json:"api_key_line,omitempty"`        // 1-based line number in api_key_file (default 1)
	Models             []string          `json:"models,omitempty"`              // available/advertised model IDs
	DefaultModel       string            `json:"default_model,omitempty"`       // fallback when --provider is passed without --model
	Headers            map[string]string `json:"headers,omitempty"`             // extra headers (AGY_LLM_GATEWAY_HEADERS)
	ProxyURL           string            `json:"proxy_url,omitempty"`           // AGY_LLM_GATEWAY_PROXY_URL
	CACert             string            `json:"ca_cert,omitempty"`             // AGY_LLM_GATEWAY_CA_CERT
	MaxConcurrency     int               `json:"max_concurrency,omitempty"`     // <= 0 means unlimited
	ConcurrencyScope   string            `json:"concurrency_scope,omitempty"`   // "provider" (default) | "model" | "prefix:<sep>" (e.g. "prefix:/")
	Timeout            string            `json:"timeout,omitempty"`             // per-turn hard timeout, default "15m"
	IdleTimeout        string            `json:"idle_timeout,omitempty"`        // stream idle / no-token timeout, default "90s" ("0"/"off" disables)
	ErrorStallGrace    string            `json:"error_stall_grace,omitempty"`   // grace window after step_type=error_message before early-stop, default "8s"
	Mode               string            `json:"mode,omitempty"`                // agy --mode, default "accept-edits"
	AllowedPermissions []string          `json:"allowed_permissions,omitempty"` // settings.json permissions.allow rules
	EnableSandbox      *bool             `json:"enable_sandbox,omitempty"`      // default true
	SandboxAllowNet    bool              `json:"sandbox_allow_network,omitempty"`
	AgyBin             string            `json:"agy_bin,omitempty"` // override agy binary path
}

// ResolvedTarget is the result of resolving --provider / --model flags against Config.
type ResolvedTarget struct {
	IsExternal     bool
	ProviderName   string
	Spec           *ProviderSpec
	Model          string
	PoolName       string
	ConcurrencyKey string
	MaxConcurrency int
	NeedsQueue     bool // true if all matching concurrency slots are currently full
	DisplayModel   string
}

// DefaultAllowedPermissions grants workspace-scoped file/command operations while relying
// on terminal sandbox + bswarm's PreToolUse hook to enforce --worktree/--owns boundaries.
var DefaultAllowedPermissions = []string{
	"read_file(*)",
	"write_file(*)",
	"edit_file(*)",
	"command(*)",
}

// DefaultConfigPaths returns ordered candidate paths for provider config discovery.
func DefaultConfigPaths(repoRoot, storeRoot string) []string {
	var paths []string
	if envPath := strings.TrimSpace(os.Getenv("BSWARM_PROVIDERS_FILE")); envPath != "" {
		paths = append(paths, envPath)
	}
	if strings.TrimSpace(repoRoot) != "" {
		paths = append(paths, filepath.Join(repoRoot, ".bswarm", "providers.json"))
	}
	if strings.TrimSpace(storeRoot) != "" {
		paths = append(paths, filepath.Join(storeRoot, "providers.json"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".gemini", "bswarm-providers.json"))
	}
	return paths
}

// Load discovers and parses the first existing provider config file.
// If no config file exists (and BSWARM_PROVIDERS_FILE was not explicitly set), it returns an empty Config and nil error.
func Load(repoRoot, storeRoot string) (*Config, string, error) {
	explicit := strings.TrimSpace(os.Getenv("BSWARM_PROVIDERS_FILE"))
	if explicit != "" {
		if _, serr := os.Stat(explicit); os.IsNotExist(serr) {
			return &Config{
				Providers: map[string]*ProviderSpec{},
				Pools:     map[string][]string{},
			}, "", nil
		}
		cfg, err := LoadFile(explicit)
		return cfg, explicit, err
	}
	for _, p := range DefaultConfigPaths(repoRoot, storeRoot) {
		if _, err := os.Stat(p); err == nil {
			cfg, lerr := LoadFile(p)
			return cfg, p, lerr
		}
	}
	return &Config{
		Providers: map[string]*ProviderSpec{},
		Pools:     map[string][]string{},
	}, "", nil
}

// LoadFile parses and validates a providers.json file.
func LoadFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read providers config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse providers config %s: %w", path, err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]*ProviderSpec{}
	}
	if cfg.Pools == nil {
		cfg.Pools = map[string][]string{}
	}
	for name, spec := range cfg.Providers {
		if spec == nil {
			return nil, fmt.Errorf("provider %q is null", name)
		}
		if strings.TrimSpace(spec.URL) == "" {
			return nil, fmt.Errorf("provider %q missing required field \"url\"", name)
		}
		if spec.WireProtocol == "" {
			spec.WireProtocol = "openai"
		}
		wp := strings.ToLower(strings.TrimSpace(spec.WireProtocol))
		if wp != "openai" && wp != "gemini" {
			return nil, fmt.Errorf("provider %q has unsupported wire_protocol %q (expected openai or gemini)", name, spec.WireProtocol)
		}
		spec.WireProtocol = wp
	}
	return &cfg, nil
}

// ResolveAPIKey fetches the API key without ever logging it.
// Order: api_key_env -> api_key_file (optional api_key_line) -> api_key.
func (s *ProviderSpec) ResolveAPIKey() (string, error) {
	if s == nil {
		return "", errors.New("nil provider spec")
	}
	if envName := strings.TrimSpace(s.APIKeyEnv); envName != "" {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("environment variable %s is empty or unset", envName)
	}
	if fpath := strings.TrimSpace(s.APIKeyFile); fpath != "" {
		if strings.HasPrefix(fpath, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				fpath = filepath.Join(home, fpath[2:])
			}
		}
		raw, err := os.ReadFile(fpath)
		if err != nil {
			return "", fmt.Errorf("read api_key_file %s: %w", fpath, err)
		}
		lineIdx := s.APIKeyLine
		if lineIdx <= 0 {
			lineIdx = 1
		}
		lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		var nonEmpty []string
		for _, ln := range lines {
			if t := strings.TrimSpace(ln); t != "" {
				nonEmpty = append(nonEmpty, t)
			}
		}
		if lineIdx > len(nonEmpty) {
			return "", fmt.Errorf("api_key_file %s has %d non-empty line(s), requested line %d", fpath, len(nonEmpty), lineIdx)
		}
		return nonEmpty[lineIdx-1], nil
	}
	return strings.TrimSpace(s.APIKey), nil
}

// EffectiveTimeout parses spec.Timeout or defaults to 15m.
func (s *ProviderSpec) EffectiveTimeout() time.Duration {
	if s != nil && strings.TrimSpace(s.Timeout) != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(s.Timeout)); err == nil && d > 0 {
			return d
		}
	}
	return 15 * time.Minute
}

// EffectiveIdleTimeout parses spec.IdleTimeout or defaults to 90s.
// Set "0", "0s", or "off" to disable stream idle detection.
func (s *ProviderSpec) EffectiveIdleTimeout() time.Duration {
	if s != nil && strings.TrimSpace(s.IdleTimeout) != "" {
		raw := strings.ToLower(strings.TrimSpace(s.IdleTimeout))
		if raw == "0" || raw == "0s" || raw == "off" || raw == "none" {
			return 0
		}
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 90 * time.Second
}

// EffectiveErrorGrace parses spec.ErrorStallGrace or defaults to 8s.
func (s *ProviderSpec) EffectiveErrorGrace() time.Duration {
	if s != nil && strings.TrimSpace(s.ErrorStallGrace) != "" {
		raw := strings.ToLower(strings.TrimSpace(s.ErrorStallGrace))
		if raw == "0" || raw == "0s" || raw == "off" || raw == "none" {
			return 0
		}
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 8 * time.Second
}

// EffectiveMode returns the configured agy --mode or "accept-edits".
func (s *ProviderSpec) EffectiveMode() string {
	if s != nil && strings.TrimSpace(s.Mode) != "" {
		return strings.TrimSpace(s.Mode)
	}
	return "accept-edits"
}

// EffectivePermissions returns configured permissions.allow rules or DefaultAllowedPermissions.
func (s *ProviderSpec) EffectivePermissions() []string {
	if s != nil && len(s.AllowedPermissions) > 0 {
		out := make([]string, 0, len(s.AllowedPermissions))
		for _, r := range s.AllowedPermissions {
			if t := strings.TrimSpace(r); t != "" {
				out = append(out, t)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return append([]string(nil), DefaultAllowedPermissions...)
}

// EffectiveSandbox returns whether terminal sandbox is enabled (default true).
func (s *ProviderSpec) EffectiveSandbox() bool {
	if s != nil && s.EnableSandbox != nil {
		return *s.EnableSandbox
	}
	return true
}

// ConcurrencyKey computes the rate-limit bucket key for (providerName, model).
// Returns "" when concurrency is unlimited (MaxConcurrency <= 0).
func (s *ProviderSpec) ConcurrencyKey(providerName, model string) string {
	if s == nil || s.MaxConcurrency <= 0 {
		return ""
	}
	scope := strings.TrimSpace(s.ConcurrencyScope)
	switch {
	case scope == "" || scope == "provider":
		return providerName
	case scope == "model":
		return providerName + ":" + model
	case strings.HasPrefix(scope, "prefix:"):
		sep := strings.TrimPrefix(scope, "prefix:")
		if sep != "" {
			if idx := strings.Index(model, sep); idx > 0 {
				return providerName + ":" + model[:idx]
			}
		}
		return providerName + ":" + model
	default:
		return providerName
	}
}

// WorkingCounts tallies how many currently working nodes hold each ConcurrencyKey.
func WorkingCounts(nodes map[string]*store.Node) map[string]int {
	counts := map[string]int{}
	for _, n := range nodes {
		if n == nil || n.State != store.NodeWorking || n.ConcurrencyKey == "" {
			continue
		}
		counts[n.ConcurrencyKey]++
	}
	return counts
}

// SlotAvailable checks whether key can accept another concurrent worker given maxConc.
func SlotAvailable(counts map[string]int, key string, maxConc int) bool {
	if key == "" || maxConc <= 0 {
		return true
	}
	return counts[key] < maxConc
}

// ParseModelSpec splits "provider:model" if provider exists in Config.
func (c *Config) ParseModelSpec(providerFlag, modelFlag string) (provName, modelName, poolName string, err error) {
	provFlag := strings.TrimSpace(providerFlag)
	modFlag := strings.TrimSpace(modelFlag)

	if provFlag == "native" || provFlag == "official" {
		return "", modFlag, "", nil
	}
	if provFlag == "pool" {
		if modFlag == "" {
			return "", "", "", errors.New("pool name is required when --provider=pool")
		}
		return "", "", modFlag, nil
	}
	if provFlag != "" {
		if c == nil || c.Providers[provFlag] == nil {
			return "", "", "", fmt.Errorf("unknown provider %q (check providers.json or run `bswarm providers`)", provFlag)
		}
		spec := c.Providers[provFlag]
		m := modFlag
		if m == "" {
			m = strings.TrimSpace(spec.DefaultModel)
		}
		if m == "" && len(spec.Models) > 0 {
			m = strings.TrimSpace(spec.Models[0])
		}
		if m == "" {
			return "", "", "", fmt.Errorf("provider %q requires --model (no default_model or models configured)", provFlag)
		}
		return provFlag, m, "", nil
	}

	if strings.HasPrefix(modFlag, "pool:") {
		pn := strings.TrimSpace(strings.TrimPrefix(modFlag, "pool:"))
		if pn == "" {
			return "", "", "", errors.New("empty pool name in --model pool:<name>")
		}
		if c == nil || len(c.Pools[pn]) == 0 {
			return "", "", "", fmt.Errorf("unknown or empty model pool %q", pn)
		}
		return "", "", pn, nil
	}

	if c != nil && strings.Contains(modFlag, ":") {
		idx := strings.IndexByte(modFlag, ':')
		candProv := strings.TrimSpace(modFlag[:idx])
		candMod := strings.TrimSpace(modFlag[idx+1:])
		if _, ok := c.Providers[candProv]; ok {
			if candMod == "" {
				spec := c.Providers[candProv]
				candMod = strings.TrimSpace(spec.DefaultModel)
				if candMod == "" && len(spec.Models) > 0 {
					candMod = strings.TrimSpace(spec.Models[0])
				}
			}
			if candMod == "" {
				return "", "", "", fmt.Errorf("provider %q requires a model after ':'", candProv)
			}
			return candProv, candMod, "", nil
		}
	}

	// Not matching any configured external provider -> fall back to official Antigravity model routing.
	return "", modFlag, "", nil
}

// ResolveTarget resolves --provider/--model flags and checks concurrency slot availability against active nodes.
func (c *Config) ResolveTarget(providerFlag, modelFlag string, activeNodes map[string]*store.Node) (ResolvedTarget, error) {
	provName, modelName, poolName, err := c.ParseModelSpec(providerFlag, modelFlag)
	if err != nil {
		return ResolvedTarget{}, err
	}
	if poolName != "" {
		return c.SelectFromPool(poolName, activeNodes)
	}
	if provName == "" {
		return ResolvedTarget{
			IsExternal:   false,
			Model:        modelName,
			DisplayModel: modelName,
		}, nil
	}
	spec := c.Providers[provName]
	ckey := spec.ConcurrencyKey(provName, modelName)
	counts := WorkingCounts(activeNodes)
	avail := SlotAvailable(counts, ckey, spec.MaxConcurrency)
	return ResolvedTarget{
		IsExternal:     true,
		ProviderName:   provName,
		Spec:           spec,
		Model:          modelName,
		ConcurrencyKey: ckey,
		MaxConcurrency: spec.MaxConcurrency,
		NeedsQueue:     !avail,
		DisplayModel:   provName + ":" + modelName,
	}, nil
}

// SelectFromPool picks the least-loaded available candidate from a configured pool.
// If every candidate's concurrency bucket is currently full, returns NeedsQueue=true so the
// node queues and PromoteReadyTasks dynamically re-selects the first freed bucket later.
func (c *Config) SelectFromPool(poolName string, activeNodes map[string]*store.Node) (ResolvedTarget, error) {
	return c.SelectFromPoolExcluding(poolName, activeNodes, nil)
}

// SelectFromPoolExcluding picks the least-loaded available candidate from poolName while skipping
// any "provider:model" targets present in exclude (used for automatic pool failover).
func (c *Config) SelectFromPoolExcluding(poolName string, activeNodes map[string]*store.Node, exclude []string) (ResolvedTarget, error) {
	if c == nil || len(c.Pools[poolName]) == 0 {
		return ResolvedTarget{}, fmt.Errorf("unknown or empty model pool %q", poolName)
	}
	excluded := make(map[string]bool, len(exclude))
	for _, ex := range exclude {
		if t := strings.TrimSpace(ex); t != "" {
			excluded[t] = true
		}
	}
	counts := WorkingCounts(activeNodes)

	type candidate struct {
		target ResolvedTarget
		load   int
		order  int
	}
	var available []candidate
	var firstValid *ResolvedTarget

	for i, entry := range c.Pools[poolName] {
		pName, mName, subPool, err := c.ParseModelSpec("", entry)
		if err != nil || subPool != "" || pName == "" {
			continue
		}
		display := pName + ":" + mName
		if excluded[display] {
			continue
		}
		spec := c.Providers[pName]
		ckey := spec.ConcurrencyKey(pName, mName)
		res := ResolvedTarget{
			IsExternal:     true,
			ProviderName:   pName,
			Spec:           spec,
			Model:          mName,
			PoolName:       poolName,
			ConcurrencyKey: ckey,
			MaxConcurrency: spec.MaxConcurrency,
			DisplayModel:   display,
		}
		if firstValid == nil {
			cp := res
			firstValid = &cp
		}
		if SlotAvailable(counts, ckey, spec.MaxConcurrency) {
			available = append(available, candidate{
				target: res,
				load:   counts[ckey],
				order:  i,
			})
		}
	}

	if firstValid == nil {
		if len(excluded) > 0 {
			return ResolvedTarget{}, fmt.Errorf("model pool %q has no remaining healthy candidates (excluded: %s)", poolName, strings.Join(exclude, ", "))
		}
		return ResolvedTarget{}, fmt.Errorf("model pool %q has no valid provider:model entries", poolName)
	}
	if len(available) == 0 {
		return ResolvedTarget{
			IsExternal:   true,
			PoolName:     poolName,
			NeedsQueue:   true,
			DisplayModel: "pool:" + poolName,
		}, nil
	}
	sort.Slice(available, func(i, j int) bool {
		if available[i].load != available[j].load {
			return available[i].load < available[j].load
		}
		return available[i].order < available[j].order
	})
	return available[0].target, nil
}

// CanStartNode checks whether an existing queued/idle external node can start right now.
// For pool-backed queued nodes without a fixed model yet, it dynamically picks an idle candidate
// while skipping any targets already listed in n.FailedTargets.
func (c *Config) CanStartNode(n *store.Node, activeNodes map[string]*store.Node) (ResolvedTarget, bool, error) {
	if n == nil {
		return ResolvedTarget{}, false, errors.New("nil node")
	}
	if n.Pool != "" && n.ConversationID == "" {
		res, err := c.SelectFromPoolExcluding(n.Pool, activeNodes, n.FailedTargets)
		if err != nil {
			return ResolvedTarget{}, false, err
		}
		return res, !res.NeedsQueue, nil
	}
	if n.Provider == "" {
		return ResolvedTarget{IsExternal: false, Model: n.Model, DisplayModel: n.Model}, true, nil
	}
	if c == nil || c.Providers[n.Provider] == nil {
		return ResolvedTarget{}, false, fmt.Errorf("provider %q is no longer configured", n.Provider)
	}
	spec := c.Providers[n.Provider]
	model := n.ProviderModel
	if model == "" {
		model = n.Model
	}
	ckey := spec.ConcurrencyKey(n.Provider, model)
	counts := WorkingCounts(activeNodes)
	avail := SlotAvailable(counts, ckey, spec.MaxConcurrency)
	return ResolvedTarget{
		IsExternal:     true,
		ProviderName:   n.Provider,
		Spec:           spec,
		Model:          model,
		PoolName:       n.Pool,
		ConcurrencyKey: ckey,
		MaxConcurrency: spec.MaxConcurrency,
		NeedsQueue:     !avail,
		DisplayModel:   n.Provider + ":" + model,
	}, avail, nil
}

// ProviderSummary is a secret-safe view of configured providers for `bswarm providers`.
type ProviderSummary struct {
	Name             string   `json:"name"`
	URL              string   `json:"url"`
	WireProtocol     string   `json:"wire_protocol"`
	KeySource        string   `json:"key_source"`
	Models           []string `json:"models,omitempty"`
	DefaultModel     string   `json:"default_model,omitempty"`
	MaxConcurrency   int      `json:"max_concurrency"`
	ConcurrencyScope string   `json:"concurrency_scope,omitempty"`
}

func (c *Config) Summaries() []ProviderSummary {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ProviderSummary, 0, len(names))
	for _, name := range names {
		s := c.Providers[name]
		ks := "none"
		switch {
		case strings.TrimSpace(s.APIKeyEnv) != "":
			ks = "env:" + strings.TrimSpace(s.APIKeyEnv)
		case strings.TrimSpace(s.APIKeyFile) != "":
			ks = fmt.Sprintf("file:%s (line %d)", strings.TrimSpace(s.APIKeyFile), max(1, s.APIKeyLine))
		case strings.TrimSpace(s.APIKey) != "":
			ks = "inline(redacted)"
		}
		out = append(out, ProviderSummary{
			Name:             name,
			URL:              s.URL,
			WireProtocol:     s.WireProtocol,
			KeySource:        ks,
			Models:           s.Models,
			DefaultModel:     s.DefaultModel,
			MaxConcurrency:   s.MaxConcurrency,
			ConcurrencyScope: s.ConcurrencyScope,
		})
	}
	return out
}
