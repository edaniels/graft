package graft

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.viam.com/test"
)

func TestCollectCommandsFromPATH(t *testing.T) {
	// The profile PATH refresh is exercised by its own tests; disable it here
	// so this test can exercise collectCommandsFromPATH with a deliberately
	// constructed PATH without interference from bash -ic capturing a different
	// one from the test runner's shell profile.
	disablePATHRefresh(t)

	tempDirGlobal := t.TempDir()
	tempDirByDir1 := t.TempDir()
	tempDirByDir2 := t.TempDir()

	exec1, err := os.Create(filepath.Join(tempDirGlobal, "exec1"))
	test.That(t, err, test.ShouldBeNil)

	exec2, err := os.Create(filepath.Join(tempDirGlobal, "exec2"))
	test.That(t, err, test.ShouldBeNil)

	exec3, err := os.Create(filepath.Join(tempDirByDir1, "exec3"))
	test.That(t, err, test.ShouldBeNil)
	exec4, err := os.Create(filepath.Join(tempDirByDir1, "exec4"))
	test.That(t, err, test.ShouldBeNil)

	exec5, err := os.Create(filepath.Join(tempDirByDir2, "exec5"))
	test.That(t, err, test.ShouldBeNil)
	exec6, err := os.Create(filepath.Join(tempDirByDir2, "exec6"))
	test.That(t, err, test.ShouldBeNil)

	newPath := tempDirGlobal

	curPath := os.Getenv("PATH")
	if curPath != "" {
		newPath = newPath + ":" + curPath
	}

	t.Setenv("PATH", newPath)

	collected, byDir := collectCommandsFromPATH(nil)
	test.That(t, collected, test.ShouldNotContain, exec1.Name())
	test.That(t, collected, test.ShouldNotContain, exec2.Name())
	test.That(t, byDir, test.ShouldBeEmpty)

	test.That(t, exec1.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec2.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec3.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec4.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec5.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec6.Chmod(0o777), test.ShouldBeNil)

	collected, byDir = collectCommandsFromPATH(map[string][]string{
		"prov1": {tempDirByDir1, tempDirByDir2},
		"prov2": {tempDirByDir2},
	})
	test.That(t, collected, test.ShouldContain, exec1.Name())
	test.That(t, collected, test.ShouldContain, exec2.Name())
	test.That(t, byDir, test.ShouldResemble, map[string][]string{
		"prov1": {exec3.Name(), exec4.Name(), exec5.Name(), exec6.Name()},
		"prov2": {exec5.Name(), exec6.Name()},
	})
}

// TestRefreshPATHFromShellInit exercises the whole refresh pipeline; marker
// parsing, dedupe, $PATH rewrite, shell gating, error handling, TTL; through
// the public entrypoint. Helper-level tests would be redundant.
func TestRefreshPATHFromShellInit(t *testing.T) {
	prevRunner := refreshPATHShellRunner

	t.Cleanup(func() {
		refreshPATHShellRunner = prevRunner

		resetFreshPATHCacheForTest()
	})

	t.Run("bash: sources rc + profile files, markers isolate PATH, dedup applies", func(t *testing.T) {
		resetFreshPATHCacheForTest()
		t.Setenv("SHELL", "/bin/bash")
		t.Setenv("PATH", "/original:/bin")

		calls := 0
		refreshPATHShellRunner = func(_ context.Context, shell string, args ...string) ([]byte, error) {
			calls++

			test.That(t, shell, test.ShouldEqual, "/bin/bash")
			test.That(t, args[0], test.ShouldEqual, "-ic")
			// `-i` already sources ~/.bashrc; the script must additionally source
			// the user-level profile files so installer-modified .profile etc.
			// take effect, but never /etc/profile (which would clobber).
			script := args[1]
			test.That(t, script, test.ShouldContainSubstring, "~/.profile")
			test.That(t, script, test.ShouldContainSubstring, "~/.bash_profile")
			test.That(t, script, test.ShouldNotContainSubstring, "/etc/profile")

			return []byte("Welcome!\n__GRAFT_PATH_START__\n/new/bin:/original:/bin:/new/bin\n__GRAFT_PATH_END__\n"), nil
		}

		refreshPATHFromShellInit()
		// Second call inside the TTL window must not re-exec.
		refreshPATHFromShellInit()

		test.That(t, calls, test.ShouldEqual, 1)
		test.That(t, os.Getenv("PATH"), test.ShouldEqual, "/new/bin:/original:/bin")
	})

	t.Run("zsh: sources zprofile and zlogin, never /etc/profile", func(t *testing.T) {
		resetFreshPATHCacheForTest()
		t.Setenv("SHELL", "/bin/zsh")
		t.Setenv("PATH", "/orig")

		refreshPATHShellRunner = func(_ context.Context, shell string, args ...string) ([]byte, error) {
			test.That(t, shell, test.ShouldEqual, "/bin/zsh")

			script := args[1]
			test.That(t, script, test.ShouldContainSubstring, "~/.zprofile")
			test.That(t, script, test.ShouldContainSubstring, "~/.zlogin")
			test.That(t, script, test.ShouldNotContainSubstring, "/etc/profile")

			return []byte("__GRAFT_PATH_START__\n/zsh/bin:/orig\n__GRAFT_PATH_END__\n"), nil
		}

		refreshPATHFromShellInit()
		test.That(t, os.Getenv("PATH"), test.ShouldEqual, "/zsh/bin:/orig")
	})

	t.Run("unsupported shell is a no-op", func(t *testing.T) {
		resetFreshPATHCacheForTest()
		t.Setenv("SHELL", "/usr/bin/fish")
		t.Setenv("PATH", "/sentinel")

		refreshPATHShellRunner = func(context.Context, string, ...string) ([]byte, error) {
			t.Fatalf("runner should not be invoked for unsupported shell")

			return nil, nil
		}

		refreshPATHFromShellInit()
		test.That(t, os.Getenv("PATH"), test.ShouldEqual, "/sentinel")
	})

	t.Run("exec error leaves PATH unchanged and respects TTL", func(t *testing.T) {
		resetFreshPATHCacheForTest()
		t.Setenv("SHELL", "/bin/bash")
		t.Setenv("PATH", "/untouched")

		calls := 0
		refreshPATHShellRunner = func(context.Context, string, ...string) ([]byte, error) {
			calls++

			return nil, os.ErrInvalid
		}

		refreshPATHFromShellInit()
		refreshPATHFromShellInit()

		test.That(t, calls, test.ShouldEqual, 1) // second call cache-hits on the advanced TTL timestamp
		test.That(t, os.Getenv("PATH"), test.ShouldEqual, "/untouched")
	})

	t.Run("missing markers leaves PATH unchanged", func(t *testing.T) {
		resetFreshPATHCacheForTest()
		t.Setenv("SHELL", "/bin/bash")
		t.Setenv("PATH", "/keep")

		refreshPATHShellRunner = func(context.Context, string, ...string) ([]byte, error) {
			return []byte("some unrelated output\n"), nil
		}

		refreshPATHFromShellInit()
		test.That(t, os.Getenv("PATH"), test.ShouldEqual, "/keep")
	})
}

// disablePATHRefresh forces refreshPATHFromShellInit to be a cache hit
// for the duration of the test so it doesn't perturb $PATH while the test is
// exercising an unrelated PATH-scanning code path.
func disablePATHRefresh(t *testing.T) {
	t.Helper()

	freshPATHMu.Lock()

	prev := freshPATHAt
	freshPATHAt = time.Now().Add(24 * time.Hour)

	freshPATHMu.Unlock()

	t.Cleanup(func() {
		freshPATHMu.Lock()

		freshPATHAt = prev

		freshPATHMu.Unlock()
	})
}

func resetFreshPATHCacheForTest() {
	freshPATHMu.Lock()
	defer freshPATHMu.Unlock()

	freshPATHValue = ""
	freshPATHAt = time.Time{}
}
