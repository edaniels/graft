package graft

import (
	"context"
	"fmt"
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

	discovered := eps.DiscoverExtraSearchPaths(context.Background(), []string{"/some/dir"})
	test.That(t, discovered, test.ShouldBeEmpty)
	test.That(t, eps.ShellHookPrefix(), test.ShouldEqual, "")
	test.That(t, eps.TrustEnv(), test.ShouldBeNil)
}

func TestEnvProviderSet_UndetectedProviders(t *testing.T) {
	eps := NewEnvProviderSetWithProviders(&mockEnvProvider{detected: false, cfgFiles: []string{".mockrc"}})
	test.That(t, len(eps.providers), test.ShouldEqual, 0)
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

			return []string{"PATH=" + fmt.Sprintf("FOO_%d:BAR", callCount)}, nil
		},
	}

	eps := NewEnvProviderSetWithProviders(mock)
	ctx := context.Background()

	_ = eps.DiscoverExtraSearchPaths(ctx, []string{tmpDir})

	test.That(t, mock.captures, test.ShouldEqual, 1)

	// Same mtime - skipped.
	_ = eps.DiscoverExtraSearchPaths(ctx, []string{tmpDir})

	test.That(t, mock.captures, test.ShouldEqual, 1)

	// Touch config - recaptures.
	test.That(t, os.Chtimes(cfgPath, time.Now().Add(time.Second), time.Now().Add(time.Second)), test.ShouldBeNil)

	discovered := eps.DiscoverExtraSearchPaths(ctx, []string{tmpDir})

	test.That(t, mock.captures, test.ShouldEqual, 2)

	test.That(t, discovered, test.ShouldResemble, map[string][]string{
		tmpDir: {"FOO_2", "BAR"},
	})
}

func TestEnvProviderSet_RefreshRemovesDeletedConfig(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, ".mockrc")
	test.That(t, os.WriteFile(cfgPath, []byte("config"), 0o600), test.ShouldBeNil)

	mock := &mockEnvProvider{
		detected: true,
		cfgFiles: []string{".mockrc"},
		captureEnv: func(_ context.Context, _ string) ([]string, error) {
			return []string{"PATH=FOO"}, nil
		},
	}

	eps := NewEnvProviderSetWithProviders(mock)
	ctx := context.Background()

	discovered := eps.DiscoverExtraSearchPaths(ctx, []string{tmpDir})
	test.That(t, discovered[tmpDir], test.ShouldHaveLength, 1)

	test.That(t, os.Remove(cfgPath), test.ShouldBeNil)

	discovered = eps.DiscoverExtraSearchPaths(ctx, []string{tmpDir})

	mock.captureEnv = func(_ context.Context, _ string) ([]string, error) { return nil, nil }

	test.That(t, discovered[tmpDir], test.ShouldHaveLength, 0)
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
	eps.DiscoverExtraSearchPaths(t.Context(), []string{"/project/root"})
	prefix = eps.ShellHookPrefix()
	test.That(t, prefix, test.ShouldContainSubstring, "MISE_TRUSTED_CONFIG_PATHS=/project/root")
}
