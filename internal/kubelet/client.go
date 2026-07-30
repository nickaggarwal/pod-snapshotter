// Package kubelet implements a minimal client for the kubelet checkpoint API.
//
// POST https://<node>:10250/checkpoint/{namespace}/{pod}/{container}
// requires the ContainerCheckpoint feature gate (beta, on by default since
// Kubernetes 1.30), a CRI runtime with checkpoint support (containerd >= 2.0
// or CRI-O >= 1.25), and CRIU installed on the node. The kubelet authorizes
// the call via SubjectAccessReview on nodes/proxy (verb: create).
package kubelet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultPort          = 10250
	serviceAccountToken  = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- path, not a credential
	serviceAccountCACert = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// checkpointResponse is the kubelet's response body.
type checkpointResponse struct {
	Items []string `json:"items"`
}

// Client calls the kubelet checkpoint endpoint on cluster nodes.
type Client struct {
	httpClient *http.Client
	port       int
	tokenFn    func() (string, error)

	// NodeAddressOverride, if set, replaces the node address (tests).
	NodeAddressOverride string
}

// Option configures the Client.
type Option func(*Client)

// WithPort overrides the kubelet port (default 10250).
func WithPort(port int) Option { return func(c *Client) { c.port = port } }

// WithTokenFunc overrides how the bearer token is obtained.
func WithTokenFunc(fn func() (string, error)) Option {
	return func(c *Client) { c.tokenFn = fn }
}

// WithHTTPClient overrides the underlying HTTP client entirely (tests).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// New builds a Client. If insecureTLS is false, kubelet serving certs are
// verified against the mounted service-account CA (plus optional caFile).
// Many clusters run kubelets with self-signed serving certs; those need
// insecureTLS=true (the same trade-off metrics-server documents).
func New(insecureTLS bool, caFile string, opts ...Option) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecureTLS {
		tlsCfg.InsecureSkipVerify = true // #nosec G402 -- explicit operator opt-in for self-signed kubelets
	} else {
		pool := x509.NewCertPool()
		for _, f := range []string{serviceAccountCACert, caFile} {
			if f == "" {
				continue
			}
			pem, err := os.ReadFile(f)
			if err != nil {
				if os.IsNotExist(err) && f == serviceAccountCACert {
					continue
				}
				return nil, fmt.Errorf("reading CA file %s: %w", f, err)
			}
			pool.AppendCertsFromPEM(pem)
		}
		tlsCfg.RootCAs = pool
	}

	c := &Client{
		httpClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
		port:       defaultPort,
		tokenFn: func() (string, error) {
			b, err := os.ReadFile(serviceAccountToken)
			if err != nil {
				return "", fmt.Errorf("reading service account token: %w", err)
			}
			return strings.TrimSpace(string(b)), nil
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Checkpoint asks the kubelet on nodeAddress to checkpoint the container and
// returns the node-local tar path the kubelet wrote (under
// /var/lib/kubelet/checkpoints/). The call is synchronous: for GPU workloads
// cuda-checkpoint drains CUDA work and copies VRAM to host memory before CRIU
// dumps, so timeouts must accommodate multi-GB transfers.
func (c *Client) Checkpoint(ctx context.Context, nodeAddress, namespace, pod, container string, timeout time.Duration) (string, error) {
	addr := nodeAddress
	if c.NodeAddressOverride != "" {
		addr = c.NodeAddressOverride
	}
	url := fmt.Sprintf("https://%s:%d/checkpoint/%s/%s/%s?timeout=%d",
		addr, c.port, namespace, pod, container, int(timeout.Seconds()))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	token, err := c.tokenFn()
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling kubelet checkpoint endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("kubelet returned 404: ContainerCheckpoint feature gate likely disabled on node %s (body: %s)", addr, strings.TrimSpace(string(body)))
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("kubelet returned %d: check nodes/proxy RBAC for the manager service account (body: %s)", resp.StatusCode, strings.TrimSpace(string(body)))
	default:
		return "", fmt.Errorf("kubelet checkpoint failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var cr checkpointResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("parsing kubelet checkpoint response: %w (body: %s)", err, strings.TrimSpace(string(body)))
	}
	if len(cr.Items) == 0 {
		return "", fmt.Errorf("kubelet checkpoint response contained no archive path")
	}
	return cr.Items[0], nil
}
