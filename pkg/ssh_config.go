package graft

import (
	"os"
	"path/filepath"
	"strings"

	ssh_config "github.com/kevinburke/ssh_config"

	"github.com/edaniels/graft/errors"
)

// resolvedSSHConfig holds the resolved SSH configuration for a host alias.
type resolvedSSHConfig struct {
	Hostname      string
	Port          string
	User          string
	IdentityFiles []string
	ProxyCommand  string
}

// sshConfigResolver looks up SSH config values for a given host alias.
type sshConfigResolver interface {
	Get(alias, key string) (string, error)
	GetAll(alias, key string) ([]string, error)
}

// defaultSSHConfigResolver uses the kevinburke/ssh_config library.
type defaultSSHConfigResolver struct{}

func (d defaultSSHConfigResolver) Get(alias, key string) (string, error) {
	val, err := ssh_config.GetStrict(alias, key)
	if err != nil {
		return "", errors.Wrap(err)
	}

	return val, nil
}

func (d defaultSSHConfigResolver) GetAll(alias, key string) ([]string, error) {
	val, err := ssh_config.GetAllStrict(alias, key)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return val, nil
}

// resolveSSHConfig resolves SSH configuration for a host alias.
// Falls back to the provided values when the config returns empty or default values.
func resolveSSHConfig(resolver sshConfigResolver, hostAlias, fallbackPort, fallbackUser string) resolvedSSHConfig {
	resolved := resolvedSSHConfig{
		Hostname: hostAlias,
		Port:     fallbackPort,
		User:     fallbackUser,
	}

	if hostname, err := resolver.Get(hostAlias, "Hostname"); err == nil && hostname != "" {
		resolved.Hostname = hostname
	}

	if port, err := resolver.Get(hostAlias, "Port"); err == nil && port != "" {
		if fallbackPort == "" {
			resolved.Port = port
		}
	}

	if resolved.Port == "" {
		resolved.Port = "22"
	}

	if user, err := resolver.Get(hostAlias, "User"); err == nil && user != "" {
		if fallbackUser == "" {
			resolved.User = user
		}
	}

	if identityFiles, err := resolver.GetAll(hostAlias, "IdentityFile"); err == nil {
		expanded := make([]string, 0, len(identityFiles))
		for _, f := range identityFiles {
			expanded = append(expanded, expandTilde(f))
		}

		resolved.IdentityFiles = expanded
	}

	if proxyCmd, err := resolver.Get(hostAlias, "ProxyCommand"); err == nil && proxyCmd != "" {
		resolved.ProxyCommand = substituteProxyTokens(proxyCmd, hostAlias, resolved.Hostname, resolved.Port, resolved.User)
	}

	return resolved
}

// substituteProxyTokens replaces SSH ProxyCommand tokens with resolved values.
// Handles %% (literal percent) per OpenSSH spec.
func substituteProxyTokens(cmd, originalHost, hostname, port, user string) string {
	const sentinel = "\x00PERCENT\x00"

	cmd = strings.ReplaceAll(cmd, "%%", sentinel)
	cmd = strings.ReplaceAll(cmd, "%h", hostname)
	cmd = strings.ReplaceAll(cmd, "%p", port)
	cmd = strings.ReplaceAll(cmd, "%r", user)
	cmd = strings.ReplaceAll(cmd, "%n", originalHost)
	cmd = strings.ReplaceAll(cmd, sentinel, "%")

	return cmd
}

// expandTilde replaces a leading ~ with the user's home directory.
func expandTilde(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(homeDir, path[1:])
}
