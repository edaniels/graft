package main

import (
	"testing"

	"go.viam.com/test"
)

func TestParseDestinationRemoteDir(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantDest   string
		wantRemote string
	}{
		{"plain host", "myhost", "myhost", ""},
		{"user@host", "user@host", "user@host", ""},
		{"user@host with remote dir", "user@host:~/proj", "user@host", "~/proj"},
		{"user@host with absolute remote dir", "user@host:/home/user/proj", "user@host", "/home/user/proj"},
		{"host with port-like colon but treated as remote dir", "myhost:~/work", "myhost", "~/work"},
		{"complex user@host:dir", "deploy@192.168.1.1:~/apps/myapp", "deploy@192.168.1.1", "~/apps/myapp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest, remoteDir := parseDestinationRemoteDir(tt.input)
			test.That(t, dest, test.ShouldEqual, tt.wantDest)
			test.That(t, remoteDir, test.ShouldEqual, tt.wantRemote)
		})
	}
}

func TestResolveProjectConnectParams(t *testing.T) {
	t.Run("no workspace sync", func(t *testing.T) {
		params := resolveProjectConnectParams(resolveProjectConnectInput{
			projectDir:    "/home/user/myproject",
			destName:      "myconn",
			destConfig:    ProjectDestinationConfig{Host: "myhost", User: "ubuntu", SyncTo: "~/proj"},
			forwards:      []string{"make"},
			workspaceDir:  "",
			syncWorkspace: false,
		})

		test.That(t, params.Name, test.ShouldEqual, "myconn")
		test.That(t, params.LocalRoot, test.ShouldEqual, "/home/user/myproject")
		test.That(t, params.RemoteRoot, test.ShouldEqual, "~/proj")
		test.That(t, params.SyncSource, test.ShouldBeEmpty)
		test.That(t, params.SyncDest, test.ShouldBeEmpty)
		test.That(t, params.WithSync, test.ShouldBeFalse)
		test.That(t, params.ForwardCommands, test.ShouldResemble, []string{"make"})
		test.That(t, params.ForwardPrefix, test.ShouldBeFalse)
	})

	t.Run("with workspace sync", func(t *testing.T) {
		params := resolveProjectConnectParams(resolveProjectConnectInput{
			projectDir:    "/home/user/arc/infra/anvil/cluster",
			destName:      "anvil",
			destConfig:    ProjectDestinationConfig{Host: "anvil-host", User: "ubuntu", SyncTo: "~/arc"},
			forwards:      []string{"pulumi", "kubectl"},
			workspaceDir:  "/home/user/arc",
			syncWorkspace: true,
		})

		test.That(t, params.Name, test.ShouldEqual, "anvil")
		test.That(t, params.LocalRoot, test.ShouldEqual, "/home/user/arc/infra/anvil/cluster")
		test.That(t, params.RemoteRoot, test.ShouldEqual, "~/arc/infra/anvil/cluster")
		test.That(t, params.SyncSource, test.ShouldEqual, "/home/user/arc")
		test.That(t, params.SyncDest, test.ShouldEqual, "~/arc")
		test.That(t, params.WithSync, test.ShouldBeTrue)
		test.That(t, params.ForwardCommands, test.ShouldResemble, []string{"pulumi", "kubectl"})
		test.That(t, params.ForwardPrefix, test.ShouldBeFalse)
	})

	t.Run("with workspace sync and prefix", func(t *testing.T) {
		params := resolveProjectConnectParams(resolveProjectConnectInput{
			projectDir:    "/home/user/arc/infra/anvil/cluster",
			destName:      "anvil",
			destConfig:    ProjectDestinationConfig{Host: "anvil-host", User: "ubuntu", SyncTo: "~/arc", Prefix: true},
			forwards:      []string{"pulumi", "kubectl", "k9s"},
			workspaceDir:  "/home/user/arc",
			syncWorkspace: true,
		})

		test.That(t, params.ForwardPrefix, test.ShouldBeTrue)
	})

	t.Run("project at workspace root with sync", func(t *testing.T) {
		params := resolveProjectConnectParams(resolveProjectConnectInput{
			projectDir:    "/home/user/arc",
			destName:      "myconn",
			destConfig:    ProjectDestinationConfig{Host: "myhost", User: "ubuntu", SyncTo: "~/arc"},
			forwards:      nil,
			workspaceDir:  "/home/user/arc",
			syncWorkspace: true,
		})

		test.That(t, params.LocalRoot, test.ShouldEqual, "/home/user/arc")
		test.That(t, params.RemoteRoot, test.ShouldEqual, "~/arc")
		test.That(t, params.SyncSource, test.ShouldEqual, "/home/user/arc")
		test.That(t, params.SyncDest, test.ShouldEqual, "~/arc")
		test.That(t, params.WithSync, test.ShouldBeTrue)
	})

	t.Run("workspace present but sync disabled", func(t *testing.T) {
		params := resolveProjectConnectParams(resolveProjectConnectInput{
			projectDir:    "/home/user/arc/infra/anvil/cluster",
			destName:      "anvil",
			destConfig:    ProjectDestinationConfig{Host: "anvil-host", User: "ubuntu", SyncTo: "~/arc"},
			forwards:      []string{"pulumi"},
			workspaceDir:  "/home/user/arc",
			syncWorkspace: false,
		})

		test.That(t, params.LocalRoot, test.ShouldEqual, "/home/user/arc/infra/anvil/cluster")
		test.That(t, params.RemoteRoot, test.ShouldEqual, "~/arc")
		test.That(t, params.SyncSource, test.ShouldBeEmpty)
		test.That(t, params.SyncDest, test.ShouldBeEmpty)
		test.That(t, params.WithSync, test.ShouldBeFalse)
	})

	t.Run("project-level sync without workspace", func(t *testing.T) {
		params := resolveProjectConnectParams(resolveProjectConnectInput{
			projectDir:    "/home/user/myproject",
			destName:      "myconn",
			destConfig:    ProjectDestinationConfig{Host: "myhost", User: "ubuntu", SyncTo: "~/proj", Sync: true},
			forwards:      []string{"make"},
			workspaceDir:  "",
			syncWorkspace: false,
		})

		test.That(t, params.WithSync, test.ShouldBeTrue)
		test.That(t, params.RemoteRoot, test.ShouldEqual, "~/proj")
	})

	t.Run("project sync false means no sync without workspace", func(t *testing.T) {
		params := resolveProjectConnectParams(resolveProjectConnectInput{
			projectDir:    "/home/user/myproject",
			destName:      "myconn",
			destConfig:    ProjectDestinationConfig{Host: "myhost", User: "ubuntu", SyncTo: "~/proj", Sync: false},
			forwards:      []string{"make"},
			workspaceDir:  "",
			syncWorkspace: false,
		})

		test.That(t, params.WithSync, test.ShouldBeFalse)
	})
}

func TestConnectBackgroundFlag(t *testing.T) {
	t.Run("flag is registered and defaults to false", func(t *testing.T) {
		flag := connectCmd.Flags().Lookup("background")
		test.That(t, flag, test.ShouldNotBeNil)
		test.That(t, flag.DefValue, test.ShouldEqual, "false")
	})
}

func TestParseConnectArgs(t *testing.T) {
	t.Run("no args returns empty strings and no error", func(t *testing.T) {
		localDir, destination, err := parseConnectArgs(nil)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, localDir, test.ShouldBeEmpty)
		test.That(t, destination, test.ShouldBeEmpty)
	})

	t.Run("one arg is rejected", func(t *testing.T) {
		_, _, err := parseConnectArgs([]string{"user@host"})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "local_dir and destination required")
	})

	t.Run("two args returns local dir and destination", func(t *testing.T) {
		localDir, destination, err := parseConnectArgs([]string{".", "user@host:~/proj"})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, localDir, test.ShouldEqual, ".")
		test.That(t, destination, test.ShouldEqual, "user@host:~/proj")
	})

	t.Run("three or more args is rejected", func(t *testing.T) {
		_, _, err := parseConnectArgs([]string{".", "user@host", "extra"})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "local_dir and destination required")
	})
}

func TestParseSSHURLRemoteDir(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantDest   string
		wantRemote string
	}{
		{"no remote dir", "ssh://user@host", "ssh://user@host", ""},
		{"with remote dir", "ssh://user@host:~/proj", "ssh://user@host", "~/proj"},
		{"with absolute remote dir", "ssh://user@host:/home/user/proj", "ssh://user@host", "/home/user/proj"},
		{"port only", "ssh://user@host:22", "ssh://user@host:22", ""},
		{"ip with remote dir", "ssh://ec2-user@3.84.148.224:~/quic-go", "ssh://ec2-user@3.84.148.224", "~/quic-go"},
		{"port and remote dir", "ssh://user@host:22:~/proj", "ssh://user@host:22", "~/proj"},
		{"non-default port and remote dir", "ssh://user@host:2222:~/proj", "ssh://user@host:2222", "~/proj"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest, remoteDir := parseSSHURLRemoteDir(tt.input)
			test.That(t, dest, test.ShouldEqual, tt.wantDest)
			test.That(t, remoteDir, test.ShouldEqual, tt.wantRemote)
		})
	}
}
