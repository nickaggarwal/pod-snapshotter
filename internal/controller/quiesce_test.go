package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapv1 "pod-snapshotter/api/v1alpha1"
)

func podWith(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vllm", Annotations: annotations}}
}

func TestResolveQuiesce(t *testing.T) {
	t.Run("no annotation keeps the v1 live-dump path", func(t *testing.T) {
		q, err := resolveQuiesce(podWith(nil))
		if err != nil || q != nil {
			t.Fatalf("got (%+v, %v), want (nil, nil)", q, err)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		before := time.Now()
		q, err := resolveQuiesce(podWith(map[string]string{
			snapv1.QuiesceAnnotation: snapv1.QuiesceModePresenceFile,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if q.Dir != snapv1.DefaultQuiesceDir {
			t.Errorf("Dir = %q, want %q", q.Dir, snapv1.DefaultQuiesceDir)
		}
		if q.Deadline == nil {
			t.Fatal("no deadline set")
		}
		if got := q.Deadline.Sub(before); got < snapv1.DefaultQuiesceTimeout-time.Minute {
			t.Errorf("deadline is only %s out, want ~%s", got, snapv1.DefaultQuiesceTimeout)
		}
	})

	t.Run("explicit dir and timeout", func(t *testing.T) {
		before := time.Now()
		q, err := resolveQuiesce(podWith(map[string]string{
			snapv1.QuiesceAnnotation:        snapv1.QuiesceModePresenceFile,
			snapv1.QuiesceDirAnnotation:     "/var/run/snapshot/",
			snapv1.QuiesceTimeoutAnnotation: "90s",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if q.Dir != "/var/run/snapshot" {
			t.Errorf("Dir = %q, want the cleaned path", q.Dir)
		}
		if got := q.Deadline.Sub(before); got > 2*time.Minute {
			t.Errorf("deadline is %s out, want ~90s", got)
		}
	})

	t.Run("rejects unknown protocol", func(t *testing.T) {
		if _, err := resolveQuiesce(podWith(map[string]string{
			snapv1.QuiesceAnnotation: "grpc-hook",
		})); err == nil {
			t.Error("expected an unsupported protocol to be rejected up front")
		}
	})

	t.Run("rejects a relative dir", func(t *testing.T) {
		if _, err := resolveQuiesce(podWith(map[string]string{
			snapv1.QuiesceAnnotation:    snapv1.QuiesceModePresenceFile,
			snapv1.QuiesceDirAnnotation: "snapshot",
		})); err == nil {
			t.Error("expected a relative rendezvous dir to be rejected")
		}
	})

	t.Run("rejects a bad timeout", func(t *testing.T) {
		for _, bad := range []string{"soon", "-5s", "0"} {
			if _, err := resolveQuiesce(podWith(map[string]string{
				snapv1.QuiesceAnnotation:        snapv1.QuiesceModePresenceFile,
				snapv1.QuiesceTimeoutAnnotation: bad,
			})); err == nil {
				t.Errorf("expected timeout %q to be rejected", bad)
			}
		}
	})
}
