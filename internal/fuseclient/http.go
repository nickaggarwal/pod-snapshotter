// Package fuseclient integrates with the sibling fuse-client project's
// per-node HTTP API (default 127.0.0.1:8081) and its FUSE mount. It never
// modifies fuse-client; it only consumes its published interfaces:
//
//	GET/PUT/DELETE/HEAD /api/files/{path}
//	GET                 /api/health
//	GET                 /api/cache/stats
package fuseclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPClient talks to a fuse-client node API endpoint.
type HTTPClient struct {
	BaseURL string // e.g. http://127.0.0.1:8081
	HTTP    *http.Client
}

// NewHTTPClient builds a client for the given endpoint.
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
	}
}

func (c *HTTPClient) fileURL(fusePath string) string {
	// Preserve path segments; escape each one.
	segs := strings.Split(strings.TrimPrefix(fusePath, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return c.BaseURL + "/api/files/" + strings.Join(segs, "/")
}

// Health returns nil when the node's fuse-client reports healthy.
func (c *HTTPClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("fuse-client health check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fuse-client health returned %d", resp.StatusCode)
	}
	return nil
}

// Stat HEADs a file and returns its size.
func (c *HTTPClient) Stat(ctx context.Context, fusePath string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.fileURL(fusePath), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", fusePath, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, fmt.Errorf("artifact %s not found on fuse-client", fusePath)
	default:
		return 0, fmt.Errorf("stat %s returned %d", fusePath, resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			return n, nil
		}
	}
	return 0, nil
}

// Delete removes a file.
func (c *HTTPClient) Delete(ctx context.Context, fusePath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.fileURL(fusePath), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("delete %s: %w", fusePath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("delete %s returned %d: %s", fusePath, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
