package graft

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/edaniels/graft/errors"
)

// RootConfig contains the complete, serializable configuration for a local graft instance and its
// connections.
//
// The config itself can be reloaded and persisted to local storage on demand. As such, it is safe
// for concurrent access.
type RootConfig struct {
	Connections []ConnectionConfig `yaml:"connections"`
	configMu    sync.Mutex
}

// Validate ensures the configuration is valid, which is useful before/after (de)serialization.
//
// TODO(erd): Consider integrating validation into YAML unmarshaling.
func (conf *RootConfig) Validate() error {
	conf.configMu.Lock()
	defer conf.configMu.Unlock()

	return conf.validate()
}

var errRootConfigDuplicateConnection = errors.NewBare("connection seen more than once")

func (conf *RootConfig) validate() error {
	seenConn := map[string]bool{}
	for idx, conn := range conf.Connections {
		if seenConn[conn.Name] {
			return errors.WrapSuffix(errRootConfigDuplicateConnection, conn.Name)
		}

		seenConn[conn.Name] = true

		err := conn.Validate()
		if err != nil {
			return errors.WrapPrefix(err, fmt.Sprintf("connection %d invalid", idx))
		}
	}

	return nil
}

// Reload refreshes the config from the given path.
func (conf *RootConfig) Reload(fromPath string) error {
	conf.configMu.Lock()
	defer conf.configMu.Unlock()

	var newConfig RootConfig

	rd, err := os.ReadFile(fromPath)
	if err != nil {
		return errors.Wrap(err)
	}

	if err := yaml.Unmarshal(rd, &newConfig); err != nil {
		return errors.Wrap(err)
	}

	conf.cloneFrom(&newConfig)

	return conf.validate()
}

// Persist saves the config to the given path.
func (conf *RootConfig) Persist(toPath string) error {
	conf.configMu.Lock()
	defer conf.configMu.Unlock()

	if err := conf.validate(); err != nil {
		return errors.WrapPrefix(err, "error validating config; cannot persist")
	}

	md, err := yaml.Marshal(conf)
	if err != nil {
		return errors.Wrap(err)
	}

	if err := os.WriteFile(toPath, md, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	if err := os.Chmod(toPath, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

// SyncModesFor returns the configured permission modes for the named
// connection's synchronization from fromLocal, or empty strings when none
// are recorded. Requests that carry no modes inherit these, so a bare graft
// sync does not reset modes configured elsewhere.
func (conf *RootConfig) SyncModesFor(connName, fromLocal string) (string, string) {
	conf.configMu.Lock()
	defer conf.configMu.Unlock()

	for _, conn := range conf.Connections {
		if conn.Name != connName {
			continue
		}

		for _, syncConf := range conn.Synchronizations {
			if syncConf.FromLocal == fromLocal {
				return syncConf.DefaultFileMode, syncConf.DefaultDirectoryMode
			}
		}
	}

	return "", ""
}

// SyncIncludesFor returns the configured syncInclude patterns for the named
// connection's synchronization from fromLocal, or nil when none are recorded.
// A request that carries no includes inherits these, so a bare graft sync does
// not drop includes configured elsewhere (e.g. right after a daemon restart,
// before reconcile has run).
func (conf *RootConfig) SyncIncludesFor(connName, fromLocal string) []string {
	conf.configMu.Lock()
	defer conf.configMu.Unlock()

	for _, conn := range conf.Connections {
		if conn.Name != connName {
			continue
		}

		for _, syncConf := range conn.Synchronizations {
			if syncConf.FromLocal == fromLocal {
				return syncConf.SyncInclude
			}
		}
	}

	return nil
}

// always update this when new top level fields are added.
func (conf *RootConfig) cloneFrom(from *RootConfig) {
	from.configMu.Lock()
	defer from.configMu.Unlock()

	conf.Connections = from.Connections
}

// A SynchronizationIntentConfig is the config for a [SynchronizationIntent].
type SynchronizationIntentConfig struct {
	FromLocal string `yaml:"fromLocal"`
	ToRemote  string `yaml:"toRemote"`
	// SyncGit enables a secondary one-way replica of FromLocal's .git
	// directory to the remote, giving the remote a read-only git view.
	SyncGit bool `yaml:"syncGit,omitempty"`
	// SyncInclude are gitignore-style patterns for content that must sync even
	// though .gitignore excludes it (e.g. generated protobufs). Each is applied
	// as a "!" negation over the gitignore-derived ignores.
	SyncInclude []string `yaml:"syncInclude,omitempty"`
	// DefaultFileMode and DefaultDirectoryMode are octal permission mode
	// strings (e.g. "644", "0644") applied to files and directories the sync
	// creates or updates on the remote. Empty means graft's defaults: "644"
	// for files and "755" for directories in the working tree, and mutagen's
	// private 0600/0700 for the .git replica. File modes must not include
	// executability bits; mutagen propagates the source's executable bit on
	// top of the base mode.
	DefaultFileMode      string `yaml:"defaultFileMode,omitempty"`
	DefaultDirectoryMode string `yaml:"defaultDirectoryMode,omitempty"`
}

// A ConnectionConfig is the configuration for a single connection, regardless of its type.
//
// This is not safe for concurrent use.
type ConnectionConfig struct {
	// The user-accessible name of the connection.
	Name string `yaml:"name"`
	// The fully qualified destination URI (e.g. an ssh:// or docker:// target).
	Destination string `yaml:"destination"`
	// The local filesystem root of the connection used for forwarding (+cwd/sync?) detection.
	LocalRoot string `yaml:"localRoot"`
	// The remote filesystem root of the connection.
	RemoteRoot string `yaml:"remoteRoot"`
	// The commands to forward from the client to the destination.
	Forward []string `yaml:"forward"`
	// Whether or not to have forwarded commands be prefixed with the connection name (e.g. python3 -> conn1-python3)
	PrefixForward []string `yaml:"prefixForward"`
	// A list of file synchronizations for this connection. It's unclear what problems, if any, occur if there are overlapping
	// intents.
	Synchronizations []SynchronizationIntentConfig `yaml:"synchronizations"`
	// Whether this connection is a background connection, excluded from CWD-based auto-selection.
	Background bool `yaml:"background,omitempty"`
	// Explicit port forwards for this connection (e.g. "8080", "3000:8080/tcp").
	Ports []string `yaml:"ports,omitempty"`
}

var errConnectionConfigDuplicateForwarding = errors.NewBare("connection duplicate forwarding detected")

// Validate esnures the connection configuration is valid, which is useful before/after (de)serialization.
func (conf *ConnectionConfig) Validate() error {
	if conf.Name == "" {
		return errors.New("no name")
	}

	if conf.Destination == "" {
		return errors.New("no destination")
	}

	seenFwd := map[string]bool{}
	for _, fwd := range conf.Forward {
		if seenFwd[fwd] {
			return errors.WrapSuffix(
				errConnectionConfigDuplicateForwarding,
				fmt.Sprintf("connection='%s', forwarding='%s'", conf.Name, fwd))
		}

		seenFwd[fwd] = true
	}

	seenPrefixFwd := map[string]bool{}
	for _, fwd := range conf.PrefixForward {
		if seenPrefixFwd[fwd] {
			return errors.WrapSuffix(
				errConnectionConfigDuplicateForwarding,
				fmt.Sprintf("connection='%s', forwarding='%s' (with prefix)", conf.Name, fwd))
		}

		seenPrefixFwd[fwd] = true
	}

	for idx, syncConf := range conf.Synchronizations {
		if err := validateSyncModes(syncConf.DefaultFileMode, syncConf.DefaultDirectoryMode); err != nil {
			return errors.WrapPrefix(err, fmt.Sprintf("synchronization %d invalid", idx))
		}
	}

	return nil
}
