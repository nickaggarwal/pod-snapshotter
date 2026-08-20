package v1alpha1

import "testing"

func TestParseCompatibilityRoundTrip(t *testing.T) {
	want := CompatibilityKey{
		GPUModel:      "NVIDIA A100 80GB PCIe",
		DriverVersion: "580.159.04",
		CRIUVersion:   "4.2.1",
	}
	got := ParseCompatibility(want.NodeCompatibility())
	if got != want {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}
	// ImageDigest is a property of the workload, not the node, so it must not
	// leak into the node-side tuple.
	withImage := want
	withImage.ImageDigest = "sha256:deadbeef"
	if withImage.NodeCompatibility() != want.NodeCompatibility() {
		t.Error("image digest must not appear in the node-side tuple")
	}
	if withImage.NodeHash() != want.NodeHash() {
		t.Error("image digest must not change the node hash")
	}
}

func TestNodeHash(t *testing.T) {
	a := CompatibilityKey{GPUModel: "A100", DriverVersion: "580", CRIUVersion: "4.2.1"}
	b := a
	if a.NodeHash() != b.NodeHash() {
		t.Error("identical tuples must hash the same")
	}
	b.DriverVersion = "570"
	if a.NodeHash() == b.NodeHash() {
		t.Error("a driver change must change the hash")
	}
	if len(a.NodeHash()) > 63 {
		t.Error("hash must be usable as a label value")
	}
	if (&CompatibilityKey{}).NodeHash() != "" {
		t.Error("an empty tuple constrains nothing and must not produce a hash")
	}
	var nilKey *CompatibilityKey
	if nilKey.NodeHash() != "" {
		t.Error("a nil key must hash to the empty string")
	}
}

func TestCompatibilityMatches(t *testing.T) {
	build := CompatibilityKey{GPUModel: "A100", DriverVersion: "580.159.04", CRIUVersion: "4.2.1"}

	if ok, why := build.Matches(build); !ok {
		t.Errorf("identical tuples should match, got %q", why)
	}

	node := build
	node.DriverVersion = "570.1"
	ok, why := build.Matches(node)
	if ok {
		t.Fatal("a driver mismatch must be rejected")
	}
	if why == "" {
		t.Error("a rejection must explain itself")
	}

	// A field the build never recorded constrains nothing.
	loose := CompatibilityKey{GPUModel: "A100"}
	if ok, _ := loose.Matches(node); !ok {
		t.Error("unrecorded build fields must not constrain placement")
	}
	// Nor does a node that has not reported one.
	if ok, _ := build.Matches(CompatibilityKey{}); !ok {
		t.Error("a node with no reported tuple must not be rejected outright")
	}
}
