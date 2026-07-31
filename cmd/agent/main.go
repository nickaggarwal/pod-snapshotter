// The pod-snapshotter agent runs as a privileged DaemonSet on every
// checkpoint-capable node. It uploads kubelet checkpoint tars to the
// fuse-client mount, pre-warms and pins artifacts, performs runc/CRIU
// restores into placeholder pod sandboxes, and publishes node prerequisite
// status.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/agent"
	"pod-snapshotter/internal/cri"
	"pod-snapshotter/internal/fuseclient"
	"pod-snapshotter/internal/restore"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(snapv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		nodeName        string
		fuseMount       string
		fuseAPIEndpoint string
		fuseAgentSocket string
		checkpointsDir  string
		workRoot        string
		criSocket       string
		hostRoot        string
		skipHostChecks  bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8083", "Metrics endpoint address (0 to disable).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8084", "Health probe endpoint address.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "This node's name (downward API).")
	flag.StringVar(&fuseMount, "fuse-mount", "/mnt/fuse", "fuse-client mount point (as visible to this container).")
	flag.StringVar(&fuseAPIEndpoint, "fuse-api-endpoint", "http://127.0.0.1:8081", "Local fuse-client HTTP API (hostNetwork). Empty disables verification.")
	flag.StringVar(&fuseAgentSocket, "fuse-agent-socket", "/var/run/fuse-client/agent.sock", "fuse-client agent gRPC socket for artifact pinning. Empty disables pinning.")
	flag.StringVar(&checkpointsDir, "checkpoints-dir", "/var/lib/kubelet/checkpoints", "Kubelet checkpoints dir (hostPath mount).")
	flag.StringVar(&workRoot, "work-root", "/var/lib/pod-snapshotter/restores", "Node-local scratch for restore bundles (hostPath mount).")
	flag.StringVar(&criSocket, "cri-socket", "/run/containerd/containerd.sock", "CRI runtime socket.")
	flag.StringVar(&hostRoot, "host-root", "", "Host filesystem mount for file checks (e.g. /host).")
	flag.BoolVar(&skipHostChecks, "skip-host-checks", false, "Skip nsenter-based prereq checks (dev only).")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if nodeName == "" {
		setupLog.Error(nil, "node name is required (set NODE_NAME or -node-name)")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false, // per-node agent; no election
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Upload controller.
	upload := &agent.UploadReconciler{
		Client:              mgr.GetClient(),
		NodeName:            nodeName,
		FuseMount:           fuseMount,
		CheckpointsHostPath: checkpointsDir,
	}
	if fuseAPIEndpoint != "" {
		fuseHTTP := fuseclient.NewHTTPClient(fuseAPIEndpoint)
		upload.VerifyFuse = fuseHTTP.Stat
	}
	if err := upload.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "agent-upload")
		os.Exit(1)
	}

	// Restore controller.
	resolver, err := cri.NewResolver(criSocket)
	if err != nil {
		setupLog.Error(err, "unable to dial CRI socket", "socket", criSocket)
		os.Exit(1)
	}
	restoreCtrl := &agent.RestoreReconciler{
		Client:    mgr.GetClient(),
		NodeName:  nodeName,
		FuseMount: fuseMount,
		WorkRoot:  workRoot,
		HostRoot:  hostRoot,
		Resolver:  resolver,
		Runc:      restore.NewHostRunc(),
	}
	if fuseAgentSocket != "" {
		if pinner, err := fuseclient.DialSession(fuseAgentSocket); err != nil {
			setupLog.Info("fuse-client agent socket unavailable; pinning disabled", "err", err)
		} else {
			restoreCtrl.Pinner = pinner
		}
	}
	if err := restoreCtrl.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "agent-restore")
		os.Exit(1)
	}

	// Prereq checker.
	prereq := &agent.PrereqChecker{
		Client:         mgr.GetClient(),
		NodeName:       nodeName,
		FuseMount:      fuseMount,
		HostRoot:       hostRoot,
		SkipHostChecks: skipHostChecks,
	}
	if err := mgr.Add(prereq); err != nil {
		setupLog.Error(err, "unable to add prereq checker")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting agent", "node", nodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "agent exited with error")
		os.Exit(1)
	}
}
