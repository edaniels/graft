package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/test"
	"gopkg.in/yaml.v2"
)

func TestBuildWorkspaceConfig(t *testing.T) {
	cfg := buildWorkspaceConfig()

	test.That(t, cfg.Version, test.ShouldEqual, "v1")
	test.That(t, cfg.Workspace, test.ShouldBeTrue)
	test.That(t, cfg.Defaults.SyncWorkspace, test.ShouldBeTrue)

	// Round-trip: marshal and unmarshal should produce the same struct.
	data, err := yaml.Marshal(cfg)
	test.That(t, err, test.ShouldBeNil)

	var decoded WorkspaceConfig

	err = yaml.Unmarshal(data, &decoded)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, decoded, test.ShouldResemble, cfg)
}

func TestBuildProjectConfig(t *testing.T) {
	t.Run("basic with user@host", func(t *testing.T) {
		cfg := buildProjectConfig("anvil", "ubuntu@anvil-host", "", nil, false)

		test.That(t, cfg.Version, test.ShouldEqual, "v1")
		test.That(t, cfg.Forwards, test.ShouldBeEmpty)
		test.That(t, cfg.Destinations, test.ShouldHaveLength, 1)

		dest := cfg.Destinations["anvil"]
		test.That(t, dest.Host, test.ShouldEqual, "anvil-host")
		test.That(t, dest.User, test.ShouldEqual, "ubuntu")
		test.That(t, dest.SyncTo, test.ShouldBeEmpty)
		test.That(t, dest.Prefix, test.ShouldBeFalse)
	})

	t.Run("basic without user", func(t *testing.T) {
		cfg := buildProjectConfig("myconn", "myhost", "", nil, false)

		dest := cfg.Destinations["myconn"]
		test.That(t, dest.Host, test.ShouldEqual, "myhost")
		test.That(t, dest.User, test.ShouldBeEmpty)
	})

	t.Run("with sync-to", func(t *testing.T) {
		cfg := buildProjectConfig("anvil", "ubuntu@anvil-host", "~/arc", nil, false)

		dest := cfg.Destinations["anvil"]
		test.That(t, dest.SyncTo, test.ShouldEqual, "~/arc")
	})

	t.Run("with forwards", func(t *testing.T) {
		cfg := buildProjectConfig("anvil", "ubuntu@anvil-host", "", []string{"pulumi", "kubectl", "k9s"}, false)

		test.That(t, cfg.Forwards, test.ShouldResemble, []string{"pulumi", "kubectl", "k9s"})
		test.That(t, cfg.Destinations["anvil"].Prefix, test.ShouldBeFalse)
	})

	t.Run("with forwards and prefix", func(t *testing.T) {
		cfg := buildProjectConfig("anvil", "ubuntu@anvil-host", "~/arc", []string{"pulumi", "kubectl", "k9s"}, true)

		test.That(t, cfg.Forwards, test.ShouldResemble, []string{"pulumi", "kubectl", "k9s"})
		test.That(t, cfg.Destinations["anvil"].Prefix, test.ShouldBeTrue)
		test.That(t, cfg.Destinations["anvil"].SyncTo, test.ShouldEqual, "~/arc")
	})

	t.Run("round-trip with connectFromProject structs", func(t *testing.T) {
		cfg := buildProjectConfig("myconn", "user@host", "~/proj", []string{"make", "go"}, true)

		data, err := yaml.Marshal(cfg)
		test.That(t, err, test.ShouldBeNil)

		var decoded ProjectConfig

		err = yaml.Unmarshal(data, &decoded)
		test.That(t, err, test.ShouldBeNil)

		test.That(t, decoded.Version, test.ShouldEqual, "v1")
		test.That(t, decoded.Forwards, test.ShouldResemble, []string{"make", "go"})
		test.That(t, decoded.Destinations, test.ShouldHaveLength, 1)

		dest := decoded.Destinations["myconn"]
		test.That(t, dest.Host, test.ShouldEqual, "host")
		test.That(t, dest.User, test.ShouldEqual, "user")
		test.That(t, dest.SyncTo, test.ShouldEqual, "~/proj")
		test.That(t, dest.Prefix, test.ShouldBeTrue)
	})
}

func TestFindWorkspaceDir(t *testing.T) {
	t.Run("no workspace found", func(t *testing.T) {
		dir := t.TempDir()
		result, err := findWorkspaceDir(dir)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result, test.ShouldBeEmpty)
	})

	t.Run("workspace in parent", func(t *testing.T) {
		wsDir := t.TempDir()
		wsCfg := WorkspaceConfig{Version: "v1", Workspace: true, Defaults: WorkspaceConfigDefaults{SyncWorkspace: true}}
		data, err := yaml.Marshal(wsCfg)
		test.That(t, err, test.ShouldBeNil)
		err = os.WriteFile(filepath.Join(wsDir, "graft.yaml"), data, 0o600)
		test.That(t, err, test.ShouldBeNil)

		projDir := filepath.Join(wsDir, "infra", "anvil")
		err = os.MkdirAll(projDir, 0o755)
		test.That(t, err, test.ShouldBeNil)

		result, err := findWorkspaceDir(projDir)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result, test.ShouldEqual, wsDir)
	})

	t.Run("non-workspace graft.yaml in parent is ignored", func(t *testing.T) {
		parentDir := t.TempDir()
		// Write a project config (not a workspace) in the parent.
		projCfg := ProjectConfig{Version: "v1", Destinations: map[string]ProjectDestinationConfig{"x": {Host: "h"}}}
		data, err := yaml.Marshal(projCfg)
		test.That(t, err, test.ShouldBeNil)
		err = os.WriteFile(filepath.Join(parentDir, "graft.yaml"), data, 0o600)
		test.That(t, err, test.ShouldBeNil)

		childDir := filepath.Join(parentDir, "child")
		err = os.MkdirAll(childDir, 0o755)
		test.That(t, err, test.ShouldBeNil)

		result, err := findWorkspaceDir(childDir)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result, test.ShouldBeEmpty)
	})

	t.Run("workspace two levels up", func(t *testing.T) {
		wsDir := t.TempDir()
		wsCfg := WorkspaceConfig{Version: "v1", Workspace: true}
		data, err := yaml.Marshal(wsCfg)
		test.That(t, err, test.ShouldBeNil)
		err = os.WriteFile(filepath.Join(wsDir, "graft.yaml"), data, 0o600)
		test.That(t, err, test.ShouldBeNil)

		deepDir := filepath.Join(wsDir, "a", "b", "c")
		err = os.MkdirAll(deepDir, 0o755)
		test.That(t, err, test.ShouldBeNil)

		result, err := findWorkspaceDir(deepDir)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result, test.ShouldEqual, wsDir)
	})
}
