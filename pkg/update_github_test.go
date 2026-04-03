package graft

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.viam.com/test"
)

func newTestGitHubReleaseClient(t *testing.T) (*GitHubReleaseClient, *http.ServeMux) {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return newGitHubReleaseClient(server.URL), mux
}

func TestGitHubReleaseClientCheckForUpdate(t *testing.T) {
	t.Run("update available", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/releases/latest/download/version.txt", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "v1.5.0\n")
		})

		result, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.LatestVersion, test.ShouldEqual, "v1.5.0")
		test.That(t, result.UpdateAvailable, test.ShouldBeTrue)
	})

	t.Run("already up to date", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/releases/latest/download/version.txt", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "v1.5.0\n")
		})

		result, err := CheckForUpdate(t.Context(), client, "v1.5.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
	})

	t.Run("dev build not updateable", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/releases/latest/download/version.txt", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "v1.5.0\n")
		})

		result, err := CheckForUpdate(t.Context(), client, "dev-abc1234")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
		test.That(t, result.IsDevBuild, test.ShouldBeTrue)
	})

	t.Run("prerelease skipped", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/releases/latest/download/version.txt", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "v2.0.0-rc1\n")
		})

		result, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
	})

	t.Run("server error", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/releases/latest/download/version.txt", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		})

		_, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "fetching latest version")
	})
}

func TestGitHubReleaseClientDownloadAndVerify(t *testing.T) {
	binaryContent := []byte("fake binary content for testing")
	hash := sha256.Sum256(binaryContent)
	checksum := hex.EncodeToString(hash[:])

	binaryName, err := PlatformBinaryName()
	if err != nil {
		t.Skip("unsupported platform")
	}

	checksumFile := fmt.Sprintf("%s  %s\n", checksum, binaryName)

	t.Run("successful download and verify", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)

		mux.HandleFunc("/releases/download/v1.0.0/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write([]byte(checksumFile))
			test.That(t, wErr, test.ShouldBeNil)
		})
		mux.HandleFunc("/releases/download/v1.0.0/"+binaryName, func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write(binaryContent)
			test.That(t, wErr, test.ShouldBeNil)
		})

		tmpPath, _, err := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)

		defer os.Remove(tmpPath)

		written, err := os.ReadFile(tmpPath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, written, test.ShouldResemble, binaryContent)

		info, err := os.Stat(tmpPath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, info.Mode()&0o100 != 0, test.ShouldBeTrue)
	})

	t.Run("checksum mismatch", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)

		mux.HandleFunc("/releases/download/v1.0.0/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write([]byte(checksumFile))
			test.That(t, wErr, test.ShouldBeNil)
		})
		mux.HandleFunc("/releases/download/v1.0.0/"+binaryName, func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write([]byte("different content"))
			test.That(t, wErr, test.ShouldBeNil)
		})

		tmpPath, _, err := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "checksum verification failed")
		test.That(t, tmpPath, test.ShouldBeEmpty)
	})

	t.Run("missing binary", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)

		mux.HandleFunc("/releases/download/v1.0.0/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write([]byte(checksumFile))
			test.That(t, wErr, test.ShouldBeNil)
		})

		_, _, err := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "downloading binary")
	})

	t.Run("missing checksums", func(t *testing.T) {
		client, _ := newTestGitHubReleaseClient(t)

		_, _, err := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "downloading checksums")
	})
}

func TestGitHubReleaseClientReleaseNotes(t *testing.T) {
	t.Run("returns notes from release", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		body := "## What's New\n- Fixed bug\n- Added feature"

		mux.HandleFunc("/releases/download/v1.5.0/release-notes.txt", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, body)
		})

		notes, err := client.ReleaseNotes(t.Context(), "v1.5.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, notes, test.ShouldEqual, body)
	})

	t.Run("error when not available", func(t *testing.T) {
		client, _ := newTestGitHubReleaseClient(t)

		_, err := client.ReleaseNotes(t.Context(), "v1.5.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "fetching release notes")
	})

	t.Run("fetches release notes after LatestVersion", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		body := "- Bug fix"

		mux.HandleFunc("/releases/latest/download/version.txt", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "v1.5.0\n")
		})
		mux.HandleFunc("/releases/download/v1.5.0/release-notes.txt", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, body)
		})

		version, err := client.LatestVersion(t.Context())
		test.That(t, err, test.ShouldBeNil)
		test.That(t, version, test.ShouldEqual, "v1.5.0")

		notes, err := client.ReleaseNotes(t.Context(), "v1.5.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, notes, test.ShouldEqual, body)
	})
}
