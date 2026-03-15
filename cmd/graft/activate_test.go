package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.viam.com/test"
)

var testGraftBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "graft-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}

	testGraftBinary = filepath.Join(dir, "graft")

	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", testGraftBinary, ".")

	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "failed to build graft: %v\n%s\n", err, output)
		os.Exit(1)
	}

	code := m.Run()

	os.RemoveAll(dir)
	os.Exit(code)
}

func shellAvailable(shell string) bool {
	_, err := exec.LookPath(shell)

	return err == nil
}

func TestActivateZsh(t *testing.T) {
	if !shellAvailable("zsh") {
		t.Skip("zsh not available on this system")
	}

	tmpHome := t.TempDir()

	script := fmt.Sprintf(`
autoload -Uz compinit && compinit -u
eval "$(%s activate zsh)"

# gr must be an alias for graft
[[ "$(whence -w gr)" == "gr: alias" ]] || { echo "FAIL: gr is not an alias: $(whence -w gr)"; exit 1; }

# completions must be registered for both graft and gr
[[ "${_comps[graft]}" == "_graft" ]] || { echo "FAIL: graft completion not registered, got: ${_comps[graft]}"; exit 2; }
[[ "${_comps[gr]}" == "_graft" ]] || { echo "FAIL: gr completion not registered, got: ${_comps[gr]}"; exit 3; }
`, testGraftBinary)

	cmd := exec.CommandContext(context.Background(), "zsh", "-f", "-c", script)

	cmd.Env = append(os.Environ(), "HOME="+tmpHome)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("zsh test output:\n%s", string(output))
	}

	test.That(t, err, test.ShouldBeNil)
}

func TestActivateBash(t *testing.T) {
	if !shellAvailable("bash") {
		t.Skip("bash not available on this system")
	}

	tmpHome := t.TempDir()

	script := fmt.Sprintf(`
eval "$(%s activate bash)"

# gr must be an alias for graft
alias gr &>/dev/null || { echo "FAIL: gr alias not defined"; exit 1; }

# completions must be registered for both graft and gr
complete -p graft 2>/dev/null | grep -q __start_graft || { echo "FAIL: graft completion not registered"; exit 2; }
complete -p gr 2>/dev/null | grep -q __start_graft || { echo "FAIL: gr completion not registered"; exit 3; }
`, testGraftBinary)

	cmd := exec.CommandContext(context.Background(), "bash", "--norc", "--noprofile", "-c", script)

	cmd.Env = append(os.Environ(), "HOME="+tmpHome)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("bash test output:\n%s", string(output))
	}

	test.That(t, err, test.ShouldBeNil)
}

func TestActivateFish(t *testing.T) {
	if !shellAvailable("fish") {
		t.Skip("fish not available on this system")
	}

	tmpHome := t.TempDir()

	script := fmt.Sprintf(`
%s activate fish | source 2>/dev/null

# gr must be a function wrapping graft
functions -q gr; or begin; echo "FAIL: gr function not defined"; exit 1; end

# completions for gr must wrap graft
complete --command gr | grep -q graft; or begin; echo "FAIL: gr completion not wrapping graft"; exit 2; end
`, testGraftBinary)

	cmd := exec.CommandContext(context.Background(), "fish", "--no-config", "-c", script)

	cmd.Env = append(os.Environ(), "HOME="+tmpHome)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("fish test output:\n%s", string(output))
	}

	test.That(t, err, test.ShouldBeNil)
}

func TestActivateUnknownShell(t *testing.T) {
	_, err := generateActivateScript("powershell", "/usr/local/bin/graft")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "unknown shell")
}

func TestActivateScriptConnectionPrompt(t *testing.T) {
	exePath := "/usr/local/bin/graft"

	for _, shell := range []string{"zsh", "bash", "fish"} {
		t.Run(shell, func(t *testing.T) {
			script, err := generateActivateScript(shell, exePath)
			test.That(t, err, test.ShouldBeNil)

			// Script must NOT read per-session current_connection file (uses connection_roots only)
			test.That(t, script, test.ShouldNotContainSubstring, "current_connection")

			// Script uses global connection_roots file for immediate CWD matching
			test.That(t, script, test.ShouldContainSubstring, "connection_roots")

			// Script sets GRAFT_CONNECTION env var
			test.That(t, script, test.ShouldContainSubstring, "GRAFT_CONNECTION")

			// Script respects GRAFT_PROMPT_DISABLE opt-out
			test.That(t, script, test.ShouldContainSubstring, "GRAFT_PROMPT_DISABLE")
		})
	}
}

func TestActivateScriptConnectionPromptPrefix(t *testing.T) {
	exePath := "/usr/local/bin/graft"

	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			script, err := generateActivateScript(shell, exePath)
			test.That(t, err, test.ShouldBeNil)

			// Prompt prefix uses [connection] format
			test.That(t, script, test.ShouldContainSubstring, "GRAFT_CONNECTION")
			test.That(t, script, test.ShouldContainSubstring, "[")
		})
	}

	t.Run("fish", func(t *testing.T) {
		script, err := generateActivateScript("fish", exePath)
		test.That(t, err, test.ShouldBeNil)

		// Fish wraps fish_prompt and outputs [connection] prefix
		test.That(t, script, test.ShouldContainSubstring, "fish_prompt")
		test.That(t, script, test.ShouldContainSubstring, "[$GRAFT_CONNECTION]")
	})
}
