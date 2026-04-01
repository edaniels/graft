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

	return nil
}
