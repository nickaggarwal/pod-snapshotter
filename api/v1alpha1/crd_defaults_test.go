package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// The defaults that actually govern a cluster live in the CRD, not in Go: the
// API server stamps them into every object that omits the field, and the Go
// zero value never appears. So the marker comment in the types file is a
// wish, and the generated YAML is the fact -- and the two drift the moment
// someone edits a marker without re-running `make manifests`.
//
// The specific drift this catches is the one that matters most here. The
// agent path is the default because it is 4.8x faster on the dump, but it
// requires a directory artifact: a cluster where checkpointer defaults to
// agent and artifactFormat still defaults to tar routes every snapshot into
// the fallback and quietly measures the old path.
func TestShippedCRDsCarryTheDefaultsTheTypesDeclare(t *testing.T) {
	for _, tc := range []struct {
		crd    string
		fields map[string]string
	}{
		{
			crd: "podsnapshot.io_podsnapshots.yaml",
			fields: map[string]string{
				"checkpointer":   CheckpointerAgent,
				"artifactFormat": "dir",
			},
		},
		{
			crd: "podsnapshot.io_snapshotbuilds.yaml",
			fields: map[string]string{
				"checkpointer":   CheckpointerAgent,
				"artifactFormat": "dir",
			},
		},
	} {
		t.Run(tc.crd, func(t *testing.T) {
			props := specProperties(t, tc.crd)
			for field, want := range tc.fields {
				p, ok := props[field].(map[string]any)
				if !ok {
					t.Fatalf("%s has no spec.%s", tc.crd, field)
				}
				got, _ := p["default"].(string)
				if got != want {
					t.Errorf("%s spec.%s default is %q, want %q (run `make manifests`)", tc.crd, field, got, want)
				}
			}
		})
	}
}

// specProperties digs the spec's property map out of a v1 CRD. Chart and
// config/crd/bases are a copy of each other (the Makefile cp's one to the
// other); the chart's is the one that reaches a cluster, so read that.
func specProperties(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "pod-snapshotter", "crds", name))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string `json:"name"`
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties map[string]any `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	for _, v := range crd.Spec.Versions {
		if v.Name == GroupVersion.Version {
			return v.Schema.OpenAPIV3Schema.Properties.Spec.Properties
		}
	}
	t.Fatalf("%s has no version %s", name, GroupVersion.Version)
	return nil
}
