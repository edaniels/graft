package graft

import (
	"context"
	"io"
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

// LatestVersion fetches the latest version string from the server.
func (h *HTTPSReleaseClient) LatestVersion(ctx context.Context) (string, error) {
	url := h.baseURL + "/latest"

	data, err := h.get(ctx, url, 1024)
	if err != nil {
		return "", errors.WrapPrefix(err, "fetching latest version")
	}

	return strings.TrimSpace(string(data)), nil
}

// DownloadChecksums downloads the checksums.txt for the given version.
func (h *HTTPSReleaseClient) DownloadChecksums(ctx context.Context, version string) ([]byte, error) {
	url := h.baseURL + "/" + version + "/checksums.txt"

	data, err := h.get(ctx, url, maxReleaseDownloadSize)
	if err != nil {
		return nil, errors.WrapPrefix(err, "downloading checksums")
	}

	return data, nil
}

// DownloadBinary downloads the release binary to destPath.
func (h *HTTPSReleaseClient) DownloadBinary(ctx context.Context, version, binaryName, destPath string) error {
	url := h.baseURL + "/" + version + "/" + binaryName

	data, err := h.get(ctx, url, maxReleaseDownloadSize)
	if err != nil {
		return errors.WrapPrefix(err, "downloading binary")
	}

	if err := os.WriteFile(destPath, data, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

func (h *HTTPSReleaseClient) get(ctx context.Context, url string, maxSize int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.Errorf("%s: %d %s", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, errors.Wrap(err)
	}

	if int64(len(data)) > maxSize {
		return nil, errors.Errorf("%s: response too large (>%d bytes)", url, maxSize)
	}

	return data, nil
}
