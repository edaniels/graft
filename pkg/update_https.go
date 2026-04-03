package graft

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/edaniels/graft/errors"
)

// HTTPSReleaseClient implements ReleaseClient by fetching artifacts from a
// plain HTTPS server (e.g. a public S3 bucket).
//
// Expected server layout:
//
//	{baseURL}/latest                          - text file with current version
//	{baseURL}/{version}/checksums.txt         - SHA256 checksums
//	{baseURL}/{version}/graft-{os}-{arch}     - platform binary
type HTTPSReleaseClient struct {
	baseURL string
	client  *http.Client
}

// NewHTTPSReleaseClient creates a ReleaseClient backed by a plain HTTPS server.
func NewHTTPSReleaseClient(baseURL string) *HTTPSReleaseClient {
	return &HTTPSReleaseClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{},
	}
}

func (h *HTTPSReleaseClient) LatestVersion(ctx context.Context) (string, error) {
	url := h.baseURL + "/latest"

	data, err := httpGet(ctx, h.client, url, maxVersionSize)
	if err != nil {
		return "", errors.WrapPrefix(err, "fetching latest version")
	}

	return strings.TrimSpace(string(data)), nil
}

func (h *HTTPSReleaseClient) DownloadChecksums(ctx context.Context, version string) ([]byte, error) {
	url := h.baseURL + "/" + version + "/checksums.txt"

	data, err := httpGet(ctx, h.client, url, maxReleaseDownloadSize)
	if err != nil {
		return nil, errors.WrapPrefix(err, "downloading checksums")
	}

	return data, nil
}

func (h *HTTPSReleaseClient) DownloadBinary(ctx context.Context, version, binaryName, destPath string) error {
	url := h.baseURL + "/" + version + "/" + binaryName

	data, err := httpGet(ctx, h.client, url, maxReleaseDownloadSize)
	if err != nil {
		return errors.WrapPrefix(err, "downloading binary")
	}

	if err := os.WriteFile(destPath, data, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

func (h *HTTPSReleaseClient) ReleaseNotes(ctx context.Context, version string) (string, error) {
	url := h.baseURL + "/" + version + "/release-notes.txt"

	data, err := httpGet(ctx, h.client, url, maxReleaseNotesSize)
	if err != nil {
		return "", errors.WrapPrefix(err, "fetching release notes")
	}

	return strings.TrimSpace(string(data)), nil
}
