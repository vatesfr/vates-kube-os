package cni

import "github.com/vatesfr/vates-kube-os/vatescfg"

// CiliumManifestPath is where the CNI manifest is written before being applied.
//
// Written to disk rather than piped into kubectl because the command runs inside
// a container: a file under /etc/kubernetes is already visible there through the
// existing mount, and needs no stdin plumbing. It sits beside the flannel one,
// and only one of the two is ever written, whichever cni.plugin selects.
const CiliumManifestPath = "/etc/kubernetes/cni/cilium.yaml"

// The Cilium images, named here rather than inline in the template.
//
// Naming them is what gives the registry rewrite something to rewrite: a mirror
// in vates-node.yaml is expressed as "quay.io goes to harbor/mirror/quay.io",
// and a reference spelled out inside a template could not be matched against it.
// The pin is the tag and the digest the chart itself pins (v1.20.1, printed by
// scripts/render-cilium.sh); both are carried so a mirror cannot silently serve
// a different build of the tag.
const (
	CiliumAgentImage    = "quay.io/cilium/cilium:v1.20.1@sha256:ae9ea21f7427fe24bc6ea7247eb552157a1b0a431744045d3f641545ca71d11b"
	CiliumOperatorImage = "quay.io/cilium/operator-generic:v1.20.1@sha256:6c3885fc7b629099fdbe2a5c87869c86feb825fa18fae299eac0f61918d16ecf"
)

// cilium is the pod network that brings its own policy engine, and the one that
// needs state of its own under /run. Here it runs alongside kube-proxy, not in
// place of it: kubeProxyReplacement is off.
type cilium struct{}

// cilium implements Plugin.
var _ Plugin = (*cilium)(nil)

func (*cilium) Name() string { return vatescfg.CNICilium }

func (*cilium) ManifestPath() string { return CiliumManifestPath }

// ReplacesKubeProxy: Cilium is rendered with kubeProxyReplacement off (see the
// template header), so kube-proxy stays and implements Services.
func (*cilium) ReplacesKubeProxy() bool { return false }

// RunDirs: the agent keeps its state under /run/cilium, and the pod network
// namespaces at /run/netns.
func (*cilium) RunDirs() []string { return []string{"/run/cilium", "/run/netns"} }

// Manifest renders the Cilium agent DaemonSet, the operator and their RBAC.
//
// The manifest is the Cilium v1.20.1 chart (scripts/render-cilium.sh), embedded
// in the binary rather than fetched when a node first comes up: it is an
// artifact of the OS image, and a control plane has to be able to start with no
// outbound access. It is rendered with the settings a node needs, stated at the
// top of the template: the pod network comes from ipam.mode kubernetes,
// kube-proxy is NOT replaced, and hubble, the external envoy and the gateway API
// are off.
//
// Nothing here is templated except the two images, because the pod network is
// not: Cilium reads it from the node's podCIDR annotation, which kubeadm writes
// from the same cni.cidr flannel is told. There is no second copy of the CIDR to
// keep in agreement, which is what makes the two CNIs interchangeable in the
// document.
//
// The image references pass through the configured mirrors, so a cluster that
// pulls from a local registry pulls Cilium from there too. Without a mirror
// configured this is the identity, and the manifest is the chart's output.
func (*cilium) Manifest(cfg *vatescfg.Config) ([]byte, error) {
	return render("cilium.yaml.tmpl", struct {
		AgentImage    string
		OperatorImage string
	}{
		AgentImage:    cfg.ImageFor(CiliumAgentImage),
		OperatorImage: cfg.ImageFor(CiliumOperatorImage),
	})
}
