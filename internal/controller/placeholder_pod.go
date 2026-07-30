package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	snapv1 "pod-snapshotter/api/v1alpha1"
)

// targetContainerIndex resolves spec.container against the pod template,
// defaulting to the first container.
func targetContainerIndex(spec *snapv1.PodRestoreSpec) (int, error) {
	containers := spec.PodTemplate.Spec.Containers
	if len(containers) == 0 {
		return 0, fmt.Errorf("spec.podTemplate has no containers")
	}
	if spec.Container == "" {
		return 0, nil
	}
	for i, c := range containers {
		if c.Name == spec.Container {
			return i, nil
		}
	}
	return 0, fmt.Errorf("container %q not found in spec.podTemplate", spec.Container)
}

// BuildPlaceholderPod materializes the restore target pod from the template.
//
// The target container's command is rewritten to a keeper process
// (sleep infinity): it holds the pod sandbox — network/IPC/UTS namespaces,
// the GPU allocation from the device plugin, kubelet-managed volumes — while
// the node agent runc-restores the checkpointed workload INTO that sandbox as
// a sibling runc container. Probes are left as authored: because the restored
// workload shares the pod network namespace, a readiness probe against the
// workload's port turns Ready exactly when the restored server answers.
func BuildPlaceholderPod(restore *snapv1.PodRestore, podName string) (*corev1.Pod, error) {
	idx, err := targetContainerIndex(&restore.Spec)
	if err != nil {
		return nil, err
	}

	tmpl := restore.Spec.PodTemplate.DeepCopy()
	pod := &corev1.Pod{
		ObjectMeta: tmpl.ObjectMeta,
		Spec:       tmpl.Spec,
	}
	pod.Namespace = restore.Namespace
	pod.Name = podName
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["app.kubernetes.io/managed-by"] = "pod-snapshotter"
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[snapv1.RestoreAnnotation] = restore.Namespace + "/" + restore.Name

	c := &pod.Spec.Containers[idx]
	// Keeper: hold the sandbox without consuming meaningful CPU. Liveness/
	// startup probes authored against the workload would kill the keeper
	// before restore completes, so only the readiness probe is preserved.
	c.Command = []string{"sh", "-c", "trap 'exit 0' TERM; sleep infinity & wait"}
	c.Args = nil
	c.LivenessProbe = nil
	c.StartupProbe = nil

	if restore.Spec.NodeName != "" {
		pod.Spec.NodeName = restore.Spec.NodeName
	}
	if len(restore.Spec.NodeSelector) > 0 {
		if pod.Spec.NodeSelector == nil {
			pod.Spec.NodeSelector = map[string]string{}
		}
		for k, v := range restore.Spec.NodeSelector {
			pod.Spec.NodeSelector[k] = v
		}
	}
	// The restored workload is not managed by the kubelet; never let the
	// kubelet restart the keeper out from under it.
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever

	return pod, nil
}
