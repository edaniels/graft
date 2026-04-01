package graft

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
)

func TestCheckForUpdate(t *testing.T) {
	t.Run("update available", func(t *testing.T) {
		client := &fakeReleaseClient{latestVersion: "v2.0.0"}

		result, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeTrue)
		test.That(t, result.LatestVersion, test.ShouldEqual, "v2.0.0")
		test.That(t, result.CurrentVersion, test.ShouldEqual, "v1.0.0")
		test.That(t, result.IsDevBuild, test.ShouldBeFalse)
	})

	t.Run("already up to date", func(t *testing.T) {
		client := &fakeReleaseClient{latestVersion: "v1.0.0"}

		result, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
	})

	t.Run("dev build not updateable", func(t *testing.T) {
		client := &fakeReleaseClient{latestVersion: "v2.0.0"}

		result, err := CheckForUpdate(t.Context(), client, "dev-abc1234")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
		test.That(t, result.IsDevBuild, test.ShouldBeTrue)
	})
}

func TestDownloadAndVerify(t *testing.T) {
	binaryName, err := PlatformBinaryName()
	if err != nil {
		t.Skip("unsupported platform")
	}

	platform := runtime.GOOS + "-" + runtime.GOARCH

	binaryContent := []byte("fake binary content for testing")
	hash := sha256.Sum256(binaryContent)
	checksum := hex.EncodeToString(hash[:])

	checksumFile := fmt.Sprintf("%s  graft-%s\n", checksum, platform)

	t.Run("successful download and verify", func(t *testing.T) {
		client := &fakeReleaseClient{
			latestVersion: "v1.0.0",
			objects: map[string][]byte{
				"v1.0.0/checksums.txt": []byte(checksumFile),
				"v1.0.0/" + binaryName: binaryContent,
			},
		}

		tmpPath, _, dlErr := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, dlErr, test.ShouldBeNil)

		defer os.Remove(tmpPath)

		written, readErr := os.ReadFile(tmpPath)
		test.That(t, readErr, test.ShouldBeNil)
		test.That(t, written, test.ShouldResemble, binaryContent)

		info, statErr := os.Stat(tmpPath)
		test.That(t, statErr, test.ShouldBeNil)
		test.That(t, info.Mode()&0o100 != 0, test.ShouldBeTrue)
	})

	t.Run("checksum mismatch", func(t *testing.T) {
		client := &fakeReleaseClient{
			latestVersion: "v1.0.0",
			objects: map[string][]byte{
				"v1.0.0/checksums.txt": []byte(checksumFile),
				"v1.0.0/" + binaryName: []byte("different content"),
			},
		}

		tmpPath, _, dlErr := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, dlErr, test.ShouldNotBeNil)
		test.That(t, dlErr.Error(), test.ShouldContainSubstring, "checksum verification failed")
		test.That(t, tmpPath, test.ShouldBeEmpty)
	})

	t.Run("missing checksums", func(t *testing.T) {
		client := &fakeReleaseClient{
			latestVersion: "v1.0.0",
			objects: map[string][]byte{
				"v1.0.0/" + binaryName: binaryContent,
			},
		}

		_, _, dlErr := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, dlErr, test.ShouldNotBeNil)
		test.That(t, dlErr.Error(), test.ShouldContainSubstring, "downloading checksums")
	})

	t.Run("missing binary", func(t *testing.T) {
		client := &fakeReleaseClient{
			latestVersion: "v1.0.0",
			objects: map[string][]byte{
				"v1.0.0/checksums.txt": []byte(checksumFile),
			},
		}

		tmpPath, _, dlErr := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, dlErr, test.ShouldNotBeNil)
		test.That(t, dlErr.Error(), test.ShouldContainSubstring, "downloading binary")
		test.That(t, tmpPath, test.ShouldBeEmpty)
	})

	t.Run("invalid version rejected", func(t *testing.T) {
		client := &fakeReleaseClient{latestVersion: "v1.0.0"}

		_, _, dlErr := DownloadAndVerify(t.Context(), client, "../../etc/passwd")
		test.That(t, dlErr, test.ShouldNotBeNil)
		test.That(t, dlErr.Error(), test.ShouldContainSubstring, "invalid version format")
	})
}

func TestVersionComparison(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		current   string
		want      bool
	}{
		{"newer major", "v2.0.0", "v1.0.0", true},
		{"newer minor", "v1.1.0", "v1.0.0", true},
		{"newer patch", "v1.0.1", "v1.0.0", true},
		{"same version", "v1.0.0", "v1.0.0", false},
		{"older version", "v1.0.0", "v2.0.0", false},
		{"candidate is prerelease", "v1.0.0-rc1", "v0.9.0", false},
		{"unparseable current (dev build)", "v1.0.0", "dev-abc1234", false},
		{"unparseable candidate", "not-a-version", "v1.0.0", false},
		{"both unparseable", "not-a-version", "also-not-a-version", false},
		{"current is prerelease", "v1.0.0", "v1.0.0-rc1", true},
		{"with v prefix", "v0.2.0", "v0.1.0", true},
		{"without v prefix", "0.2.0", "0.1.0", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			test.That(t, versionNewerThan(tc.candidate, tc.current), test.ShouldEqual, tc.want)
		})
	}
}

func TestParseChecksum(t *testing.T) {
	const (
		sum1 = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
		sum2 = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
		sum3 = "fedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"
	)

	checksums := sum1 + "  graft-linux-amd64\n" +
		sum2 + "  graft-darwin-arm64\n" +
		sum3 + "  graft-linux-arm64\n"

	t.Run("finds correct checksum", func(t *testing.T) {
		sum, err := parseChecksum(checksums, "graft-darwin-arm64")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, sum, test.ShouldEqual, sum2)
	})

	t.Run("finds first entry", func(t *testing.T) {
		sum, err := parseChecksum(checksums, "graft-linux-amd64")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, sum, test.ShouldEqual, sum1)
	})

	t.Run("finds last entry", func(t *testing.T) {
		sum, err := parseChecksum(checksums, "graft-linux-arm64")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, sum, test.ShouldEqual, sum3)
	})

	t.Run("returns error for missing binary", func(t *testing.T) {
		_, err := parseChecksum(checksums, "graft-windows-amd64")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "checksum not found")
	})

	t.Run("handles empty input", func(t *testing.T) {
		_, err := parseChecksum("", "graft-linux-amd64")
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("handles trailing newlines and whitespace", func(t *testing.T) {
		sum, err := parseChecksum("  "+sum1+"  graft-linux-amd64  \n\n", "graft-linux-amd64")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, sum, test.ShouldEqual, sum1)
	})

	t.Run("rejects short checksum", func(t *testing.T) {
		_, err := parseChecksum("abc123  graft-linux-amd64\n", "graft-linux-amd64")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "invalid checksum length")
	})

	t.Run("rejects non-hex checksum", func(t *testing.T) {
		badHex := "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
		_, err := parseChecksum(badHex+"  graft-linux-amd64\n", "graft-linux-amd64")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "invalid checksum hex")
	})
}

func TestPlatformBinaryName(t *testing.T) {
	name, err := PlatformBinaryName()

	// This test is platform-dependent; just verify it doesn't error on supported platforms.
	if err != nil {
		test.That(t, err.Error(), test.ShouldContainSubstring, "unsupported platform")
	} else {
		test.That(t, name, test.ShouldNotBeEmpty)
	}
}

func TestValidateVersionString(t *testing.T) {
	t.Run("valid versions", func(t *testing.T) {
		test.That(t, validateVersionString("v1.0.0"), test.ShouldBeNil)
		test.That(t, validateVersionString("v0.1.0"), test.ShouldBeNil)
		test.That(t, validateVersionString("v1.2.3-rc1"), test.ShouldBeNil)
		test.That(t, validateVersionString("1.0.0"), test.ShouldBeNil)
	})

	t.Run("rejects path traversal", func(t *testing.T) {
		err := validateVersionString("../../etc/passwd")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "invalid version format")
	})

	t.Run("rejects non-semver", func(t *testing.T) {
		test.That(t, validateVersionString("not-a-version"), test.ShouldNotBeNil)
		test.That(t, validateVersionString(""), test.ShouldNotBeNil)
		test.That(t, validateVersionString("latest"), test.ShouldNotBeNil)
	})
}

func TestReplaceBinary(t *testing.T) {
	t.Run("replaces target with source", func(t *testing.T) {
		dir := t.TempDir()
		targetPath := filepath.Join(dir, "target")
		tmpPath := filepath.Join(dir, "source")

		test.That(t, os.WriteFile(targetPath, []byte("old"), ExecFilePerms), test.ShouldBeNil)
		test.That(t, os.WriteFile(tmpPath, []byte("new"), ExecFilePerms), test.ShouldBeNil)

		err := ReplaceBinary(tmpPath, targetPath)
		test.That(t, err, test.ShouldBeNil)

		data, err := os.ReadFile(targetPath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldEqual, "new")

		// Source file should no longer exist (it was renamed)
		_, err = os.Stat(tmpPath)
		test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
	})

	t.Run("fails with missing source", func(t *testing.T) {
		dir := t.TempDir()
		targetPath := filepath.Join(dir, "target")
		tmpPath := filepath.Join(dir, "nonexistent")

		test.That(t, os.WriteFile(targetPath, []byte("old"), ExecFilePerms), test.ShouldBeNil)

		err := ReplaceBinary(tmpPath, targetPath)
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "replacing binary")
	})
}

func TestAcquireUpdateLock(t *testing.T) {
	t.Run("acquire and release", func(t *testing.T) {
		lock, err := AcquireUpdateLock()
		test.That(t, err, test.ShouldBeNil)
		test.That(t, lock.Close(), test.ShouldBeNil)
	})

	t.Run("second acquire fails while held", func(t *testing.T) {
		lock1, err := AcquireUpdateLock()
		test.That(t, err, test.ShouldBeNil)

		defer lock1.Close()

		_, err = AcquireUpdateLock()
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "another update is already in progress")
	})

	t.Run("reacquire after release", func(t *testing.T) {
		lock1, err := AcquireUpdateLock()
		test.That(t, err, test.ShouldBeNil)
		test.That(t, lock1.Close(), test.ShouldBeNil)

		lock2, err := AcquireUpdateLock()
		test.That(t, err, test.ShouldBeNil)
		test.That(t, lock2.Close(), test.ShouldBeNil)
	})
}

func TestReleaseClientFromConfig(t *testing.T) {
	t.Run("env var returns HTTPS client", func(t *testing.T) {
		t.Setenv("GRAFT_UPDATE_URL", "https://example.com/releases")

		client := ReleaseClientFromConfig()
		_, ok := client.(*HTTPSReleaseClient)
		test.That(t, ok, test.ShouldBeTrue)
	})

	t.Run("no env var returns GitHub client", func(t *testing.T) {
		t.Setenv("GRAFT_UPDATE_URL", "")

		client := ReleaseClientFromConfig()
		_, ok := client.(*GitHubReleaseClient)
		test.That(t, ok, test.ShouldBeTrue)
	})

	t.Run("build-time default used when no env var", func(t *testing.T) {
		t.Setenv("GRAFT_UPDATE_URL", "")

		old := defaultUpdateURL
		defaultUpdateURL = "https://build-time.example.com/releases"

		t.Cleanup(func() { defaultUpdateURL = old })

		client := ReleaseClientFromConfig()
		_, ok := client.(*HTTPSReleaseClient)
		test.That(t, ok, test.ShouldBeTrue)
	})

	t.Run("env var overrides build-time default", func(t *testing.T) {
		t.Setenv("GRAFT_UPDATE_URL", "https://override.example.com/releases")

		old := defaultUpdateURL
		defaultUpdateURL = "https://build-time.example.com/releases"

		t.Cleanup(func() { defaultUpdateURL = old })

		client := ReleaseClientFromConfig()
		httpsClient, ok := client.(*HTTPSReleaseClient)
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, httpsClient.baseURL, test.ShouldEqual, "https://override.example.com/releases")
	})
}

// fakeReleaseClient implements ReleaseClient for testing.
type fakeReleaseClient struct {
	latestVersion string
	objects       map[string][]byte
	latestErr     error
}

func (f *fakeReleaseClient) LatestVersion(_ context.Context) (string, error) {
	if f.latestErr != nil {
		return "", f.latestErr
	}

	return f.latestVersion, nil
}

func (f *fakeReleaseClient) DownloadChecksums(_ context.Context, version string) ([]byte, error) {
	key := version + "/checksums.txt"

	data, ok := f.objects[key]
	if !ok {
		return nil, errors.Errorf("not found: %s", key)
	}

	return data, nil
}

func (f *fakeReleaseClient) DownloadBinary(_ context.Context, version, binaryName, destPath string) error {
	key := version + "/" + binaryName

	data, ok := f.objects[key]
	if !ok {
		return errors.Errorf("not found: %s", key)
	}

	if err := os.WriteFile(destPath, data, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	return nil
}
