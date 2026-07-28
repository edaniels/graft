package main

import (
	"testing"

	"go.viam.com/test"
)

func TestPartitionForwardArgs(t *testing.T) {
	t.Run("splits commands and ports", func(t *testing.T) {
		commands, ports := partitionForwardArgs([]string{"go", "8080", "make", "3000:8080/tcp"})
		test.That(t, commands, test.ShouldResemble, []string{"go", "make"})
		test.That(t, ports, test.ShouldResemble, []string{"8080", "3000:8080/tcp"})
	})

	t.Run("a command literally named agent is a command, not special", func(t *testing.T) {
		commands, ports := partitionForwardArgs([]string{"agent"})
		test.That(t, commands, test.ShouldResemble, []string{"agent"})
		test.That(t, ports, test.ShouldBeEmpty)
	})
}

// withForwardAgentFlags sets forwardAgentFlag/forwardRemoveAgentFlag for the
// duration of the test and restores their prior values after, since they're
// package-level vars shared with the real CLI commands.
func withForwardAgentFlags(t *testing.T, agent, removeAgent bool) {
	t.Helper()

	prevAgent, prevRemoveAgent := forwardAgentFlag, forwardRemoveAgentFlag
	forwardAgentFlag, forwardRemoveAgentFlag = agent, removeAgent

	t.Cleanup(func() {
		forwardAgentFlag, forwardRemoveAgentFlag = prevAgent, prevRemoveAgent
	})
}

// TestForwardAgentIsAFlagNotASubcommand verifies that "agent" always works as
// a literal forwarded command name via `graft forward agent`: --agent is a
// flag, not a subcommand, so it can never collide with a positional
// command/port argument (there are real tools named "agent", e.g. various
// *-agent daemons).
func TestForwardAgentIsAFlagNotASubcommand(t *testing.T) {
	t.Run("forward: 'agent' with no --agent flag is a normal positional arg", func(t *testing.T) {
		withForwardAgentFlags(t, false, false)

		err := forwardCmd.Args(forwardCmd, []string{"agent"})
		test.That(t, err, test.ShouldBeNil)

		commands, _ := partitionForwardArgs([]string{"agent"})
		test.That(t, commands, test.ShouldResemble, []string{"agent"})
	})

	t.Run("forward: --agent with no positional args is valid", func(t *testing.T) {
		withForwardAgentFlags(t, true, false)

		err := forwardCmd.Args(forwardCmd, []string{})
		test.That(t, err, test.ShouldBeNil)
	})

	t.Run("forward: --agent rejects extra positional args", func(t *testing.T) {
		withForwardAgentFlags(t, true, false)

		err := forwardCmd.Args(forwardCmd, []string{"agent"})
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("forward: no --agent and no args is rejected", func(t *testing.T) {
		withForwardAgentFlags(t, false, false)

		err := forwardCmd.Args(forwardCmd, []string{})
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("forward remove: 'agent' with no --agent flag is a normal positional arg", func(t *testing.T) {
		withForwardAgentFlags(t, false, false)

		err := forwardRemoveCmd.Args(forwardRemoveCmd, []string{"agent"})
		test.That(t, err, test.ShouldBeNil)
	})

	t.Run("forward remove: --agent with no positional args is valid", func(t *testing.T) {
		withForwardAgentFlags(t, false, true)

		err := forwardRemoveCmd.Args(forwardRemoveCmd, []string{})
		test.That(t, err, test.ShouldBeNil)
	})

	t.Run("forward remove: --agent rejects extra positional args", func(t *testing.T) {
		withForwardAgentFlags(t, false, true)

		err := forwardRemoveCmd.Args(forwardRemoveCmd, []string{"agent"})
		test.That(t, err, test.ShouldNotBeNil)
	})
}
