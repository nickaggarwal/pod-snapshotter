// The pod-snapshotter manager runs the PodSnapshot and PodRestore
// controllers: it calls the kubelet checkpoint API, creates placeholder pods
// for restores, and coordinates with the node agents through CRD status.
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
	"pod-snapshotter/internal/artifact"
	"pod-snapshotter/internal/controller"
	"pod-snapshotter/internal/fuseclient"
	"pod-snapshotter/internal/kubelet"
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
		metricsAddr         string
		probeAddr           string
		enableLeaderElection bool
		kubeletPort         int
		kubeletInsecureTLS  bool
		kubeletCAFile       string
		fuseAPIEndpoint     string
		requirePrereqs      bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Metrics endpoint address (0 to disable).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "Health probe endpoint address.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for HA.")
	flag.IntVar(&kubeletPort, "kubelet-port", 10250, "Kubelet API port.")
	flag.BoolVar(&kubeletInsecureTLS, "kubelet-insecure-tls", false, "Skip kubelet serving cert verification (self-signed kubelets).")
	flag.StringVar(&kubeletCAFile, "kubelet-ca-file", "", "Extra CA bundle for kubelet serving certs.")
	flag.StringVar(&fuseAPIEndpoint, "fuse-api-endpoint", "", "fuse-client HTTP API endpoint for artifact stat/delete, e.g. http://fuse-client.fuse-system:8081. Empty disables artifact verification.")
	flag.BoolVar(&requirePrereqs, "require-node-prereqs", true, "Only checkpoint pods on nodes whose agent reports prereqs ok.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "pod-snapshotter.podsnapshot.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	kubeletClient, err := kubelet.New(kubeletInsecureTLS, kubeletCAFile, kubelet.WithPort(kubeletPort))
	if err != nil {
		setupLog.Error(err, "unable to create kubelet client")
		os.Exit(1)
	}

	var store *artifact.Store
	if fuseAPIEndpoint != "" {
		store = &artifact.Store{Fuse: fuseclient.NewHTTPClient(fuseAPIEndpoint)}
	} else {
		store = &artifact.Store{}
	}

	if err := (&controller.PodSnapshotReconciler{
		Client:         mgr.GetClient(),
		Kubelet:        kubeletClient,
		Artifacts:      store,
		RequirePrereqs: requirePrereqs,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PodSnapshot")
		os.Exit(1)
	}
	if err := (&controller.PodRestoreReconciler{
		Client:    mgr.GetClient(),
		Artifacts: store,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PodRestore")
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

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
