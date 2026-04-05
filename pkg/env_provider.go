package graft

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"

	"github.com/edaniels/graft/errors"
)

// EnvProvider captures directory-specific environment from tools like mise.
type EnvProvider interface {
	Detect() bool
	CaptureEnv(ctx context.Context, dir string) ([]string, error)
	ConfigFiles() []string
}

func detectTool(name string, lookPath func(string) (string, error)) bool {
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	_, err := lookPath(name)

	return err == nil
}

type miseEnvProvider struct {
	lookPath     func(file string) (string, error)
	trustedRoots atomic.Pointer[[]string]
}

func (m *miseEnvProvider) Detect() bool { return detectTool("mise", m.lookPath) }

func (m *miseEnvProvider) ConfigFiles() []string {
	return []string{".mise.toml", ".tool-versions", "mise.toml", ".mise/config.toml"}
}

// trustedConfigPathsEnv returns the MISE_TRUSTED_CONFIG_PATHS=... env var
// string, or empty if no trusted roots are set.
func (m *miseEnvProvider) trustedConfigPathsEnv() string {
	if roots := m.trustedRoots.Load(); roots != nil && len(*roots) > 0 {
		return "MISE_TRUSTED_CONFIG_PATHS=" + strings.Join(*roots, ":")
	}

	return ""
}

const captureTimeout = 10 * time.Second

func (m *miseEnvProvider) CaptureEnv(ctx context.Context, dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()

	normalizedDir, err := filesystem.Normalize(dir)
	if err != nil {
		return nil, errors.WrapPrefix(err, "error normalizing directory for env capture")
	}

	cmd := exec.CommandContext(ctx, "mise", "env", "-C", normalizedDir)
	cmd.Dir = normalizedDir

	if trustEnv := m.trustedConfigPathsEnv(); trustEnv != "" {
		cmd.Env = append(os.Environ(), trustEnv)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.DebugContext(ctx, "mise env failed", "dir", normalizedDir, "error", err)

		return nil, nil
	}

	return parseMiseOutput(string(out)), nil
}

func parseMiseOutput(output string) []string {
	var env []string

	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "export ") {
			continue
		}

		kv := strings.TrimPrefix(line, "export ")
		if idx := strings.Index(kv, "="); idx > 0 {
			key := kv[:idx]
			val := kv[idx+1:]
			val = stripMatchedQuotes(val)
			env = append(env, key+"="+val)
		}
	}

	return env
}

type cachedEnv struct {
	pathDirs    []string
	configMtime time.Time
	configPath  string
	negative    bool
	negativeAt  time.Time
	// capturedAt tracks when a configPath-less entry was captured,
	// so Refresh can skip re-capturing if it was recent.
	capturedAt time.Time
}

// refreshInterval controls how often entries without a trackable config file
// (e.g. mise walking up parent dirs) are re-captured.
const refreshInterval = 30 * time.Second

const negativeCacheTTL = 5 * time.Minute

// EnvProviderSet manages multiple EnvProviders with caching.
type EnvProviderSet struct {
	providers []EnvProvider
	cache     map[string]*cachedEnv
	mu        sync.RWMutex
}

// NewEnvProviderSet creates an EnvProviderSet, probing for installed providers.
func NewEnvProviderSet() *EnvProviderSet {
	return NewEnvProviderSetWithProviders(
		&miseEnvProvider{},
	)
}

// NewEnvProviderSetWithProviders creates an EnvProviderSet with explicitly provided providers.
// Only providers that pass Detect() are kept.
func NewEnvProviderSetWithProviders(candidates ...EnvProvider) *EnvProviderSet {
	var detected []EnvProvider

	for _, p := range candidates {
		if p.Detect() {
			detected = append(detected, p)
		}
	}

	return &EnvProviderSet{
		providers: detected,
		cache:     map[string]*cachedEnv{},
	}
}

// DiscoverExtraSearchPaths discovers the environment for the given seed directories plus all directories already
// in the cache. Entries whose config files no longer exist are removed. seedDirs are also used as trusted roots for
// mise.
func (eps *EnvProviderSet) DiscoverExtraSearchPaths(ctx context.Context, seedDirs []string) map[string][]string {
	if len(eps.providers) == 0 {
		return nil
	}

	// Update trusted roots on the mise provider so configs in seed dirs
	// are auto-trusted without requiring `mise trust`.
	for _, p := range eps.providers {
		if mp, ok := p.(*miseEnvProvider); ok {
			mp.trustedRoots.Store(&seedDirs)
		}
	}

	discovedDirs := make(map[string][]string, len(seedDirs))
	for _, dir := range seedDirs {
		discovedDirs[dir] = eps.discoverExtraSearchPaths(ctx, dir)
	}

	return discovedDirs
}

func (eps *EnvProviderSet) discoverExtraSearchPaths(ctx context.Context, dir string) []string {
	eps.mu.RLock()
	existing, hasCached := eps.cache[dir]
	eps.mu.RUnlock()

	if hasCached {
		switch {
		case existing.negative:
			if time.Since(existing.negativeAt) < negativeCacheTTL {
				return existing.pathDirs
			}
		case existing.configPath != "":
			info, err := os.Stat(existing.configPath)
			if err != nil {
				eps.mu.Lock()
				delete(eps.cache, dir)
				eps.mu.Unlock()

				return nil
			}

			if !info.ModTime().After(existing.configMtime) {
				return existing.pathDirs
			}
		default:
			// No trackable config file (e.g. mise walking up parent dirs).
			// Re-capture periodically instead of every tick.
			if time.Since(existing.capturedAt) < refreshInterval {
				return existing.pathDirs
			}
		}
	}

	return eps.discoverAndCacheSearchPaths(ctx, dir)
}

// discoverAndCacheSearchPaths captures env for a directory and stores it in the cache.
// Returns the captured env (nil if negative).
func (eps *EnvProviderSet) discoverAndCacheSearchPaths(ctx context.Context, dir string) []string {
	normalizedDir, err := filesystem.Normalize(dir)
	if err != nil {
		// best effort
		normalizedDir = dir
	}

	configPath, configMtime := eps.findConfigFile(normalizedDir)
	env := eps.captureFromProviders(ctx, normalizedDir)

	if env == nil && configPath == "" {
		eps.mu.Lock()
		eps.cache[dir] = &cachedEnv{negative: true, negativeAt: time.Now()}
		eps.mu.Unlock()

		return nil
	}

	pathDirs := extractPATHDirs(env)

	eps.mu.Lock()
	eps.cache[dir] = &cachedEnv{
		pathDirs:    pathDirs,
		configMtime: configMtime,
		configPath:  configPath,
		capturedAt:  time.Now(),
	}
	eps.mu.Unlock()

	return pathDirs
}

// ShellHookPrefix returns a bash DEBUG trap that re-evaluates mise
// before every simple command. This makes env activation directory-aware
// in non-interactive bash -c invocations without any command string parsing.
func (eps *EnvProviderSet) ShellHookPrefix() string {
	for _, p := range eps.providers {
		mp, ok := p.(*miseEnvProvider)
		if !ok {
			continue
		}

		var prefix string
		if trustEnv := mp.trustedConfigPathsEnv(); trustEnv != "" {
			prefix = "export " + trustEnv + "; "
		}

		return prefix + `trap 'eval "$(mise hook-env -s bash 2>/dev/null)"' DEBUG; `
	}

	return ""
}

// TrustEnv returns env vars needed so the user's shell hook works without
// manual `mise trust`.
func (eps *EnvProviderSet) TrustEnv() []string {
	for _, p := range eps.providers {
		mp, ok := p.(*miseEnvProvider)
		if !ok {
			continue
		}

		if trustEnv := mp.trustedConfigPathsEnv(); trustEnv != "" {
			return []string{trustEnv}
		}
	}

	return nil
}

// ExtraPATHDirs returns all PATH additions from all cached entries.
func (eps *EnvProviderSet) ExtraPATHDirs() []string {
	eps.mu.RLock()
	defer eps.mu.RUnlock()

	seen := map[string]struct{}{}

	var dirs []string

	for _, cached := range eps.cache {
		for _, d := range cached.pathDirs {
			if _, ok := seen[d]; !ok {
				seen[d] = struct{}{}
				dirs = append(dirs, d)
			}
		}
	}

	return dirs
}

func (eps *EnvProviderSet) findConfigFile(dir string) (string, time.Time) {
	for _, p := range eps.providers {
		for _, cfgName := range p.ConfigFiles() {
			cfgPath := filepath.Join(dir, cfgName)

			info, err := os.Stat(cfgPath)
			if err == nil {
				return cfgPath, info.ModTime()
			}
		}
	}

	return "", time.Time{}
}

func (eps *EnvProviderSet) captureFromProviders(ctx context.Context, dir string) []string {
	for _, p := range eps.providers {
		env, err := p.CaptureEnv(ctx, dir)
		if err != nil {
			continue
		}

		if env != nil {
			return env
		}
	}

	return nil
}

func stripMatchedQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}

	return s
}

func extractPATHDirs(env []string) []string {
	for _, kv := range env {
		if after, ok := strings.CutPrefix(kv, "PATH="); ok {
			var dirs []string

			for dir := range strings.SplitSeq(after, ":") {
				if dir != "" {
					dirs = append(dirs, dir)
				}
			}

			return dirs
		}
	}

	return nil
}
