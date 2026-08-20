package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// EmbeddedObjectMeta is the part of ObjectMeta that is meaningful on a pod
// template embedded in a CRD.
//
// corev1.PodTemplateSpec cannot be used directly: controller-gen renders its
// ObjectMeta as a bare `type: object` with no properties, and the API server
// then rejects every field inside it. That would make the quiesce
// annotations — which the whole §4 contract is carried by — impossible to
// set on a template.
type EmbeddedObjectMeta struct {
	// Labels applied to the created pod, on top of the ones pod-snapshotter
	// sets itself.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations applied to the created pod. This is where the quiesce
	// contract lives (podsnapshot.io/quiesce and friends).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PodTemplate is corev1.PodTemplateSpec with a schema-visible metadata block.
// The JSON shape is identical, so manifests written against a plain pod
// template keep working.
type PodTemplate struct {
	// +optional
	Metadata EmbeddedObjectMeta `json:"metadata,omitempty"`

	Spec corev1.PodSpec `json:"spec"`
}

// ToPodTemplateSpec converts to the core type.
func (t *PodTemplate) ToPodTemplateSpec() corev1.PodTemplateSpec {
	out := corev1.PodTemplateSpec{Spec: *t.Spec.DeepCopy()}
	if len(t.Metadata.Labels) > 0 {
		out.Labels = make(map[string]string, len(t.Metadata.Labels))
		for k, v := range t.Metadata.Labels {
			out.Labels[k] = v
		}
	}
	if len(t.Metadata.Annotations) > 0 {
		out.Annotations = make(map[string]string, len(t.Metadata.Annotations))
		for k, v := range t.Metadata.Annotations {
			out.Annotations[k] = v
		}
	}
	return out
}
