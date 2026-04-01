package graft

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/test"
)

func newTestHTTPSServer(t *testing.T, objects map[string][]byte) *HTTPSReleaseClient {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)

			return
		}

		_, err := w.Write(data)
		test.That(t, err, test.ShouldBeNil)
	}))
	t.Cleanup(server.Close)

	return NewHTTPSReleaseClient(server.URL)
}

func TestHTTPSReleaseClientLatestVersion(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{
			"/latest": []byte("v1.5.0\n"),
		})

		version, err := client.LatestVersion(t.Context())
		test.That(t, err, test.ShouldBeNil)
		test.That(t, version, test.ShouldEqual, "v1.5.0")
	})

	t.Run("trims whitespace and newlines", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{
			"/latest": []byte("  v2.0.0  \n\n"),
		})

		version, err := client.LatestVersion(t.Context())
		test.That(t, err, test.ShouldBeNil)
		test.That(t, version, test.ShouldEqual, "v2.0.0")
	})

	t.Run("server error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)

		client := NewHTTPSReleaseClient(server.URL)

		_, err := client.LatestVersion(t.Context())
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "500")
	})

	t.Run("not found", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{})

		_, err := client.LatestVersion(t.Context())
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "404")
	})
}

func TestHTTPSReleaseClientDownloadChecksums(t *testing.T) {
	checksumContent := []byte("abc123  graft-linux-amd64\ndef456  graft-darwin-arm64\n")

	t.Run("happy path", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{
			"/v1.0.0/checksums.txt": checksumContent,
		})

		data, err := client.DownloadChecksums(t.Context(), "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, data, test.ShouldResemble, checksumContent)
	})

	t.Run("not found", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{})

		_, err := client.DownloadChecksums(t.Context(), "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "404")
	})
}

func TestHTTPSReleaseClientDownloadBinary(t *testing.T) {
	binaryContent := []byte("fake binary content")

	t.Run("happy path", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{
			"/v1.0.0/graft-linux-amd64": binaryContent,
		})

		destPath := filepath.Join(t.TempDir(), "graft-linux-amd64")

		err := client.DownloadBinary(t.Context(), "v1.0.0", "graft-linux-amd64", destPath)
		test.That(t, err, test.ShouldBeNil)

		written, err := os.ReadFile(destPath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, written, test.ShouldResemble, binaryContent)
	})

	t.Run("not found", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{})

		destPath := filepath.Join(t.TempDir(), "graft-linux-amd64")

		err := client.DownloadBinary(t.Context(), "v1.0.0", "graft-linux-amd64", destPath)
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "404")
	})
}

func TestHTTPSReleaseClientDownloadAndVerify(t *testing.T) {
	binaryContent := []byte("fake binary content for https testing")
	hash := sha256.Sum256(binaryContent)
	checksum := hex.EncodeToString(hash[:])

	binaryName, err := PlatformBinaryName()
	if err != nil {
		t.Skip("unsupported platform")
	}

	checksumFile := fmt.Sprintf("%s  %s\n", checksum, binaryName)

	t.Run("successful download and verify", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{
			"/v1.0.0/checksums.txt": []byte(checksumFile),
			"/v1.0.0/" + binaryName: binaryContent,
		})

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
		client := newTestHTTPSServer(t, map[string][]byte{
			"/v1.0.0/checksums.txt": []byte(checksumFile),
			"/v1.0.0/" + binaryName: []byte("different content"),
		})

		tmpPath, _, dlErr := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, dlErr, test.ShouldNotBeNil)
		test.That(t, dlErr.Error(), test.ShouldContainSubstring, "checksum verification failed")
		test.That(t, tmpPath, test.ShouldBeEmpty)
	})
}

func TestHTTPSReleaseClientCheckForUpdate(t *testing.T) {
	t.Run("update available", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{
			"/latest": []byte("v2.0.0\n"),
		})

		result, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeTrue)
		test.That(t, result.LatestVersion, test.ShouldEqual, "v2.0.0")
	})

	t.Run("already up to date", func(t *testing.T) {
		client := newTestHTTPSServer(t, map[string][]byte{
			"/latest": []byte("v1.0.0\n"),
		})

		result, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
	})
}
