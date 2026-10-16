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

// testCiliumCIDR is a pod network the manifest must NOT contain: Cilium reads
// it from the node's podCIDR annotation, which kubeadm writes from cni.cidr.
const testCiliumCIDR = "10.88.0.0/16"

// ciliumConfig is the least the cilium manifest reads: the registry is empty
// (ImageFor is the identity), and the pod network is carried only so a test can
// assert it does not appear in the rendered file.
func ciliumConfig() *vatescfg.Config {
	cfg := &vatescfg.Config{}
	cfg.CNI.Plugin = vatescfg.CNICilium
	cfg.CNI.CIDR = testCiliumCIDR
	return cfg
}

func ciliumManifest(t *testing.T) []byte {
	t.Helper()
	m, err := (&cilium{}).Manifest(ciliumConfig())
	if err != nil {
		t.Fatalf("cilium.Manifest() failed: %v", err)
	}
	return m
}

// Cilium, the other side of the cni.cidr contract (see flannel_test.go): the
// pod network is not in the manifest at all -- the agent reads it from the
// node's podCIDR annotation -- and the images are the ones the chart pins.
func TestCiliumManifestRendersPinnedAndAdapted(t *testing.T) {
	m := ciliumManifest(t)
	for _, want := range []string{
		// The pin, tag and digest, as the chart carries it.
		`image: "quay.io/cilium/cilium:v1.20.1@sha256:ae9ea21f7427fe24bc6ea7247eb552157a1b0a431744045d3f641545ca71d11b"`,
		`image: "quay.io/cilium/operator-generic:v1.20.1@sha256:6c3885fc7b629099fdbe2a5c87869c86feb825fa18fae299eac0f61918d16ecf"`,
		// The adaptations stated in the template's header.
		"ipam: \"kubernetes\"",
		"kube-proxy-replacement: \"false\"",
		"enable-tcx: \"false\"",
		"cgroup-root: \"/sys/fs/cgroup\"",
		// The state goes under /run, the PID-1 tmpfs, not under /var.
		"path: /run/cilium",
		"path: /run/netns",
		// A single bootstrap node cannot run the chart's two-operator
		// anti-affinity.
		"replicas: 1",
		// The node's own CNI configuration: the agent writes 05-cilium.conflist
		// (which sorts ahead of the image's 10-flannel.conf) and the
		// cni-exclusive flag whiteouts the flannel conf on cilium nodes.
		"write-cni-conf-when-ready: /host/etc/cni/net.d/05-cilium.conflist",
		"cni-exclusive: \"true\"",
		// The podCIDR annotation is the pod network; it is written by kubeadm
		// from cni.cidr and may lag a node that joins mid-rename, so the agent
		// must not require it to be present at startup.
		"k8s-require-ipv4-pod-cidr: \"false\"",
		// The agent runs on the host network: no CNI chicken-and-egg for the
		// DaemonSet itself.
		"hostNetwork: true",
		// No mesh, no observability, no external sidecar.
		"enable-hubble: \"false\"",
		"external-envoy-proxy: \"false\"",
		// The ports that say the agent and operator are actually serving.
		"hostPort: 9879",
		"hostPort: 9234",
		"hostPort: 9963",
	} {
		if !strings.Contains(string(m), want) {
			t.Errorf("the cilium manifest does not carry\n  %s", want)
		}
	}
	// The pod network is NOT in this manifest: Cilium reads it from the
	// node's podCIDR annotation, which kubeadm writes from cni.cidr. If the
	// CIDR ever appears in the rendered file, a second copy of the pod network
	// has been created and the two can drift.
	if strings.Contains(string(m), testCiliumCIDR) {
		t.Errorf("the cilium manifest carries the pod CIDR %q; it must come from the node's podCIDR annotation, not from the manifest", testCiliumCIDR)
	}
	if strings.Contains(string(m), "path: /var/run") {
		t.Error("the cilium manifest still names a /var/run hostPath; the hostPath the node creates and labels is the /run one, and the chart's /var/run paths are reached on the host through the standard /var/run -> /run link PID 1 recreates")
	}
}

// The chart 1.20 ships no crds/ directory: the operator creates the
// CustomResourceDefinitions (skipCRDCreation defaults to false) and the agent
// waits on them. Pin the arrangement, so a future template that either
// re-introduces CRD documents or strips the operator's create verb is caught
// here rather than on a node whose agent sits in CrashLoopBackOff.
func TestCiliumOperatorCreatesTheCRDs(t *testing.T) {
	m := ciliumManifest(t)
	if strings.Contains(string(m), "kind: CustomResourceDefinition") {
		t.Error("the cilium manifest contains CRD documents; chart 1.20 has no crds/ directory, the operator creates them")
	}
	if !strings.Contains(string(m), "customresourcedefinitions") || !strings.Contains(string(m), "create") {
		t.Error("the cilium manifest no longer grants the operator create on customresourcedefinitions; the agent would wait 5 minutes on CRDs that never appear")
	}
}

// The operator runs with one replica (the chart's required pod anti-affinity
// would leave a second one Pending on a single-node cluster), so its rolling
// update must make room BEFORE it creates: maxUnavailable 100%. The chart's
// 50% is 0 of one replica, which forces the rollout to schedule a surge pod
// first -- and the very anti-affinity that justifies the single replica would
// refuse to run two operators on the one node, so the update hangs. maxSurge
// stays 0 for the same reason: a surge pod on the only node cannot be
// scheduled either.
func TestCiliumOperatorUpdateSurvivesASingleNode(t *testing.T) {
	m := ciliumManifest(t)
	dec := yaml.NewDecoder(bytes.NewReader(m))
	found := false
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Replicas *int `yaml:"replicas"`
				Strategy struct {
					RollingUpdate struct {
						MaxSurge       string `yaml:"maxSurge"`
						MaxUnavailable string `yaml:"maxUnavailable"`
					} `yaml:"rollingUpdate"`
				} `yaml:"strategy"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("the cilium manifest is not valid YAML: %v", err)
		}
		if doc.Kind != "Deployment" || doc.Metadata.Name != "cilium-operator" {
			continue
		}
		found = true
		if doc.Spec.Replicas == nil || *doc.Spec.Replicas != 1 {
			t.Fatalf("the operator deployment runs %v replicas, want 1", *doc.Spec.Replicas)
		}
		ru := doc.Spec.Strategy.RollingUpdate
		if ru.MaxUnavailable != "100%" {
			t.Errorf("the operator rollingUpdate has maxUnavailable %q, want 100%%: 50%% of one replica is 0, and the rollout would need a surge pod the anti-affinity cannot schedule", ru.MaxUnavailable)
		}
		if ru.MaxSurge != "0" {
			t.Errorf("the operator rollingUpdate has maxSurge %q, want 0: a surge operator cannot be scheduled beside the running one on a single node", ru.MaxSurge)
		}
	}
	if !found {
		t.Fatal("the cilium manifest has no cilium-operator Deployment")
	}
}

// The rendered manifest must stay a document the API server can apply:
// parseable YAML, with exactly the objects the chart renders for this
// configuration. The count is the assertion: adding or dropping a resource
// (an RBAC, a Namespace, a second container) changes it.
func TestCiliumManifestIsApplicableYAML(t *testing.T) {
	m := ciliumManifest(t)
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
			t.Fatalf("the cilium manifest is not valid YAML: %v", err)
		}
		n++
		if doc.Kind != "" {
			kinds = append(kinds, doc.Kind)
		}
	}
	if n != 18 {
		t.Errorf("the cilium manifest rendered %d documents, want 18 (the chart's agent + operator + RBAC set)", n)
	}
	for _, want := range []string{
		"Namespace",
		"DaemonSet",  // the agent
		"Deployment", // the operator
		"ConfigMap",  // cilium-config
	} {
		if !slices.Contains(kinds, want) {
			t.Errorf("the cilium manifest has no %s document: %v", want, kinds)
		}
	}
}
