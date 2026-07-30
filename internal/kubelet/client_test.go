package kubelet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeKubelet implements the checkpoint endpoint shape.
func fakeKubelet(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(true, "",
		WithHTTPClient(srv.Client()),
		WithTokenFunc(func() (string, error) { return "test-token", nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Point the client at the test server: strip scheme, split host:port.
	addr := strings.TrimPrefix(srv.URL, "https://")
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("unexpected test server addr %s", addr)
	}
	c.NodeAddressOverride = host
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	c.port = p
	return c, srv
}

func TestCheckpointSuccess(t *testing.T) {
	var gotPath, gotAuth string
	c, _ := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		json.NewEncoder(w).Encode(map[string][]string{
			"items": {"/var/lib/kubelet/checkpoints/checkpoint-vllm-0_default-vllm-2026.tar"},
		})
	})

	tarPath, err := c.Checkpoint(context.Background(), "ignored", "default", "vllm-0", "vllm", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/var/lib/kubelet/checkpoints/checkpoint-vllm-0_default-vllm-2026.tar"; tarPath != want {
		t.Errorf("tarPath = %q, want %q", tarPath, want)
	}
	if want := "/checkpoint/default/vllm-0/vllm"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("auth header = %q", gotAuth)
	}
}

func TestCheckpointFeatureGateDisabled(t *testing.T) {
	c, _ := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	})
	_, err := c.Checkpoint(context.Background(), "ignored", "ns", "pod", "ctr", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "ContainerCheckpoint feature gate") {
		t.Errorf("expected feature-gate hint, got: %v", err)
	}
}

func TestCheckpointForbidden(t *testing.T) {
	c, _ := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
	})
	_, err := c.Checkpoint(context.Background(), "ignored", "ns", "pod", "ctr", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "nodes/proxy RBAC") {
		t.Errorf("expected RBAC hint, got: %v", err)
	}
}

func TestCheckpointCRIUFailure(t *testing.T) {
	c, _ := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "checkpointing of default/pod/ctr failed: CRIU binary not found", http.StatusInternalServerError)
	})
	_, err := c.Checkpoint(context.Background(), "ignored", "ns", "pod", "ctr", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "CRIU binary not found") {
		t.Errorf("expected kubelet body surfaced, got: %v", err)
	}
}

func TestCheckpointEmptyItems(t *testing.T) {
	c, _ := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string][]string{"items": {}})
	})
	_, err := c.Checkpoint(context.Background(), "ignored", "ns", "pod", "ctr", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "no archive path") {
		t.Errorf("expected empty-items error, got: %v", err)
	}
}
