package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	snapv1 "pod-snapshotter/api/v1alpha1"
)

func sampleRestore() *snapv1.PodRestore {
	return &snapv1.PodRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "default", UID: "uid-r1"},
		Spec: snapv1.PodRestoreSpec{
			ArtifactURI: "fuse:///snapshots/default/s1/vllm.tar",
			NodeName:    "gpu-node-1",
			PodTemplate: snapv1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "vllm",
						Image:   "vllm/vllm-openai:v0.9",
						Command: []string{"python", "-m", "vllm.entrypoints.openai.api_server"},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8000)},
							},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8000)},
							},
						},
					}},
				},
			},
		},
	}
}

func TestBuildPlaceholderPod(t *testing.T) {
	pod, err := BuildPlaceholderPod(sampleRestore(), "r1-restored")
	if err != nil {
		t.Fatal(err)
	}

	c := pod.Spec.Containers[0]
	if c.Command[0] != "sh" {
		t.Errorf("command not rewritten to keeper: %v", c.Command)
	}
	if c.Args != nil {
		t.Errorf("args should be cleared, got %v", c.Args)
	}
	// GPU limits preserved: the placeholder holds the allocation.
	if c.Resources.Limits["nvidia.com/gpu"] != resource.MustParse("1") {
		t.Error("GPU limit not preserved")
	}
	// Readiness preserved (probes the restored workload via shared netns);
	// liveness dropped (would kill the keeper).
	if c.ReadinessProbe == nil {
		t.Error("readiness probe should be preserved")
	}
	if c.LivenessProbe != nil {
		t.Error("liveness probe should be removed")
	}
	if pod.Spec.NodeName != "gpu-node-1" {
		t.Errorf("nodeName = %q", pod.Spec.NodeName)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	if pod.Annotations[snapv1.RestoreAnnotation] != "default/r1" {
		t.Errorf("restore annotation = %q", pod.Annotations[snapv1.RestoreAnnotation])
	}
}

func TestBuildPlaceholderPodContainerSelection(t *testing.T) {
	r := sampleRestore()
	r.Spec.Container = "does-not-exist"
	if _, err := BuildPlaceholderPod(r, "x"); err == nil {
		t.Error("expected error for unknown container")
	}

	r.Spec.Container = ""
	r.Spec.PodTemplate.Spec.Containers = nil
	if _, err := BuildPlaceholderPod(r, "x"); err == nil {
		t.Error("expected error for empty template")
	}
}

func TestBuildPlaceholderPodCarriesTemplateMetadata(t *testing.T) {
	restore := sampleRestore()
	restore.Spec.PodTemplate.Metadata = snapv1.EmbeddedObjectMeta{
		Labels:      map[string]string{"app": "vllm"},
		Annotations: map[string]string{snapv1.QuiesceAnnotation: snapv1.QuiesceModePresenceFile},
	}
	pod, err := BuildPlaceholderPod(restore, "r1-restored")
	if err != nil {
		t.Fatal(err)
	}
	if pod.Labels["app"] != "vllm" {
		t.Errorf("template labels dropped: %v", pod.Labels)
	}
	if pod.Annotations[snapv1.QuiesceAnnotation] != snapv1.QuiesceModePresenceFile {
		t.Errorf("quiesce annotation dropped: %v", pod.Annotations)
	}
	// The operator's own markers still win their keys.
	if pod.Annotations[snapv1.RestoreAnnotation] != "default/r1" {
		t.Errorf("restore back-reference missing: %v", pod.Annotations)
	}
}

func TestBuildPlaceholderPodConfinesToCompatibleNodes(t *testing.T) {
	restore := sampleRestore()
	restore.Spec.NodeName = ""
	restore.Spec.NodeSelector = map[string]string{"pool": "gpuckpt"}
	restore.Status.Compatibility = &snapv1.CompatibilityKey{
		GPUModel: "NVIDIA A100 80GB PCIe", DriverVersion: "580.159.04", CRIUVersion: "4.2.1",
	}

	pod, err := BuildPlaceholderPod(restore, "r1-restored")
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.NodeSelector["pool"] != "gpuckpt" {
		t.Errorf("user nodeSelector lost: %v", pod.Spec.NodeSelector)
	}
	want := restore.Status.Compatibility.NodeHash()
	if got := pod.Spec.NodeSelector[snapv1.CompatibilityHashLabel]; got != want {
		t.Errorf("compat selector = %q, want %q", got, want)
	}

	// Without a build reference nothing is constrained.
	plain := sampleRestore()
	plain.Spec.NodeName = ""
	pod, err = BuildPlaceholderPod(plain, "r1-restored")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pod.Spec.NodeSelector[snapv1.CompatibilityHashLabel]; ok {
		t.Error("a restore with no compatibility tuple must not constrain placement")
	}
}

func TestBuildBuilderPodForcesCheckpointableSettings(t *testing.T) {
	build := &snapv1.SnapshotBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "default"},
		Spec: snapv1.SnapshotBuildSpec{
			Revision: "rev-1",
			PodTemplate: snapv1.PodTemplate{
				Metadata: snapv1.EmbeddedObjectMeta{
					Annotations: map[string]string{snapv1.QuiesceAnnotation: snapv1.QuiesceModePresenceFile},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyAlways,
					Containers:    []corev1.Container{{Name: "vllm", Image: "vllm/vllm-openai:v0.9.2"}},
				},
			},
		},
	}
	pod, err := BuildBuilderPod(build, "b1-build")
	if err != nil {
		t.Fatal(err)
	}
	// The workload's real command survives — unlike a placeholder pod, a
	// build pod is supposed to actually run.
	if pod.Spec.Containers[0].Image != "vllm/vllm-openai:v0.9.2" {
		t.Errorf("build container rewritten: %+v", pod.Spec.Containers[0])
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.AppArmorProfile == nil ||
		pod.Spec.SecurityContext.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined {
		t.Errorf("build pod must default to AppArmor Unconfined: %+v", pod.Spec.SecurityContext)
	}
	if pod.Annotations[snapv1.QuiesceAnnotation] != snapv1.QuiesceModePresenceFile {
		t.Errorf("quiesce annotation dropped: %v", pod.Annotations)
	}
	if pod.Annotations[snapv1.BuildAnnotation] != "default/b1" {
		t.Errorf("build back-reference missing: %v", pod.Annotations)
	}
}

func TestBuildBuilderPodKeepsExplicitAppArmor(t *testing.T) {
	build := &snapv1.SnapshotBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "default"},
		Spec: snapv1.SnapshotBuildSpec{
			Revision: "rev-1",
			PodTemplate: snapv1.PodTemplate{Spec: corev1.PodSpec{
				SecurityContext: &corev1.PodSecurityContext{
					AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			}},
		},
	}
	pod, err := BuildBuilderPod(build, "b1-build")
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.SecurityContext.AppArmorProfile.Type != corev1.AppArmorProfileTypeRuntimeDefault {
		t.Error("an explicitly chosen AppArmor profile must not be overridden")
	}
}
