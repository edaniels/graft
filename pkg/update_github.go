package graft

import (
	"context"
	"io"
	"os"

	"github.com/google/go-github/v84/github"

	"github.com/edaniels/graft/errors"
)

const (
	updateRepoOwner = "edaniels"
	updateRepoName  = "graft"
)

// GithubToken returns the GitHub token from environment variables.
// Checks GRAFT_GITHUB_TOKEN first, then GITHUB_TOKEN.
func GithubToken() string {
	if token := os.Getenv("GRAFT_GITHUB_TOKEN"); token != "" {
		return token
	}

	return os.Getenv("GITHUB_TOKEN")
}

// GitHubReleaseClient implements ReleaseClient using the GitHub Releases API.
type GitHubReleaseClient struct {
	client *github.Client
}

// NewGitHubReleaseClient creates a ReleaseClient backed by GitHub Releases.
func NewGitHubReleaseClient(token string) *GitHubReleaseClient {
	var client *github.Client
	if token != "" {
		client = github.NewClient(nil).WithAuthToken(token)
	} else {
		client = github.NewClient(nil)
	}

	return &GitHubReleaseClient{client: client}
}

// LatestVersion fetches the latest release tag from GitHub.
func (g *GitHubReleaseClient) LatestVersion(ctx context.Context) (string, error) {
	release, _, err := g.client.Repositories.GetLatestRelease(ctx, updateRepoOwner, updateRepoName)
	if err != nil {
		return "", errors.WrapPrefix(err, "fetching latest release")
	}

	return release.GetTagName(), nil
}

// DownloadChecksums downloads the checksums.txt asset for the given version.
func (g *GitHubReleaseClient) DownloadChecksums(ctx context.Context, version string) ([]byte, error) {
	release, err := g.releaseForVersion(ctx, version)
	if err != nil {
		return nil, err
	}

	assetID, err := findAssetID(release, "checksums.txt")
	if err != nil {
		return nil, err
	}

	return g.downloadAsset(ctx, assetID)
}

// DownloadBinary downloads the release binary to destPath.
func (g *GitHubReleaseClient) DownloadBinary(ctx context.Context, version, binaryName, destPath string) error {
	release, err := g.releaseForVersion(ctx, version)
	if err != nil {
		return err
	}

	assetID, err := findAssetID(release, binaryName)
	if err != nil {
		return err
	}

	data, err := g.downloadAsset(ctx, assetID)
	if err != nil {
		return err
	}

	if err := os.WriteFile(destPath, data, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

func (g *GitHubReleaseClient) ReleaseNotes(ctx context.Context, version string) (string, error) {
	release, err := g.releaseForVersion(ctx, version)
	if err != nil {
		return "", err
	}

	return release.GetBody(), nil
}

func (g *GitHubReleaseClient) releaseForVersion(ctx context.Context, version string) (*github.RepositoryRelease, error) {
	release, _, err := g.client.Repositories.GetReleaseByTag(ctx, updateRepoOwner, updateRepoName, version)
	if err != nil {
		return nil, errors.WrapPrefix(err, "fetching release")
	}

	return release, nil
}

func (g *GitHubReleaseClient) downloadAsset(ctx context.Context, assetID int64) ([]byte, error) {
	rc, _, err := g.client.Repositories.DownloadReleaseAsset(ctx,
		updateRepoOwner, updateRepoName, assetID, g.client.Client())
	if err != nil {
		return nil, errors.WrapPrefix(err, "downloading asset")
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, maxReleaseDownloadSize+1))
	if err != nil {
		return nil, errors.Wrap(err)
	}

	if int64(len(data)) > maxReleaseDownloadSize {
		return nil, errors.Errorf("asset too large (>%d bytes)", maxReleaseDownloadSize)
	}

	return data, nil
}

func findAssetID(release *github.RepositoryRelease, name string) (int64, error) {
	for _, asset := range release.Assets {
		if asset.GetName() == name {
			return asset.GetID(), nil
		}
	}

	return 0, errors.Errorf("asset not found in release: %s", name)
}
