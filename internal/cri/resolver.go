// Package cri resolves pod sandboxes and containers through the CRI runtime
// API (containerd / CRI-O socket) — enough to find the placeholder pod's
// pause PID (namespace paths) and the keeper container's rootfs.
package cri

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// SandboxInfo is what the restore pipeline needs from the CRI.
type SandboxInfo struct {
	SandboxID string
	// PausePID is the sandbox (pause container) init PID on the host; its
	// /proc/<pid>/ns/{net,ipc,uts} are the namespaces to join.
	PausePID int
	// KeeperContainerID is the CRI id of the keeper container.
	KeeperContainerID string
	// KeeperPID is the keeper's init PID (fallback namespace source when the
	// runtime does not expose the sandbox pid).
	KeeperPID int
	// KeeperRootfs is the keeper container's rootfs on the host. The keeper
	// runs the SAME image as the checkpointed container and only executes
	// `sleep`, so the restored workload reuses this rootfs (with
	// rootfs-diff.tar applied on top) instead of preparing a new snapshot.
	KeeperRootfs string
	// KeeperMounts maps destination -> source from the keeper's OCI config.
	// Runtime-generated mount sources (nvidia-ctk hook files, etc.) for the
	// NEW pod live here; the spec rewriter borrows them when the old spec's
	// source has vanished with the old pod.
	KeeperMounts map[string]string
	// PodCgroupPath is the placeholder pod's cgroup (v2) path, e.g.
	// /kubepods.slice/kubepods-burstable.slice/kubepods-...-pod<uid>.slice
	PodCgroupPath string
}

// Resolver queries the CRI runtime service.
type Resolver struct {
	client runtimeapi.RuntimeServiceClient
	conn   *grpc.ClientConn
}

// NewResolver dials the CRI socket (e.g. /run/containerd/containerd.sock).
func NewResolver(socketPath string) (*Resolver, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", socketPath)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing CRI socket %s: %w", socketPath, err)
	}
	return &Resolver{client: runtimeapi.NewRuntimeServiceClient(conn), conn: conn}, nil
}

// Close releases the connection.
func (r *Resolver) Close() error { return r.conn.Close() }

// criInfo is the verbose "info" blob containerd/CRI-O attach to status
// responses; pid is the only field we need.
type criInfo struct {
	PID int `json:"pid"`
}

// Resolve finds the sandbox and keeper container for a pod by UID.
func (r *Resolver) Resolve(ctx context.Context, podUID, keeperContainerName string) (*SandboxInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	sandboxes, err := r.client.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{
			LabelSelector: map[string]string{"io.kubernetes.pod.uid": podUID},
			State:         &runtimeapi.PodSandboxStateValue{State: runtimeapi.PodSandboxState_SANDBOX_READY},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("listing sandboxes for pod uid %s: %w", podUID, err)
	}
	if len(sandboxes.Items) == 0 {
		return nil, fmt.Errorf("no ready sandbox found for pod uid %s", podUID)
	}
	sb := sandboxes.Items[0]
	info := &SandboxInfo{SandboxID: sb.Id}

	// Sandbox pause PID from verbose status.
	sbStatus, err := r.client.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{
		PodSandboxId: sb.Id, Verbose: true,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox status %s: %w", sb.Id, err)
	}
	if raw, ok := sbStatus.Info["info"]; ok {
		var ci criInfo
		if json.Unmarshal([]byte(raw), &ci) == nil {
			info.PausePID = ci.PID
		}
	}

	// Keeper container.
	containers, err := r.client.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{
			PodSandboxId: sb.Id,
			State:        &runtimeapi.ContainerStateValue{State: runtimeapi.ContainerState_CONTAINER_RUNNING},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers in sandbox %s: %w", sb.Id, err)
	}
	for _, c := range containers.Containers {
		if keeperContainerName == "" || c.Metadata.GetName() == keeperContainerName {
			info.KeeperContainerID = c.Id
			cStatus, err := r.client.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{
				ContainerId: c.Id, Verbose: true,
			})
			if err != nil {
				return nil, fmt.Errorf("container status %s: %w", c.Id, err)
			}
			if raw, ok := cStatus.Info["info"]; ok {
				var ci criInfo
				if json.Unmarshal([]byte(raw), &ci) == nil {
					info.KeeperPID = ci.PID
				}
			}
			break
		}
	}
	if info.KeeperContainerID == "" {
		return nil, fmt.Errorf("keeper container %q not running in sandbox %s", keeperContainerName, sb.Id)
	}

	// The keeper's rootfs: containerd v2 task bundles live at a fixed path
	// keyed by container id. runc requires a real absolute path (it rejects
	// /proc/<pid>/root, which is a magic symlink).
	if info.KeeperContainerID != "" {
		p := fmt.Sprintf("/run/containerd/io.containerd.runtime.v2.task/k8s.io/%s/rootfs", info.KeeperContainerID)
		if _, err := os.Stat(p); err == nil {
			info.KeeperRootfs = p
		} else if info.KeeperPID > 0 {
			// Fallback for non-containerd layouts.
			info.KeeperRootfs = fmt.Sprintf("/proc/%d/root", info.KeeperPID)
		}
	}
	// The placeholder pod's real cgroup: parent of the keeper container's
	// scope, read from host /proc (requires hostPID). Restored containers
	// are placed here so they inherit the POD's limits (GPU/memory), not
	// the agent's.
	if info.KeeperPID > 0 {
		if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", info.KeeperPID)); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				// cgroup v2: "0::/kubepods.slice/.../cri-containerd-<id>.scope"
				if rest, ok := strings.CutPrefix(line, "0::"); ok {
					info.PodCgroupPath = path.Dir(rest)
					break
				}
			}
		}
	}

	// Keeper OCI config: destination->source of its mounts.
	if info.KeeperContainerID != "" {
		cfgPath := fmt.Sprintf("/run/containerd/io.containerd.runtime.v2.task/k8s.io/%s/config.json", info.KeeperContainerID)
		if raw, err := os.ReadFile(cfgPath); err == nil {
			var cfg struct {
				Mounts []struct {
					Destination string `json:"destination"`
					Source      string `json:"source"`
				} `json:"mounts"`
			}
			if json.Unmarshal(raw, &cfg) == nil {
				info.KeeperMounts = make(map[string]string, len(cfg.Mounts))
				for _, m := range cfg.Mounts {
					info.KeeperMounts[m.Destination] = m.Source
				}
			}
		}
	}

	// Namespace fallback: some runtimes omit the sandbox pid in verbose info.
	if info.PausePID == 0 {
		info.PausePID = info.KeeperPID
	}
	if info.PausePID == 0 {
		return nil, fmt.Errorf("could not determine a PID for sandbox %s namespaces", sb.Id)
	}
	return info, nil
}
