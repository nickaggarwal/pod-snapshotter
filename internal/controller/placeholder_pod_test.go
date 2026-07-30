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
			PodTemplate: corev1.PodTemplateSpec{
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
