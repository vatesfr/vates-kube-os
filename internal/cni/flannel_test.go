package cni

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/vatesfr/vates-kube-os/vatescfg"
)

// flannelConfig is the least the flannel manifest reads: the pod network, and
// no registry, so ImageFor is the identity. The rest of vates-node.yaml does
// not reach this template.
func flannelConfig(cidr string) *vatescfg.Config {
	cfg := &vatescfg.Config{}
	cfg.CNI.Plugin = vatescfg.CNIFlannel
	cfg.CNI.CIDR = cidr
	return cfg
}

// The two CNIs are the same field, cni.cidr, told to two different readers:
// flannel is given the range in its manifest, cilium reads it from the node's
// podCIDR annotation, which kubeadm writes from the same field (see
// cilium_test.go). This is what keeps the two interchangeable in
// vates-node.yaml, so both sides of the contract are asserted, each in its own
// manifest.
func TestFlannelManifestRendersPinnedAndAdapted(t *testing.T) {
	// A distinctive pod network, so that a template which stopped reading
	// cni.cidr and baked a default in would fail here rather than agree by
	// accident with the document.
	m, err := (&flannel{}).Manifest(flannelConfig("10.77.0.0/16"))
	if err != nil {
		t.Fatalf("flannel.Manifest() failed: %v", err)
	}
	for _, want := range []string{
		// The pod network, from vates-node.yaml, in the net-conf.json flanneld
		// reads off the ConfigMap.
		`"Network": "10.77.0.0/16"`,
		// The pins: the flannel release, and the CNI plugin image kept identical
		// to the one the OS image already stages into /opt/cni/bin.
		"image: docker.io/flannel/flannel:v0.26.1",
		"image: ghcr.io/flannel-io/flannel-cni-plugin:v1.9.1-flannel3",
		// Where the two init containers install: the plugin binary and the
		// configuration, on the overlays the node mounts for exactly this.
		"- /opt/cni/bin/flannel",
		"- /etc/cni/net.d/10-flannel.conflist",
		// flanneld's runtime state, on the /run tmpfs the node creates and
		// labels (see RunDirs).
		"path: /run/flannel",
		"mountPath: /run/flannel",
		// The DaemonSet must start on a node that is NotReady precisely because
		// it has no CNI yet, so it tolerates every taint.
		"operator: Exists",
	} {
		if !strings.Contains(string(m), want) {
			t.Errorf("the flannel manifest does not carry\n  %s", want)
		}
	}
	// The pin that matters most: upstream's own example ships an older CNI
	// plugin, and letting it through would have the init container overwrite the
	// binary the OS image already staged into /opt/cni/bin with a different
	// build. The template's header names the old version, so match the image
	// reference, not the bare tag.
	if strings.Contains(string(m), "flannel-cni-plugin:v1.5.1-flannel2") {
		t.Error("the flannel manifest installs upstream's older CNI plugin image; the DaemonSet would overwrite the /opt/cni/bin binary the image stages")
	}
}

// The rendered manifest must stay a document the API server can apply:
// parseable YAML, with exactly the objects the flannel release ships. The count
// is the assertion: adding or dropping a resource changes it.
func TestFlannelManifestIsApplicableYAML(t *testing.T) {
	m, err := (&flannel{}).Manifest(flannelConfig("10.244.0.0/16"))
	if err != nil {
		t.Fatalf("flannel.Manifest() failed: %v", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(m))
	var kinds []string
	n := 0
	for {
		var doc struct {
			Kind string `yaml:"kind"`
		}
		err := dec.Decode(&doc)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("the flannel manifest is not valid YAML: %v", err)
		}
		n++
		if doc.Kind != "" {
			kinds = append(kinds, doc.Kind)
		}
	}
	if n != 6 {
		t.Errorf("the flannel manifest rendered %d documents, want 6 (Namespace, ClusterRole, ClusterRoleBinding, ServiceAccount, ConfigMap, DaemonSet)", n)
	}
	for _, want := range []string{
		"Namespace",
		"ConfigMap", // kube-flannel-cfg, the pod network's own copy
		"DaemonSet", // the agent
	} {
		if !slices.Contains(kinds, want) {
			t.Errorf("the flannel manifest has no %s document: %v", want, kinds)
		}
	}
}
