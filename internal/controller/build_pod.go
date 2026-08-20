package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	snapv1 "pod-snapshotter/api/v1alpha1"
)

// buildContainerIndex resolves spec.container against the build template,
// defaulting to the first container.
func buildContainerIndex(build *snapv1.SnapshotBuild) (int, error) {
	containers := build.Spec.PodTemplate.Spec.Containers
	if len(containers) == 0 {
		return 0, fmt.Errorf("spec.podTemplate has no containers")
	}
	if build.Spec.Container == "" {
		return 0, nil
	}
	for i, c := range containers {
		if c.Name == build.Spec.Container {
			return i, nil
		}
	}
	return 0, fmt.Errorf("container %q not found in spec.podTemplate", build.Spec.Container)
}

// BuildBuilderPod materializes the one-shot pod a SnapshotBuild checkpoints.
//
// Unlike a restore's placeholder pod, nothing about the workload is replaced:
// the build pod runs the real command, loads the real weights, and parks at
// the shim's quiesce point. Two things are forced, because a pod that exists
// only to be checkpointed has no reason to get them wrong:
//
//   - restartPolicy Never — a restart after the dump would produce a second,
//     un-checkpointed process holding the GPU.
//   - AppArmor Unconfined — the kubelet's default confinement is recorded in
//     the CRIU image and cannot be re-entered at restore
//     (docs/prerequisites.md, "Workload requirements").
func BuildBuilderPod(build *snapv1.SnapshotBuild, podName string) (*corev1.Pod, error) {
	if _, err := buildContainerIndex(build); err != nil {
		return nil, err
	}

	tmpl := build.Spec.PodTemplate.DeepCopy()
	pod := &corev1.Pod{
		ObjectMeta: tmpl.ObjectMeta,
		Spec:       tmpl.Spec,
	}
	pod.Namespace = build.Namespace
	pod.Name = podName
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["app.kubernetes.io/managed-by"] = "pod-snapshotter"
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[snapv1.BuildAnnotation] = build.Namespace + "/" + build.Name

	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if pod.Spec.SecurityContext.AppArmorProfile == nil {
		pod.Spec.SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{
			Type: corev1.AppArmorProfileTypeUnconfined,
		}
	}
	return pod, nil
}
