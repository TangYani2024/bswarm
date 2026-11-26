package provider

import (
	"os"
	"strings"
	"sync"
	"time"
)

// Reloader transparently hot-reloads providers.json whenever the file is created,
// modified, switched (via precedence), or removed on disk.
// If a user saves an incomplete/invalid JSON mid-edit, Reloader keeps the last known
// good Config so running trees never crash, and automatically picks up the fix on next check.
type Reloader struct {
	mu        sync.Mutex
	repoRoot  string
	storeRoot string
	lastPath  string
	lastMod   time.Time
	lastSize  int64
	cached    *Config
}

func NewReloader(repoRoot, storeRoot string) *Reloader {
	return &Reloader{
		repoRoot:  repoRoot,
		storeRoot: storeRoot,
		cached:    &Config{Providers: map[string]*ProviderSpec{}, Pools: map[string][]string{}},
	}
}

func (r *Reloader) resolveActivePath() string {
	if explicit := strings.TrimSpace(os.Getenv("BSWARM_PROVIDERS_FILE")); explicit != "" {
		return explicit
	}
	for _, p := range DefaultConfigPaths(r.repoRoot, r.storeRoot) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// Current returns the up-to-date Config, active file path, whether a reload occurred, and any parse error
// (note: on parse error, the previously valid Config is still returned so callers stay resilient).
func (r *Reloader) Current() (*Config, string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	path := r.resolveActivePath()
	if path == "" {
		changed := r.lastPath != ""
		r.lastPath = ""
		r.lastMod = time.Time{}
		r.lastSize = 0
		r.cached = &Config{Providers: map[string]*ProviderSpec{}, Pools: map[string][]string{}}
		return r.cached, "", changed, nil
	}

	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			changed := r.lastPath != ""
			r.lastPath = ""
			r.lastMod = time.Time{}
			r.lastSize = 0
			r.cached = &Config{Providers: map[string]*ProviderSpec{}, Pools: map[string][]string{}}
			return r.cached, "", changed, nil
		}
		return r.cached, r.lastPath, false, err
	}

	if path == r.lastPath && fi.ModTime().Equal(r.lastMod) && fi.Size() == r.lastSize {
		return r.cached, r.lastPath, false, nil
	}

	cfg, lerr := LoadFile(path)
	if lerr != nil {
		// Keep last known good config intact until user finishes fixing the file.
		return r.cached, r.lastPath, false, lerr
	}
	r.lastPath = path
	r.lastMod = fi.ModTime()
	r.lastSize = fi.Size()
	r.cached = cfg
	return r.cached, r.lastPath, true, nil
}
