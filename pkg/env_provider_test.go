package graft

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.viam.com/test"
)

const testMisePath = "/usr/bin/mise"

type mockEnvProvider struct {
	detected   bool
	cfgFiles   []string
	captureEnv func(ctx context.Context, dir string) ([]string, error)
	captures   int
}

func (m *mockEnvProvider) Detect() bool          { return m.detected }
func (m *mockEnvProvider) ConfigFiles() []string { return m.cfgFiles }

func (m *mockEnvProvider) CaptureEnv(ctx context.Context, dir string) ([]string, error) {
	m.captures++

	if m.captureEnv != nil {
		return m.captureEnv(ctx, dir)
	}

	return nil, nil
}

func TestParseMiseOutput(t *testing.T) {
	t.Run("standard output", func(t *testing.T) {
		output := `export FOO=bar
export PATH=/mise/bin:/usr/bin
export BAZ="hello world"
`
		env := parseMiseOutput(output)
		test.That(t, len(env), test.ShouldEqual, 3)
		test.That(t, env[0], test.ShouldEqual, "FOO=bar")
		test.That(t, env[1], test.ShouldEqual, "PATH=/mise/bin:/usr/bin")
		test.That(t, env[2], test.ShouldEqual, "BAZ=hello world")
	})

	t.Run("empty output", func(t *testing.T) {
		test.That(t, parseMiseOutput(""), test.ShouldBeNil)
	})

	t.Run("non-export lines ignored", func(t *testing.T) {
		output := "# comment\nexport FOO=bar\nsome other line\n"
		env := parseMiseOutput(output)
		test.That(t, len(env), test.ShouldEqual, 1)
		test.That(t, env[0], test.ShouldEqual, "FOO=bar")
	})

	t.Run("quoted values", func(t *testing.T) {
		env := parseMiseOutput("export A='bar baz'\nexport B=\"bar'baz\"\n")
		test.That(t, len(env), test.ShouldEqual, 2)
		test.That(t, env[0], test.ShouldEqual, "A=bar baz")
		test.That(t, env[1], test.ShouldEqual, "B=bar'baz")
	})

	t.Run("value containing equals", func(t *testing.T) {
		env := parseMiseOutput("export URL=postgres://host/db?opt=val\n")
		test.That(t, len(env), test.ShouldEqual, 1)
		test.That(t, env[0], test.ShouldEqual, "URL=postgres://host/db?opt=val")
	})

	t.Run("mismatched quotes not stripped", func(t *testing.T) {
		env := parseMiseOutput("export FOO=\"bar'\n")
		test.That(t, env[0], test.ShouldEqual, "FOO=\"bar'")
	})
}

func TestMiseEnvProvider_Detect(t *testing.T) {
	notInstalled := &miseEnvProvider{
		lookPath: func(string) (string, error) {
			return "", &os.PathError{Op: "lookpath", Path: "mise", Err: os.ErrNotExist}
		},
	}
	test.That(t, notInstalled.Detect(), test.ShouldBeFalse)

	installed := &miseEnvProvider{
		lookPath: func(string) (string, error) { return testMisePath, nil },
	}
	test.That(t, installed.Detect(), test.ShouldBeTrue)
}

func TestExtractPATHDirs(t *testing.T) {
	dirs := extractPATHDirs([]string{"FOO=bar", "PATH=/a:/b:/c", "BAZ=qux"})
	test.That(t, len(dirs), test.ShouldEqual, 3)
	test.That(t, dirs[0], test.ShouldEqual, "/a")
	test.That(t, dirs[1], test.ShouldEqual, "/b")
	test.That(t, dirs[2], test.ShouldEqual, "/c")

	test.That(t, extractPATHDirs([]string{"FOO=bar"}), test.ShouldBeNil)
	test.That(t, extractPATHDirs(nil), test.ShouldBeNil)
}

func TestEnvProviderSet_NoProviders(t *testing.T) {
	eps := NewEnvProviderSetWithProviders()

	eps.Refresh(context.Background(), []string{"/some/dir"})
	test.That(t, eps.EnvForDir(context.Background(), "/some/dir"), test.ShouldBeNil)
	test.That(t, eps.ExtraPATHDirs(), test.ShouldBeNil)
	test.That(t, eps.ShellHookPrefix(), test.ShouldEqual, "")
	test.That(t, eps.TrustEnv(), test.ShouldBeNil)
}

func TestEnvProviderSet_UndetectedProviders(t *testing.T) {
	eps := NewEnvProviderSetWithProviders(&mockEnvProvider{detected: false, cfgFiles: []string{".mockrc"}})
	test.That(t, len(eps.providers), test.ShouldEqual, 0)
}

func TestEnvProviderSet_EnvForDir(t *testing.T) {
	tmpDir := t.TempDir()
	test.That(t, os.WriteFile(filepath.Join(tmpDir, ".mockrc"), []byte("config"), 0o600), test.ShouldBeNil)

	mock := &mockEnvProvider{
		detected: true,
		cfgFiles: []string{".mockrc"},
		captureEnv: func(_ context.Context, _ string) ([]string, error) {
			return []string{"FOO=bar"}, nil
		},
	}

	eps := NewEnvProviderSetWithProviders(mock)

	// First call captures on-demand.
	env := eps.EnvForDir(context.Background(), tmpDir)
	test.That(t, len(env), test.ShouldEqual, 1)
	test.That(t, env[0], test.ShouldEqual, "FOO=bar")
	test.That(t, mock.captures, test.ShouldEqual, 1)

	// Second call hits cache.
	env = eps.EnvForDir(context.Background(), tmpDir)
	test.That(t, env[0], test.ShouldEqual, "FOO=bar")
	test.That(t, mock.captures, test.ShouldEqual, 1)
}

func TestEnvProviderSet_NegativeCache(t *testing.T) {
	tmpDir := t.TempDir()

	mock := &mockEnvProvider{
		detected:   true,
		cfgFiles:   []string{".mockrc"},
		captureEnv: func(_ context.Context, _ string) ([]string, error) { return nil, nil },
	}

	eps := NewEnvProviderSetWithProviders(mock)

	test.That(t, eps.EnvForDir(context.Background(), tmpDir), test.ShouldBeNil)
	test.That(t, mock.captures, test.ShouldEqual, 1)

	// Negative cache hit.
	test.That(t, eps.EnvForDir(context.Background(), tmpDir), test.ShouldBeNil)
	test.That(t, mock.captures, test.ShouldEqual, 1)
}

func TestEnvProviderSet_RefreshRecapturesOnMtimeChange(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, ".mockrc")
	test.That(t, os.WriteFile(cfgPath, []byte("config"), 0o600), test.ShouldBeNil)

	callCount := 0
	mock := &mockEnvProvider{
		detected: true,
		cfgFiles: []string{".mockrc"},
		captureEnv: func(_ context.Context, _ string) ([]string, error) {
			callCount++

			return []string{"A=" + string(rune('0'+callCount))}, nil
		},
	}

	eps := NewEnvProviderSetWithProviders(mock)
	ctx := context.Background()

	eps.Refresh(ctx, []string{tmpDir})
	test.That(t, mock.captures, test.ShouldEqual, 1)

	// Same mtime - skipped.
	eps.Refresh(ctx, []string{tmpDir})
	test.That(t, mock.captures, test.ShouldEqual, 1)

	// Touch config - recaptures.
	test.That(t, os.Chtimes(cfgPath, time.Now().Add(time.Second), time.Now().Add(time.Second)), test.ShouldBeNil)

	eps.Refresh(ctx, []string{tmpDir})
	test.That(t, mock.captures, test.ShouldEqual, 2)

	env := eps.EnvForDir(ctx, tmpDir)
	test.That(t, env[0], test.ShouldEqual, "A=2")
}

func TestEnvProviderSet_RefreshRemovesDeletedConfig(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, ".mockrc")
	test.That(t, os.WriteFile(cfgPath, []byte("config"), 0o600), test.ShouldBeNil)

	mock := &mockEnvProvider{
		detected: true,
		cfgFiles: []string{".mockrc"},
		captureEnv: func(_ context.Context, _ string) ([]string, error) {
			return []string{"A=1"}, nil
		},
	}

	eps := NewEnvProviderSetWithProviders(mock)
	ctx := context.Background()

	eps.Refresh(ctx, []string{tmpDir})
	test.That(t, len(eps.EnvForDir(ctx, tmpDir)), test.ShouldEqual, 1)

	test.That(t, os.Remove(cfgPath), test.ShouldBeNil)
	eps.Refresh(ctx, []string{tmpDir})

	mock.captureEnv = func(_ context.Context, _ string) ([]string, error) { return nil, nil }

	test.That(t, eps.EnvForDir(ctx, tmpDir), test.ShouldBeNil)
}

func TestShellHookPrefix(t *testing.T) {
	provider := &miseEnvProvider{
		lookPath: func(string) (string, error) { return testMisePath, nil },
	}

	eps := NewEnvProviderSetWithProviders(provider)
	prefix := eps.ShellHookPrefix()
	test.That(t, prefix, test.ShouldContainSubstring, "trap")
	test.That(t, prefix, test.ShouldContainSubstring, "DEBUG")
	test.That(t, prefix, test.ShouldContainSubstring, `mise hook-env -s bash`)

	// With trusted roots.
	eps.Refresh(t.Context(), []string{"/project/root"})
	prefix = eps.ShellHookPrefix()
	test.That(t, prefix, test.ShouldContainSubstring, "MISE_TRUSTED_CONFIG_PATHS=/project/root")
}

// TestDiscoveryGrowsOverTime simulates the full discovery lifecycle:
// periodic Refresh seeds initial directories, on-demand EnvForDir adds
// new ones, and ExtraPATHDirs grows to include all discovered PATH entries.
func TestDiscoveryGrowsOverTime(t *testing.T) {
	seedDir := t.TempDir()
	test.That(t, os.WriteFile(filepath.Join(seedDir, ".mockrc"), []byte("v1"), 0o600), test.ShouldBeNil)

	onDemandDir := t.TempDir()
	test.That(t, os.WriteFile(filepath.Join(onDemandDir, ".mockrc"), []byte("v1"), 0o600), test.ShouldBeNil)

	laterSeedDir := t.TempDir()
	test.That(t, os.WriteFile(filepath.Join(laterSeedDir, ".mockrc"), []byte("v1"), 0o600), test.ShouldBeNil)

	mock := &mockEnvProvider{
		detected: true,
		cfgFiles: []string{".mockrc"},
		captureEnv: func(_ context.Context, dir string) ([]string, error) {
			switch dir {
			case seedDir:
				return []string{"PATH=/tools/go/bin", "GOROOT=/tools/go"}, nil
			case onDemandDir:
				return []string{"PATH=/tools/node/bin", "NODE_ENV=development"}, nil
			case laterSeedDir:
				return []string{"PATH=/tools/python/bin", "PYTHONPATH=/app"}, nil
			default:
				return nil, nil
			}
		},
	}

	eps := NewEnvProviderSetWithProviders(mock)
	ctx := context.Background()

	// Phase 1: initial Refresh with one seed directory.
	eps.Refresh(ctx, []string{seedDir})
	test.That(t, mock.captures, test.ShouldEqual, 1)

	pathDirs := eps.ExtraPATHDirs()
	test.That(t, len(pathDirs), test.ShouldEqual, 1)
	test.That(t, pathDirs[0], test.ShouldEqual, "/tools/go/bin")

	// Phase 2: on-demand EnvForDir adds a new directory.
	env := eps.EnvForDir(ctx, onDemandDir)
	test.That(t, len(env), test.ShouldEqual, 2)
	test.That(t, mock.captures, test.ShouldEqual, 2)

	pathDirs = eps.ExtraPATHDirs()
	test.That(t, len(pathDirs), test.ShouldEqual, 2)

	pathSet := map[string]bool{}
	for _, d := range pathDirs {
		pathSet[d] = true
	}

	test.That(t, pathSet["/tools/go/bin"], test.ShouldBeTrue)
	test.That(t, pathSet["/tools/node/bin"], test.ShouldBeTrue)

	// Phase 3: second connection adds another seed directory.
	eps.Refresh(ctx, []string{seedDir, laterSeedDir})
	test.That(t, mock.captures, test.ShouldEqual, 3)

	pathDirs = eps.ExtraPATHDirs()
	test.That(t, len(pathDirs), test.ShouldEqual, 3)

	pathSet = map[string]bool{}
	for _, d := range pathDirs {
		pathSet[d] = true
	}

	test.That(t, pathSet["/tools/go/bin"], test.ShouldBeTrue)
	test.That(t, pathSet["/tools/node/bin"], test.ShouldBeTrue)
	test.That(t, pathSet["/tools/python/bin"], test.ShouldBeTrue)

	// Phase 4: unchanged dirs are not re-captured.
	eps.Refresh(ctx, []string{seedDir, laterSeedDir})
	test.That(t, mock.captures, test.ShouldEqual, 3)
}
