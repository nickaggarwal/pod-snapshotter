package fuseclient

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"pod-snapshotter/internal/pb"
)

// SessionClient pins artifact prefixes in the fuse-client cache via the
// AgentService gRPC socket (default /var/run/fuse-client/agent.sock).
//
// Pinning is genuinely enforced by fuse-client (pinned prefixes are excluded
// from NVMe eviction). The CachePolicy.Warmup field is stored but not acted
// on by fuse-client today, so pre-warming is done by the pod-snapshotter
// agent itself: it reads the artifact through the mount (see agent restore).
type SessionClient struct {
	conn *grpc.ClientConn
	cli  pb.AgentServiceClient
}

// DialSession connects to the fuse-client agent Unix socket.
func DialSession(socketPath string) (*SessionClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", socketPath)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing fuse-client agent socket %s: %w", socketPath, err)
	}
	return &SessionClient{conn: conn, cli: pb.NewAgentServiceClient(conn)}, nil
}

// Close releases the connection.
func (s *SessionClient) Close() error { return s.conn.Close() }

// Pin creates a pinned read-only session covering rootPath (a path prefix
// relative to the cache root, e.g. /snapshots/<ns>/<name>). volumeID must be
// unique per consumer; use the PodRestore UID.
func (s *SessionClient) Pin(ctx context.Context, volumeID, rootPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := s.cli.CreateSession(ctx, &pb.CreateSessionRequest{
		VolumeId: volumeID,
		RootPath: rootPath,
		ReadOnly: true,
		Policy: &pb.CachePolicy{
			CacheMode:    "readonly",
			Warmup:       "full", // informational; fuse-client does not act on it yet
			Pinned:       true,
			SourcePolicy: "peer-first",
		},
	})
	if err != nil {
		return fmt.Errorf("pinning %s: %w", rootPath, err)
	}
	return nil
}

// Unpin deletes the pin session.
func (s *SessionClient) Unpin(ctx context.Context, volumeID string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.cli.DeleteSession(ctx, &pb.DeleteSessionRequest{VolumeId: volumeID}); err != nil {
		return fmt.Errorf("unpinning session %s: %w", volumeID, err)
	}
	return nil
}
