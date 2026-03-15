package graft

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/google/go-github/v84/github"
	"go.viam.com/test"
)

func newTestGitHubReleaseClient(t *testing.T) (*GitHubReleaseClient, *http.ServeMux) {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ghClient := github.NewClient(nil)
	baseURL, err := url.Parse(server.URL + "/")
	test.That(t, err, test.ShouldBeNil)

	ghClient.BaseURL = baseURL

	return &GitHubReleaseClient{
		client:         ghClient,
		cachedReleases: make(map[string]*github.RepositoryRelease),
	}, mux
}

func writeReleaseJSON(t *testing.T, w http.ResponseWriter, tagName string, assets ...*github.ReleaseAsset) {
	t.Helper()

	release := github.RepositoryRelease{
		TagName: &tagName,
		Assets:  assets,
	}

	data, err := json.Marshal(release)
	test.That(t, err, test.ShouldBeNil)

	_, err = w.Write(data)
	test.That(t, err, test.ShouldBeNil)
}

func TestGitHubReleaseClientCheckForUpdate(t *testing.T) {
	t.Run("update available", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/repos/edaniels/graft/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
			writeReleaseJSON(t, w, "v1.5.0")
		})

		result, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.LatestVersion, test.ShouldEqual, "v1.5.0")
		test.That(t, result.UpdateAvailable, test.ShouldBeTrue)
	})

	t.Run("already up to date", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/repos/edaniels/graft/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
			writeReleaseJSON(t, w, "v1.5.0")
		})

		result, err := CheckForUpdate(t.Context(), client, "v1.5.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
	})

	t.Run("dev build not updateable", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/repos/edaniels/graft/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
			writeReleaseJSON(t, w, "v1.5.0")
		})

		result, err := CheckForUpdate(t.Context(), client, "dev-abc1234")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
		test.That(t, result.IsDevBuild, test.ShouldBeTrue)
	})

	t.Run("prerelease skipped", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/repos/edaniels/graft/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
			writeReleaseJSON(t, w, "v2.0.0-rc1")
		})

		result, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, result.UpdateAvailable, test.ShouldBeFalse)
	})

	t.Run("API error", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)
		mux.HandleFunc("/repos/edaniels/graft/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		})

		_, err := CheckForUpdate(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "fetching latest release")
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

		mux.HandleFunc("/repos/edaniels/graft/releases/tags/v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
			writeReleaseJSON(t, w, "v1.0.0",
				&github.ReleaseAsset{ID: new(int64(10)), Name: new(binaryName)},
				&github.ReleaseAsset{ID: new(int64(20)), Name: new("checksums.txt")},
			)
		})
		mux.HandleFunc("/repos/edaniels/graft/releases/assets/10", func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write(binaryContent)
			test.That(t, wErr, test.ShouldBeNil)
		})
		mux.HandleFunc("/repos/edaniels/graft/releases/assets/20", func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write([]byte(checksumFile))
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

		mux.HandleFunc("/repos/edaniels/graft/releases/tags/v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
			writeReleaseJSON(t, w, "v1.0.0",
				&github.ReleaseAsset{ID: new(int64(10)), Name: new(binaryName)},
				&github.ReleaseAsset{ID: new(int64(20)), Name: new("checksums.txt")},
			)
		})
		mux.HandleFunc("/repos/edaniels/graft/releases/assets/10", func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write([]byte("different content"))
			test.That(t, wErr, test.ShouldBeNil)
		})
		mux.HandleFunc("/repos/edaniels/graft/releases/assets/20", func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write([]byte(checksumFile))
			test.That(t, wErr, test.ShouldBeNil)
		})

		tmpPath, _, err := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "checksum verification failed")
		test.That(t, tmpPath, test.ShouldBeEmpty)
	})

	t.Run("missing binary asset", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)

		mux.HandleFunc("/repos/edaniels/graft/releases/tags/v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
			writeReleaseJSON(t, w, "v1.0.0",
				&github.ReleaseAsset{ID: new(int64(20)), Name: new("checksums.txt")},
			)
		})
		mux.HandleFunc("/repos/edaniels/graft/releases/assets/20", func(w http.ResponseWriter, _ *http.Request) {
			_, wErr := w.Write([]byte(checksumFile))
			test.That(t, wErr, test.ShouldBeNil)
		})

		_, _, err := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "asset not found")
	})

	t.Run("missing checksums asset", func(t *testing.T) {
		client, mux := newTestGitHubReleaseClient(t)

		mux.HandleFunc("/repos/edaniels/graft/releases/tags/v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
			writeReleaseJSON(t, w, "v1.0.0",
				&github.ReleaseAsset{ID: new(int64(10)), Name: new(binaryName)},
			)
		})

		_, _, err := DownloadAndVerify(t.Context(), client, "v1.0.0")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "asset not found")
	})
}

func TestFindAssetID(t *testing.T) {
	release := &github.RepositoryRelease{
		TagName: new("v1.0.0"),
		Assets: []*github.ReleaseAsset{
			{ID: new(int64(100)), Name: new("graft-linux-amd64")},
			{ID: new(int64(200)), Name: new("graft-darwin-arm64")},
			{ID: new(int64(300)), Name: new("checksums.txt")},
		},
	}

	t.Run("finds binary asset", func(t *testing.T) {
		id, err := findAssetID(release, "graft-darwin-arm64")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, id, test.ShouldEqual, int64(200))
	})

	t.Run("finds checksums asset", func(t *testing.T) {
		id, err := findAssetID(release, "checksums.txt")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, id, test.ShouldEqual, int64(300))
	})

	t.Run("returns error for missing asset", func(t *testing.T) {
		_, err := findAssetID(release, "nonexistent")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "asset not found")
	})
}
