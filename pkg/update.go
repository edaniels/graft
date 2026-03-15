package graft

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/Masterminds/semver/v3"

	"github.com/edaniels/graft/errors"
)

// ReleaseClient abstracts how release artifacts are fetched, allowing
// both GitHub Releases and plain HTTPS servers to share the same
// update-check and download-and-verify logic.
type ReleaseClient interface {
	// LatestVersion returns the latest available release version string.
	LatestVersion(ctx context.Context) (string, error)

	// DownloadChecksums fetches the checksums.txt for the given version.
	DownloadChecksums(ctx context.Context, version string) ([]byte, error)

	// DownloadBinary downloads the release binary to destPath.
	DownloadBinary(ctx context.Context, version, binaryName, destPath string) error
}

// UpdateCheckResult is returned by CheckForUpdate.
type UpdateCheckResult struct {
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
	IsDevBuild      bool // true when current version is not a valid semver release
}

// CheckForUpdate queries a ReleaseClient for a newer version.
func CheckForUpdate(ctx context.Context, client ReleaseClient, currentVersion string) (*UpdateCheckResult, error) {
	slog.DebugContext(ctx, "checking for update", "current_version", currentVersion)

	latestVersion, err := client.LatestVersion(ctx)
	if err != nil {
		slog.DebugContext(ctx, "failed to fetch latest version", "error", err)

		return nil, errors.WrapPrefix(err, "checking for update")
	}

	slog.DebugContext(ctx, "fetched latest version", "latest_version", latestVersion)

	_, parseErr := semver.NewVersion(currentVersion)
	isDevBuild := parseErr != nil

	updateAvailable := versionNewerThan(latestVersion, currentVersion)
	slog.DebugContext(ctx, "update check result",
		"latest_version", latestVersion, "current_version", currentVersion,
		"update_available", updateAvailable, "is_dev_build", isDevBuild)

	return &UpdateCheckResult{
		CurrentVersion:  currentVersion,
		LatestVersion:   latestVersion,
		UpdateAvailable: updateAvailable,
		IsDevBuild:      isDevBuild,
	}, nil
}

// DownloadAndVerify downloads a release binary via the ReleaseClient and verifies its SHA256 checksum.
// Returns the path to the verified temporary file and the resolved target binary path.
func DownloadAndVerify(ctx context.Context, client ReleaseClient, version string) (string, string, error) {
	if verr := validateVersionString(version); verr != nil {
		return "", "", verr
	}

	binaryName, err := PlatformBinaryName()
	if err != nil {
		return "", "", err
	}

	slog.DebugContext(ctx, "downloading release", "version", version, "binary", binaryName)

	checksumsData, err := client.DownloadChecksums(ctx, version)
	if err != nil {
		return "", "", errors.WrapPrefix(err, "downloading checksums")
	}

	expectedChecksum, err := parseChecksum(string(checksumsData), binaryName)
	if err != nil {
		return "", "", err
	}

	slog.DebugContext(ctx, "resolved expected checksum", "binary", binaryName, "checksum", expectedChecksum)

	// Resolve the real binary path once, for both temp file placement and later replacement.
	execPath, err := os.Executable()
	if err != nil {
		return "", "", errors.Wrap(err)
	}

	resolvedTarget, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		return "", "", errors.Wrap(err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(resolvedTarget), "graft-update-*")
	if err != nil {
		return "", "", errors.WrapPrefix(err, "creating temp file")
	}

	tmpFilePath := tmpFile.Name()
	tmpFile.Close()

	slog.DebugContext(ctx, "downloading binary", "dest", tmpFilePath)

	if dlErr := client.DownloadBinary(ctx, version, binaryName, tmpFilePath); dlErr != nil {
		os.Remove(tmpFilePath)

		return "", "", errors.WrapPrefix(dlErr, "downloading binary")
	}

	// Read and verify checksum
	binaryData, err := os.ReadFile(tmpFilePath)
	if err != nil {
		os.Remove(tmpFilePath)

		return "", "", errors.WrapPrefix(err, "reading downloaded binary")
	}

	actualHash := sha256.Sum256(binaryData)
	actualChecksum := hex.EncodeToString(actualHash[:])

	slog.DebugContext(ctx, "verifying checksum", "expected", expectedChecksum, "actual", actualChecksum)

	if actualChecksum != expectedChecksum {
		os.Remove(tmpFilePath)

		return "", "", errors.Errorf("checksum verification failed\n  expected: %s\n  actual:   %s", expectedChecksum, actualChecksum)
	}

	if err := os.Chmod(tmpFilePath, ExecFilePerms); err != nil {
		os.Remove(tmpFilePath)

		return "", "", errors.WrapPrefix(err, "setting permissions")
	}

	slog.DebugContext(ctx, "binary downloaded and verified", "path", tmpFilePath, "version", version)

	return tmpFilePath, resolvedTarget, nil
}

// PlatformBinaryName returns the expected binary asset name for the current platform.
func PlatformBinaryName() (string, error) {
	platform := runtime.GOOS + "-" + runtime.GOARCH

	switch platform {
	case "linux-amd64", "linux-arm64", "darwin-arm64":
	default:
		return "", errors.Errorf("unsupported platform: %s", platform)
	}

	return BinaryName(runtime.GOOS, runtime.GOARCH), nil
}

func parseChecksum(checksums, binaryName string) (string, error) {
	for rawLine := range strings.SplitSeq(checksums, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == binaryName {
			checksum := strings.ToLower(parts[0])

			if len(checksum) != 64 {
				return "", errors.Errorf("invalid checksum length for %s: got %d chars, want 64", binaryName, len(checksum))
			}

			if _, hexErr := hex.DecodeString(checksum); hexErr != nil {
				return "", errors.Errorf("invalid checksum hex for %s: %s", binaryName, checksum)
			}

			return checksum, nil
		}
	}

	return "", errors.Errorf("checksum not found for %s", binaryName)
}

// ReplaceBinary atomically replaces the binary at targetPath with the one at tmpPath.
func ReplaceBinary(tmpPath, targetPath string) error {
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return errors.WrapPrefix(err, "replacing binary")
	}

	return nil
}

// versionNewerThan returns true if candidate is a newer stable semver than current.
// Returns false if either version cannot be parsed, or if the candidate is a prerelease.
func versionNewerThan(candidate, current string) bool {
	candidateVer, err := semver.NewVersion(candidate)
	if err != nil {
		return false
	}

	if candidateVer.Prerelease() != "" {
		return false
	}

	currentVer, err := semver.NewVersion(current)
	if err != nil {
		return false
	}

	return candidateVer.GreaterThan(currentVer)
}

// validateVersionString checks that a version string is valid semver,
// preventing path traversal when interpolated into URLs.
func validateVersionString(version string) error {
	if _, err := semver.NewVersion(version); err != nil {
		return errors.Errorf("invalid version format %q", version)
	}

	return nil
}

// UpdateLock holds a file lock to prevent concurrent updates.
type UpdateLock struct {
	f *os.File
}

// Close releases the update lock and removes the lock file.
func (l *UpdateLock) Close() error {
	defer os.Remove(l.f.Name())

	return errors.Wrap(l.f.Close())
}

// AcquireUpdateLock acquires an exclusive lock to prevent concurrent update processes.
// The caller must close the returned lock when done.
func AcquireUpdateLock() (*UpdateLock, error) {
	lockPath := filepath.Join(os.TempDir(), fmt.Sprintf("graft-update-%d.lock", os.Getuid()))

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errors.WrapPrefix(err, "creating update lock file")
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()

		return nil, errors.New("another update is already in progress")
	}

	return &UpdateLock{f: f}, nil
}
