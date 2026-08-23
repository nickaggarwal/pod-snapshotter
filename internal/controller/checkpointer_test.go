package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapv1 "pod-snapshotter/api/v1alpha1"
)

func nodeWith(name, caps string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if caps != "" {
		n.Annotations = map[string]string{snapv1.CapabilitiesAnnotation: caps}
	}
	return n
}

func snapWith(checkpointer, uri string) *snapv1.PodSnapshot {
	return &snapv1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s"},
		Spec:       snapv1.PodSnapshotSpec{PodName: "vllm", Checkpointer: checkpointer},
		Status: snapv1.PodSnapshotStatus{
			NodeName: "node-a",
			Phase:    snapv1.SnapshotPhaseCheckpointing,
			Artifact: &snapv1.ArtifactStatus{URI: uri},
		},
	}
}

// Routing has to degrade, never stall: every reason the agent cannot take the
// dump has to come back as "kubelet, and here is why", because a snapshot
// that silently waits for an agent that will never act looks identical to a
// slow checkpoint.
func TestAgentCheckpointRouting(t *testing.T) {
	const dirURI = "fuse:///snapshots/default/s/vllm/"
	const tarURI = "fuse:///snapshots/default/s/vllm.tar"

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := snapv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		node    *corev1.Node
		snap    *snapv1.PodSnapshot
		want    bool
		wantWhy string
	}{
		{
			name: "capable agent, directory artifact",
			node: nodeWith("node-a", snapv1.AgentCheckpointCapability),
			snap: snapWith(snapv1.CheckpointerAgent, dirURI),
			want: true,
		},
		{
			// Not a fallback message: nobody asked for the agent, so there is
			// nothing to explain.
			name: "kubelet was asked for",
			node: nodeWith("node-a", snapv1.AgentCheckpointCapability),
			snap: snapWith(snapv1.CheckpointerKubelet, dirURI),
		},
		{
			name:    "a tar artifact cannot be produced directly",
			node:    nodeWith("node-a", snapv1.AgentCheckpointCapability),
			snap:    snapWith(snapv1.CheckpointerAgent, tarURI),
			wantWhy: "directory artifact",
		},
		{
			// The upgrade case: new manager, old agent. It must not strand.
			name:    "agent too old to advertise the capability",
			node:    nodeWith("node-a", ""),
			snap:    snapWith(snapv1.CheckpointerAgent, dirURI),
			wantWhy: snapv1.AgentCheckpointCapability,
		},
		{
			// Capabilities is a list; a node advertising others must still
			// match on the one being looked for.
			name: "capability among others",
			node: nodeWith("node-a", "something-else,"+snapv1.AgentCheckpointCapability+",more"),
			snap: snapWith(snapv1.CheckpointerAgent, dirURI),
			want: true,
		},
		{
			// A prefix match would say yes here, and route the dump to an
			// agent that cannot run it.
			name:    "a capability that merely contains the name is not it",
			node:    nodeWith("node-a", "not-"+snapv1.AgentCheckpointCapability+"-either"),
			snap:    snapWith(snapv1.CheckpointerAgent, dirURI),
			wantWhy: snapv1.AgentCheckpointCapability,
		},
		{
			name:    "node has vanished",
			node:    nodeWith("other", snapv1.AgentCheckpointCapability),
			snap:    snapWith(snapv1.CheckpointerAgent, dirURI),
			wantWhy: "agent capabilities",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.node).Build()
			r := &PodSnapshotReconciler{Client: c}

			got, why := r.agentCheckpoints(context.Background(), tc.snap)
			if got != tc.want {
				t.Fatalf("agentCheckpoints = %v (%q), want %v", got, why, tc.want)
			}
			if tc.wantWhy == "" {
				if why != "" {
					t.Fatalf("unexpected fallback reason %q", why)
				}
				return
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Fatalf("reason %q does not mention %q", why, tc.wantWhy)
			}
			if !strings.Contains(why, "kubelet") {
				t.Fatalf("reason %q does not say what it fell back to", why)
			}
		})
	}
}
