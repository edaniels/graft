package graft

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/edaniels/graft/errors"
)

const (
	updateRepoOwner = "edaniels"
	updateRepoName  = "graft"
	githubBaseURL   = "https://github.com"
)

// GitHubReleaseClient implements ReleaseClient using GitHub's direct
// download URLs instead of the API, avoiding rate limits. Layout:
//
//	{baseURL}/releases/latest/download/version.txt
//	{baseURL}/releases/download/{version}/checksums.txt
//	{baseURL}/releases/download/{version}/release-notes.txt
//	{baseURL}/releases/download/{version}/graft-{os}-{arch}
type GitHubReleaseClient struct {
	baseURL string
	client  *http.Client
}

func newGitHubReleaseClient(baseURL string) *GitHubReleaseClient {
	return &GitHubReleaseClient{
		baseURL: baseURL,
		client:  &http.Client{},
	}
}

// NewGitHubReleaseClient creates a ReleaseClient backed by GitHub's direct download URLs.
func NewGitHubReleaseClient() *GitHubReleaseClient {
	return newGitHubReleaseClient(fmt.Sprintf("%s/%s/%s", githubBaseURL, updateRepoOwner, updateRepoName))
}

func (g *GitHubReleaseClient) LatestVersion(ctx context.Context) (string, error) {
	url := g.baseURL + "/releases/latest/download/version.txt"

	data, err := httpGet(ctx, g.client, url, maxVersionSize)
	if err != nil {
		return "", errors.WrapPrefix(err, "fetching latest version")
	}

	return strings.TrimSpace(string(data)), nil
}

func (g *GitHubReleaseClient) DownloadChecksums(ctx context.Context, version string) ([]byte, error) {
	url := g.baseURL + "/releases/download/" + version + "/checksums.txt"

	data, err := httpGet(ctx, g.client, url, maxReleaseDownloadSize)
	if err != nil {
		return nil, errors.WrapPrefix(err, "downloading checksums")
	}

	return data, nil
}

func (g *GitHubReleaseClient) DownloadBinary(ctx context.Context, version, binaryName, destPath string) error {
	url := g.baseURL + "/releases/download/" + version + "/" + binaryName

	data, err := httpGet(ctx, g.client, url, maxReleaseDownloadSize)
	if err != nil {
		return errors.WrapPrefix(err, "downloading binary")
	}

	if err := os.WriteFile(destPath, data, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

func (g *GitHubReleaseClient) ReleaseNotes(ctx context.Context, version string) (string, error) {
	url := g.baseURL + "/releases/download/" + version + "/release-notes.txt"

	data, err := httpGet(ctx, g.client, url, maxReleaseNotesSize)
	if err != nil {
		return "", errors.WrapPrefix(err, "fetching release notes")
	}

	return strings.TrimSpace(string(data)), nil
}
