package graft

import (
	"path/filepath"
	"strings"
	"testing"
)

// testEnv holds environment variable settings for a test case.
type testEnv map[string]string

func (e testEnv) apply(t *testing.T) {
	t.Helper()

	for k, v := range e {
		t.Setenv(k, v)
	}
}

func TestGraftDirHelpers(t *testing.T) {
	homeDir := "/home/testuser"

	tests := []struct {
		name string
		env  testEnv
		fn   func(string) string
		want string
	}{
		// graftConfigHome tests - returns ~/.config/graft
		{
			"configHome default",
			testEnv{"GRAFT_CONFIG_HOME": "", "XDG_CONFIG_HOME": ""},
			graftConfigHome,
			filepath.Join(homeDir,
				".config",
				"graft"),
		},

		{
			"configHome xdg override",

			testEnv{
				"GRAFT_CONFIG_HOME": "",
				"XDG_CONFIG_HOME":   "/xdg/config",
			},
			graftConfigHome,
			"/xdg/config/graft",
		},

		{
			"configHome graft override",

			testEnv{"GRAFT_CONFIG_HOME": "/graft/config"},
			graftConfigHome,
			"/graft/config",
		},

		// graftStateHome tests - returns ~/.local/state/graft
		{
			"stateHome default",

			testEnv{
				"GRAFT_STATE_HOME": "",
				"XDG_STATE_HOME":   "",
			},
			graftStateHome,
			filepath.Join(homeDir,
				".local",
				"state",
				"graft"),
		},

		{
			"stateHome xdg override",

			testEnv{
				"GRAFT_STATE_HOME": "",
				"XDG_STATE_HOME":   "/xdg/state",
			},
			graftStateHome,
			"/xdg/state/graft",
		},

		{
			"stateHome graft override",

			testEnv{"GRAFT_STATE_HOME": "/graft/state"},
			graftStateHome,
			"/graft/state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.env.apply(t)

			if got := tt.fn(homeDir); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPathFunctions(t *testing.T) {
	clearEnv := testEnv{
		"GRAFT_CONFIG_HOME": "", "GRAFT_STATE_HOME": "",
		"XDG_CONFIG_HOME": "", "XDG_STATE_HOME": "",
	}

	tests := []struct {
		name       string
		env        testEnv
		fn         func() (string, error)
		wantSuffix string // use suffix match for paths that include user home
		wantExact  string // use exact match when env override is set
	}{
		// Socket paths - inside graft state dir
		{
			"socket local default",
			clearEnv,
			func() (string, error) {
				return DaemonSocketPathForCurrentHost(ServerRoleLocal)
			},
			"graft/local/graftd.sock",
			"",
		},

		{
			"socket remote default",
			clearEnv,
			func() (string, error) {
				return DaemonSocketPathForCurrentHost(ServerRoleRemote)
			},
			"graft/remote/graftd.sock",
			"",
		},

		{
			"socket xdg override",
			testEnv{
				"XDG_STATE_HOME": "/xdg/state",
			},
			func() (string, error) {
				return DaemonSocketPathForCurrentHost(ServerRoleLocal)
			},
			"",
			"/xdg/state/graft/local/graftd.sock",
		},

		{
			"socket graft override",
			testEnv{
				"GRAFT_STATE_HOME": "/graft/state",
			},
			func() (string, error) {
				return DaemonSocketPathForCurrentHost(ServerRoleLocal)
			},
			"",
			"/graft/state/local/graftd.sock",
		},

		// Config paths - inside graft config dir
		{
			"config local default",
			clearEnv,
			func() (string, error) {
				return RootConfigPathForCurrentHost(ServerRoleLocal)
			},
			"graft/local/config.yml",
			"",
		},

		{
			"config remote default",
			clearEnv,
			func() (string, error) {
				return RootConfigPathForCurrentHost(ServerRoleRemote)
			},
			"graft/remote/config.yml",
			"",
		},

		{
			"config xdg override",
			testEnv{
				"XDG_CONFIG_HOME": "/xdg/config",
			},
			func() (string, error) {
				return RootConfigPathForCurrentHost(ServerRoleLocal)
			},
			"",
			"/xdg/config/graft/local/config.yml",
		},

		{
			"config graft override",
			testEnv{
				"GRAFT_CONFIG_HOME": "/graft/config",
			},
			func() (string, error) {
				return RootConfigPathForCurrentHost(ServerRoleLocal)
			},
			"",
			"/graft/config/local/config.yml",
		},

		// Logs paths - inside graft state dir
		{
			"logs local default",
			clearEnv,
			func() (string, error) {
				return DaemonLogsPathForCurrentHost(ServerRoleLocal)
			},
			"graft/local/logs",
			"",
		},

		{
			"logs remote default",
			clearEnv,
			func() (string, error) {
				return DaemonLogsPathForCurrentHost(ServerRoleRemote)
			},
			"graft/remote/logs",
			"",
		},

		{
			"logs xdg override",
			testEnv{
				"XDG_STATE_HOME": "/xdg/state",
			},
			func() (string, error) {
				return DaemonLogsPathForCurrentHost(ServerRoleLocal)
			},
			"",
			"/xdg/state/graft/local/logs",
		},

		{
			"logs graft override",
			testEnv{
				"GRAFT_STATE_HOME": "/graft/state",
			},
			func() (string, error) {
				return DaemonLogsPathForCurrentHost(ServerRoleLocal)
			},
			"",
			"/graft/state/local/logs",
		},

		// Sessions paths - inside graft state dir
		{
			"sessions default",
			clearEnv,
			SessionsRoot,
			"graft/local/sessions",
			"",
		},

		{
			"sessions xdg override",
			testEnv{
				"XDG_STATE_HOME": "/xdg/state",
			},
			SessionsRoot,
			"",
			"/xdg/state/graft/local/sessions",
		},

		{
			"sessions graft override",
			testEnv{"GRAFT_STATE_HOME": "/graft/state"},
			SessionsRoot,
			"",
			"/graft/state/local/sessions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv.apply(t)
			tt.env.apply(t)

			got, err := tt.fn()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantExact != "" {
				if got != tt.wantExact {
					t.Errorf("got %q, want %q", got, tt.wantExact)
				}
			} else if !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("got %q, want suffix %q", got, tt.wantSuffix)
			}
		})
	}
}

func TestForRemotePaths(t *testing.T) {
	homeDir := "/home/remoteuser"

	clearEnv := testEnv{
		"GRAFT_CONFIG_HOME": "", "GRAFT_STATE_HOME": "",
		"XDG_CONFIG_HOME": "", "XDG_STATE_HOME": "",
	}

	tests := []struct {
		name string
		env  testEnv
		fn   func(string) (string, error)
		want string
	}{
		{
			"socket default", clearEnv,
			func(h string) (string, error) { return DaemonSocketPathForRemote(h, "") },
			"/home/remoteuser/.local/state/graft/remote/graftd.sock",
		},
		{
			"socket xdg",
			testEnv{"XDG_STATE_HOME": "/xdg/state"},
			func(h string) (string, error) { return DaemonSocketPathForRemote(h, "") },
			"/xdg/state/graft/remote/graftd.sock",
		},
		{
			"socket graft",
			testEnv{"GRAFT_STATE_HOME": "/graft/state"},
			func(h string) (string, error) { return DaemonSocketPathForRemote(h, "") },
			"/graft/state/remote/graftd.sock",
		},
		{"config default", clearEnv, RootConfigPathForRemote, "/home/remoteuser/.config/graft/remote/config.yml"},
		{"config xdg", testEnv{"XDG_CONFIG_HOME": "/xdg/config"}, RootConfigPathForRemote, "/xdg/config/graft/remote/config.yml"},
		{"config graft", testEnv{"GRAFT_CONFIG_HOME": "/graft/config"}, RootConfigPathForRemote, "/graft/config/remote/config.yml"},
		{"logs default", clearEnv, DaemonLogsPathForRemote, "/home/remoteuser/.local/state/graft/remote/logs"},
		{"logs xdg", testEnv{"XDG_STATE_HOME": "/xdg/state"}, DaemonLogsPathForRemote, "/xdg/state/graft/remote/logs"},
		{"logs graft", testEnv{"GRAFT_STATE_HOME": "/graft/state"}, DaemonLogsPathForRemote, "/graft/state/remote/logs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv.apply(t)
			tt.env.apply(t)

			got, err := tt.fn(homeDir)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoleSubdir(t *testing.T) {
	tests := []struct {
		role    ServerRole
		want    string
		wantErr bool
	}{
		{ServerRoleLocal, "local", false},
		{ServerRoleRemote, "remote", false},
		{ServerRole(99), "", true},
	}

	for _, tt := range tests {
		got, err := roleSubdir(tt.role)
		if (err != nil) != tt.wantErr {
			t.Errorf("roleSubdir(%v) error = %v, wantErr %v", tt.role, err, tt.wantErr)

			continue
		}

		if got != tt.want {
			t.Errorf("roleSubdir(%v) = %q, want %q", tt.role, got, tt.want)
		}
	}
}

func TestSessionPath(t *testing.T) {
	t.Setenv("GRAFT_STATE_HOME", "")
	t.Setenv("XDG_STATE_HOME", "/home/user/.local/state")

	path, err := SessionPath(12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "/home/user/.local/state/graft/local/sessions/12345"
	if path != want {
		t.Errorf("got %q, want %q", path, want)
	}

	t.Setenv("GRAFT_STATE_HOME", "/graft/state")

	path, err = SessionPath(67890)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want = "/graft/state/local/sessions/67890"
	if path != want {
		t.Errorf("got %q, want %q", path, want)
	}
}
