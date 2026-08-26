package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// CompatibilityHashLabel is set on Nodes by the agent alongside
// CompatibilityAnnotation. Node annotations cannot be selected on, and the
// GPU model contains spaces so it cannot be a label value — so the tuple is
// hashed, and a restore confines its placeholder pod to nodes carrying the
// build's hash.
const CompatibilityHashLabel = "podsnapshot.io/compat-hash"

// NodeCompatibility renders the node-side part of the tuple (everything that
// is a property of the machine rather than of the workload) as
// "gpu=<model>;driver=<ver>;criu=<ver>".
func (k CompatibilityKey) NodeCompatibility() string {
	return fmt.Sprintf("gpu=%s;driver=%s;criu=%s", k.GPUModel, k.DriverVersion, k.CRIUVersion)
}

// NodeHash is a short, label-safe digest of NodeCompatibility. Empty for a
// nil key, or one carrying no node-side information to match on.
func (k *CompatibilityKey) NodeHash() string {
	if k == nil || (k.GPUModel == "" && k.DriverVersion == "" && k.CRIUVersion == "") {
		return ""
	}
	sum := sha256.Sum256([]byte(k.NodeCompatibility()))
	return hex.EncodeToString(sum[:])[:16]
}

// ParseCompatibility reads the node annotation written by the agent. Unknown
// keys are ignored so the format can grow.
func ParseCompatibility(s string) CompatibilityKey {
	var k CompatibilityKey
	for _, field := range strings.Split(s, ";") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "gpu":
			k.GPUModel = value
		case "driver":
			k.DriverVersion = value
		case "criu":
			k.CRIUVersion = value
		}
	}
	return k
}
