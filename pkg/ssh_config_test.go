package graft

import (
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/test"
)

type fakeSSHConfigResolver struct {
	values    map[string]map[string]string
	allValues map[string]map[string][]string
}

func (f *fakeSSHConfigResolver) Get(alias, key string) (string, error) {
	if hostVals, ok := f.values[alias]; ok {
		if val, ok := hostVals[key]; ok {
			return val, nil
		}
	}

	return "", nil
}

func (f *fakeSSHConfigResolver) GetAll(alias, key string) ([]string, error) {
	if hostVals, ok := f.allValues[alias]; ok {
		if vals, ok := hostVals[key]; ok {
			return vals, nil
		}
	}

	return nil, nil
}

func TestResolveSSHConfigWithConfigEntries(t *testing.T) {
	resolver := &fakeSSHConfigResolver{
		values: map[string]map[string]string{
			"myhost": {
				"Hostname":     "actual.host.com",
				"Port":         "2222",
				"User":         "configuser",
				"ProxyCommand": "ssh -W %h:%p jump.host.com",
			},
		},
		allValues: map[string]map[string][]string{
			"myhost": {
				"IdentityFile": {"~/.ssh/id_custom", "/absolute/path/key"},
			},
		},
	}

	resolved := resolveSSHConfig(resolver, "myhost", "", "")

	test.That(t, resolved.Hostname, test.ShouldEqual, "actual.host.com")
	test.That(t, resolved.Port, test.ShouldEqual, "2222")
	test.That(t, resolved.User, test.ShouldEqual, "configuser")
	test.That(t, resolved.ProxyCommand, test.ShouldEqual, "ssh -W actual.host.com:2222 jump.host.com")

	homeDir, err := os.UserHomeDir()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resolved.IdentityFiles[0], test.ShouldEqual, filepath.Join(homeDir, ".ssh/id_custom"))
	test.That(t, resolved.IdentityFiles[1], test.ShouldEqual, "/absolute/path/key")
}

func TestResolveSSHConfigFallbackValues(t *testing.T) {
	resolver := &fakeSSHConfigResolver{
		values:    map[string]map[string]string{},
		allValues: map[string]map[string][]string{},
	}

	resolved := resolveSSHConfig(resolver, "somehost", "3333", "explicituser")

	test.That(t, resolved.Hostname, test.ShouldEqual, "somehost")
	test.That(t, resolved.Port, test.ShouldEqual, "3333")
	test.That(t, resolved.User, test.ShouldEqual, "explicituser")
	test.That(t, resolved.ProxyCommand, test.ShouldBeEmpty)
	test.That(t, resolved.IdentityFiles, test.ShouldBeEmpty)
}

func TestResolveSSHConfigExplicitUserOverridesConfig(t *testing.T) {
	resolver := &fakeSSHConfigResolver{
		values: map[string]map[string]string{
			"myhost": {
				"User": "configuser",
			},
		},
		allValues: map[string]map[string][]string{},
	}

	resolved := resolveSSHConfig(resolver, "myhost", "", "explicituser")

	test.That(t, resolved.User, test.ShouldEqual, "explicituser")
}

func TestResolveSSHConfigExplicitPortOverridesConfig(t *testing.T) {
	resolver := &fakeSSHConfigResolver{
		values: map[string]map[string]string{
			"myhost": {
				"Port": "2222",
			},
		},
		allValues: map[string]map[string][]string{},
	}

	resolved := resolveSSHConfig(resolver, "myhost", "3333", "")

	test.That(t, resolved.Port, test.ShouldEqual, "3333")
}

func TestResolveSSHConfigDefaultPort(t *testing.T) {
	resolver := &fakeSSHConfigResolver{
		values:    map[string]map[string]string{},
		allValues: map[string]map[string][]string{},
	}

	resolved := resolveSSHConfig(resolver, "somehost", "", "")

	test.That(t, resolved.Port, test.ShouldEqual, "22")
}

func TestSubstituteProxyTokens(t *testing.T) {
	cmd := "ssh -W %h:%p -l %r gateway.%n"
	result := substituteProxyTokens(cmd, "alias", "real.host", "2222", "myuser")
	test.That(t, result, test.ShouldEqual, "ssh -W real.host:2222 -l myuser gateway.alias")

	// %% should become a literal %
	result = substituteProxyTokens("echo %%h is %h", "alias", "real.host", "22", "user")
	test.That(t, result, test.ShouldEqual, "echo %h is real.host")
}

func TestExpandTilde(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	test.That(t, err, test.ShouldBeNil)

	test.That(t, expandTilde("~/.ssh/id_rsa"), test.ShouldEqual, filepath.Join(homeDir, ".ssh/id_rsa"))
	test.That(t, expandTilde("/absolute/path"), test.ShouldEqual, "/absolute/path")
	test.That(t, expandTilde("relative/path"), test.ShouldEqual, "relative/path")
}
