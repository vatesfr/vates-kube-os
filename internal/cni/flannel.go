package cni

import "github.com/vatesfr/vates-kube-os/vatescfg"

// FlannelManifestPath is where the CNI manifest is written before being applied.
//
// Written to disk rather than piped into kubectl because the command runs inside
// a container: a file under /etc/kubernetes is already visible there through the
// existing mount, and needs no stdin plumbing.
const FlannelManifestPath = "/etc/kubernetes/cni/flannel.yaml"

// The flannel images, named here rather than inline in the template.
//
// Naming them is what gives the registry rewrite something to rewrite: a mirror
// in vates-node.yaml is expressed as "docker.io goes to harbor/mirror/docker.io",
// and a reference spelled out inside a template could not be matched against it.
const (
	FlannelImage          = "docker.io/flannel/flannel:v0.26.1"
	FlannelCNIPluginImage = "ghcr.io/flannel-io/flannel-cni-plugin:v1.9.1-flannel3"
)

// flannel is the default pod network, and the one the image is built around:
// its configuration is the lower layer of the /etc/cni/net.d overlay. Its agent
// runs alongside kube-proxy rather than replacing it.
type flannel struct{}

// flannel implements Plugin.
var _ Plugin = (*flannel)(nil)

func (*flannel) Name() string { return vatescfg.CNIFlannel }

func (*flannel) ManifestPath() string { return FlannelManifestPath }

// ReplacesKubeProxy: flanneld handles the pod network, kube-proxy handles
// Services. Neither can do the other's job, so both run.
func (*flannel) ReplacesKubeProxy() bool { return false }

// RunDirs: flanneld writes the pod network's subnet file into /run/flannel, and
// the CNI plugin on the host reads it back to configure every pod's sandbox.
func (*flannel) RunDirs() []string { return []string{"/run/flannel"} }

// Manifest renders the kube-flannel DaemonSet and its supporting objects.
//
// The manifest is the flannel v0.26.1 release, embedded in the binary rather
// than fetched when a node first comes up: it is an artifact of the OS image, and
// a control plane has to be able to start with no outbound access.
//
// Two things differ from upstream, both noted at the top of the template: the pod
// network comes from vates-node.yaml, and the CNI plugin image is pinned to the
// version this image already stages into /opt/cni/bin -- upstream ships an older
// one there, and letting the DaemonSet install it would silently replace the
// staged binary with a different version.
//
// The image references pass through the configured mirrors, so a cluster that
// pulls from a local registry pulls flannel from there too. Without a mirror
// configured this is the identity, and the manifest is byte-for-byte what it
// was before the feature existed.
func (*flannel) Manifest(cfg *vatescfg.Config) ([]byte, error) {
	return render("kube-flannel.yaml.tmpl", struct {
		PodCIDR        string
		FlannelImage   string
		CNIPluginImage string
	}{
		PodCIDR:        cfg.CNI.CIDR,
		FlannelImage:   cfg.ImageFor(FlannelImage),
		CNIPluginImage: cfg.ImageFor(FlannelCNIPluginImage),
	})
}
