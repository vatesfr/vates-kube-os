package vatescfg

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/vatesfr/vates-kube-os/internal/k8sbin"
)

// Role is the node's part in the cluster. The OS supports both, and the
// provider picks one per machine.
type Role string

const (
	// RoleMaster runs the control plane: the kubelet plus etcd, kube-apiserver,
	// kube-controller-manager and kube-scheduler as static pods.
	RoleMaster Role = "master"
	// RoleWorker runs only the kubelet, which joins an existing control plane.
	RoleWorker Role = "worker"
)

// Node is the machine's identity.
//
// The name is normally left out of the document: a NoCloud config drive carries
// it in its meta-data `local-hostname`, and the node falls back to that when
// no name is set here. But an infrastructure provider that does not write a
// usable `local-hostname` -- Xen Orchestra writes `instance-id` only -- can
// state the name here instead, where the bootstrap provider, which knows the
// CAPI Machine's name, can put it. At least one of the two must be present.
type Node struct {
	// Name is the Kubernetes node name. Optional; when set it wins over the
	// config drive's meta-data `local-hostname`.
	Name string `yaml:"name,omitempty"`
}

// PKI is the certificate material the provider hands the node, when it hands
// any. It is absent on the direct-drive path, where the drive carries the
// material as files; on the CAPI path the document is the only channel, so the
// provider states the PEMs here and the node writes them where kubeadm and the
// management API expect them.
type PKI struct {
	// ClusterCA is the cluster's certificate authority. Its certificate is
	// enough for a joining machine -- a worker, or a control plane that fetches
	// the shared certificates with a certificate key; the key is only needed by
	// a node that must sign with it.
	ClusterCA CertificateAuthority `yaml:"clusterCA,omitempty"`
	// APICA is the operator's certificate authority for the management API. A
	// bootstrapping control plane mints its :50000 server certificate from it
	// and trusts the clients it signs, which is how a provider authenticates
	// without holding the cluster CA.
	APICA CertificateAuthority `yaml:"apiCA,omitempty"`
}

// CertificateAuthority is a PEM certificate, and optionally its private key.
type CertificateAuthority struct {
	// Cert is the PEM certificate. Required when the block is present.
	Cert string `yaml:"cert,omitempty"`
	// Key is the PEM private key, present only when the holder must sign.
	Key string `yaml:"key,omitempty"`
}

// Config is the whole of vates-node.yaml.
//
// Every field is required unless its doc comment says otherwise. Unknown keys
// are rejected rather than ignored: a typo in a field name must not silently
// leave a node half-configured, which is precisely the failure class this
// project keeps running into.
type Config struct {
	Role       Role       `yaml:"role"`
	Node       Node       `yaml:"node,omitempty"`
	PKI        PKI        `yaml:"pki,omitempty"`
	Kubernetes Kubernetes `yaml:"kubernetes"`
	Cluster    Cluster    `yaml:"cluster"`
	Network    Network    `yaml:"network"`
	CNI        CNI        `yaml:"cni"`
	Registry   Registry   `yaml:"registry,omitempty"`
	Binaries   Binaries   `yaml:"binaries,omitempty"`
	Dashboard  Dashboard  `yaml:"dashboard,omitempty"`
	API        API        `yaml:"api,omitempty"`
	Cloud      Cloud      `yaml:"cloud,omitempty"`
	Time       Time       `yaml:"time,omitempty"`
}

// Time is how the node keeps its clock. There is deliberately no timezone: the
// system runs in UTC, the way Kubernetes and etcd do internally, so a node needs
// a source of time and not a local zone. That also keeps the read-only root
// honest -- there is no /etc/localtime to write, and no tz database in the image.
type Time struct {
	// Servers are the NTP servers or pools to synchronize from, tried in order.
	// Optional; when the document lists none the node falls back to
	// DefaultNTPServers, so a machine with no explicit time configuration still
	// has a source. A server that cannot be reached is not fatal: the boot-time
	// sync is bounded, and the node then runs on the hypervisor's clock.
	Servers []string `yaml:"servers,omitempty"`
}

// DefaultNTPServers is the fallback source when time.servers names none. A
// public pool is the least surprising default; a cluster with no route to it
// names its own servers, and a node that can reach neither still boots.
var DefaultNTPServers = []string{"pool.ntp.org"}

// NTPServers returns the servers to synchronize from, with the fallback applied.
func (c *Config) NTPServers() []string {
	if len(c.Time.Servers) == 0 {
		return DefaultNTPServers
	}
	return c.Time.Servers
}

// Cloud is the node's relationship with an external cloud provider, and the
// one place that keeps the hypervisor out of the image.
//
// The OS image knows nothing of any hypervisor: no XCP-ng, no Xen Orchestra, no
// providerID format. The day a node runs elsewhere, what changes is the value
// here, not the image. That is the whole point of the field.
//
// When a cloud controller manager (CCM) is to own the node's lifecycle -- its
// providerID, its addresses, its labels, removing it when the VM disappears --
// the kubelet has to stand down and let it: `--cloud-provider=external`. Since
// Kubernetes 1.31 the in-tree providers are gone, so "external" is the only
// value the kubelet takes besides nothing at all.
//
// A single switch turns the flag on, and an optional list of manifests rides
// with it. The manifests exist for the flows with no cluster-side applier -- a
// hand-booted node, another provider -- where a manifest with no credentials has
// to come from somewhere.
// Under CAPI they are ABSENT: the provider pushes the CCM and CSI through a
// ClusterResourceSet, which is where their XO credentials live, and a node has
// no business carrying a cluster's secrets.
type Cloud struct {
	// Provider is the external provider the kubelet defers to. Optional; empty
	// means no CCM, and the kubelet manages the node's lifecycle itself. Only
	// "external" is accepted: the in-tree providers were removed in Kubernetes
	// 1.31, so any other name would select a provider that no longer exists.
	Provider string `yaml:"provider,omitempty"`
	// Manifests are applied by the bootstrap, once the kubelet is up, for the
	// addons that have nowhere else to live. Optional, and normally empty under
	// CAPI. Each entry is an http(s) URL. Setting it requires provider:
	// external: these are the external provider's manifests, and applying them
	// while the kubelet still owns the lifecycle would leave two things
	// deciding what runs.
	Manifests []string `yaml:"manifests,omitempty"`
}

// CloudProviderExternal is the only provider value the schema accepts. Since
// Kubernetes 1.31 the in-tree providers are gone; the kubelet takes "external"
// or nothing.
const CloudProviderExternal = "external"

// UsesExternalCloudProvider reports whether the kubelet must be started with
// --cloud-provider=external, deferring the node's lifecycle to a CCM.
func (c *Config) UsesExternalCloudProvider() bool {
	return c.Cloud.Provider == CloudProviderExternal
}

// API configures the node's management API, the SSH replacement on :50000.
type API struct {
	// Port is the TCP port the management API listens on. Optional; defaults to
	// DefaultAPIPort.
	//
	// 50000 is the immutable-OS convention. It is a setting rather than a
	// constant because the port is a deployment's choice,
	// and vateskctl is told the same value (see `vateskctl gen --api-port`).
	Port int `yaml:"port,omitempty"`
}

// DefaultAPIPort is the management API's port when the document does not say.
const DefaultAPIPort = 50000

// APIPort is the port to listen on, with the default applied.
func (c *Config) APIPort() int {
	if c.API.Port == 0 {
		return DefaultAPIPort
	}
	return c.API.Port
}

// Dashboard is what the machine shows on its console.
//
// There is no "nothing" value, and that is deliberate. A machine that shows no
// dashboard but leaves a login prompt is a machine with a local door: it
// advertises the operating system and its version, and it is a door that the
// nodes this image builds cannot even open, since no account carries a
// password. It would buy no rescue and cost a surface. There is no local
// session at all: tui or gui, and nothing else. Both are read-only displays.
//
// The console is the only window onto a node that has no SSH, so what it shows
// is a property of the machine and belongs in the machine's file -- but it is
// also the one setting whose cost is not the same for every value: "gui" needs a
// screen and the drawing libraries, where "tui" needs nothing at all.
type Dashboard struct {
	// Mode is "tui" or "gui". Optional; defaults to "gui".
	//
	//   gui   the graphical dashboard, full screen, drawn straight onto the
	//         screen with cairo. The default: it is what a machine with a screen
	//         should show, and it falls back to the text one when there is no
	//         DRM device or the renderer cannot take the screen.
	//   tui   the text dashboard: no dependency, works on any console, and the
	//         only one that can work on a serial console where a graphical one
	//         cannot run.
	Mode DashboardMode `yaml:"mode,omitempty"`
}

// DashboardMode is how the console is drawn.
type DashboardMode string

const (
	// DashboardGUI is the graphical dashboard, and the default.
	DashboardGUI DashboardMode = "gui"
	// DashboardTUI is the text dashboard: the fallback, and the one for a
	// console with no DRM device.
	DashboardTUI DashboardMode = "tui"
)

// DashboardModeValue is the mode to use, with the default applied.
func (c *Config) DashboardModeValue() DashboardMode {
	if c.Dashboard.Mode == "" {
		return DashboardGUI
	}
	return c.Dashboard.Mode
}

// Registry says where this node's container images come from.
//
// It exists so that a cluster without internet access can be built from a
// mirror, and it is deliberately two things rather than one, because the images
// fall into two groups with different mechanisms:
//
//   - Kubernetes' own images are selected by kubeadm through imageRepository, a
//     single base under which kube-apiserver, etcd and CoreDNS are found. That
//     is kubeadm's own mechanism, not ours, and it is what this uses.
//   - Everything else (flannel, cilium, kube-vip, the pause image in
//     containerd's configuration) is referenced by us, host and path included,
//     and is rewritten by the mirrors below.
//
// Both empty means the public registries, which is today's behaviour.
type Registry struct {
	// Kubernetes is the base for the cluster's own images:
	// {Kubernetes}/kube-apiserver, {Kubernetes}/etcd, {Kubernetes}/coredns.
	// Optional; empty means registry.k8s.io, kubeadm's default.
	Kubernetes string `yaml:"kubernetes,omitempty"`
	// Mirrors rewrite the other images. The longest matching host wins, so a
	// rule for ghcr.io does not have to be repeated for ghcr.io/org.
	Mirrors []RegistryMirror `yaml:"mirrors,omitempty"`
}

// RegistryMirror redirects one upstream host to a mirror.
type RegistryMirror struct {
	// Host is the upstream registry, e.g. "ghcr.io" or "docker.io".
	Host string `yaml:"host,omitempty"`
	// Replace is where that host's images are found instead, without a scheme:
	// "harbor.example.com/mirror/ghcr.io". The path after the host is kept, so
	// ghcr.io/kube-vip/kube-vip:v1.0.0 becomes
	// harbor.example.com/mirror/ghcr.io/kube-vip/kube-vip:v1.0.0 -- the layout
	// Harbour's proxy-cache projects use, and one that is also trivial to
	// produce with a plain reverse proxy.
	Replace string `yaml:"replace,omitempty"`
}

// Binaries says where the Kubernetes binaries come from.
//
// They are fetched at first boot rather than baked into the image, which is what
// lets ONE image serve any supported Kubernetes version. The layout is the one
// Kubernetes publishes, so the public value needs no explanation and an internal
// mirror only has to reproduce a directory tree.
type Binaries struct {
	// Base is the prefix the layout hangs under. Optional; empty means the
	// public https://dl.k8s.io/release.
	Base string `yaml:"base,omitempty"`
}

// Kubernetes carries the container image versions to run.
//
// The OS image itself is version-agnostic: this is what selects the kubelet and
// control plane images, so the same OS image serves any supported Kubernetes
// release. It is set by the provider from the Cluster object's
// spec.topology.version, which is why it lives here and not at build time.
type Kubernetes struct {
	// Version is the Kubernetes release, e.g. "v1.31.0". Required.
	//
	// The leading "v" and full three-component form are enforced because this
	// string is used verbatim as a container image tag; a wrong shape produces
	// an image that does not exist, and the failure surfaces as a pull error
	// far from its cause.
	Version string `yaml:"version"`
}

// Cluster identifies the control plane this node talks to.
type Cluster struct {
	// ControlPlaneEndpoint is the API server address as "host:port". Required.
	ControlPlaneEndpoint string `yaml:"controlPlaneEndpoint"`
	// Token is the kubeadm bootstrap token, "abcdef.0123456789abcdef". Required
	// for workers, which use it to join. Optional on masters, which create the
	// cluster instead of joining it.
	Token string `yaml:"token,omitempty"`
	// CertificateKey is kubeadm's --certificate-key: the secret a joining
	// control plane uses to fetch the cluster's certificates. Optional, and only
	// meaningful on a RoleMaster that is joining rather than bootstrapping.
	//
	// It expires -- kubeadm gives it two hours -- so it is a value the provider
	// must generate close to the machine's creation, not one to store.
	CertificateKey string `yaml:"certificateKey,omitempty"`
	// CACertHash is the sha256 of the cluster's CA certificate, which the
	// joining node uses to verify it is talking to the right cluster. Required
	// for a master that has a certificateKey, because without it the node
	// accepts whatever answers on the control plane endpoint.
	CACertHash string `yaml:"caCertHash,omitempty"`
	// VIP makes this control plane node participate in owning a virtual IP, so
	// that the cluster's endpoint is reachable whichever control plane node is
	// up. Optional: a cluster behind an external load balancer leaves it unset.
	VIP VIP `yaml:"vip,omitempty"`
	// ServiceCIDR is the range Service addresses are allocated from. Optional;
	// defaults to 10.96.0.0/12, kubeadm's own default.
	//
	// It is here because the KUBELET has to be told the cluster's DNS service
	// address, and that address is derived from this range. The kubelet does not
	// read it from the cluster -- kubeadm writes it into the kubelet's own
	// configuration during `kubeadm init`, and this system installs that
	// configuration itself. Leaving it out is not a small omission: every pod
	// falls back to the node's resolver, no Service name resolves, and the pods
	// still show 1/1 Running. Measured: a pod on a worker logged
	//   kubelet does not have ClusterDNS IP configured and cannot create Pod
	//   using "ClusterFirst" policy. Falling back to "Default" policy.
	// and `curl` inside it reported "Could not resolve host".
	ServiceCIDR string `yaml:"serviceCIDR,omitempty"`
	// DNSDomain is the cluster's DNS domain. Optional; defaults to
	// "cluster.local", kubeadm's default.
	DNSDomain string `yaml:"dnsDomain,omitempty"`
	// Proxy controls kube-proxy, which kubeadm installs with its addons.
	Proxy Proxy `yaml:"proxy,omitempty"`
}

// Proxy selects whether kube-proxy runs.
type Proxy struct {
	// Disabled omits kube-proxy, for a CNI that replaces it in eBPF (Cilium's
	// kubeProxyReplacement). It is only meaningful with cni.plugin: none, because
	// flannel does NOT replace kube-proxy: without it, Services have no
	// implementation, and the schema refuses the pair rather than leave a cluster
	// whose Services silently do nothing. Optional; false keeps kube-proxy.
	Disabled bool `yaml:"disabled,omitempty"`
}

// VIP is the floating control plane address, as provided by kube-vip.
//
// kube-vip is used rather than reimplemented. Claiming an address means ARP
// announcements, detecting the current owner, and leader election against the
// API server; kube-vip is a static pod that already does exactly this, which
// makes it one more entry in the same manifests directory as etcd and
// kube-apiserver rather than a second mechanism to maintain.
type VIP struct {
	// Address is the virtual address to own, e.g. "192.168.1.10". Optional.
	//
	// It is kept separate from ControlPlaneEndpoint rather than derived from it
	// because the two legitimately differ: a cluster fronted by an external load
	// balancer has an endpoint that is not an address kube-vip owns, and
	// conflating them would make that configuration impossible to express.
	Address string `yaml:"address,omitempty"`
	// Interface is the interface to announce on. Optional; defaults to
	// Network.Iface, which is almost always right because the VIP has to be
	// reachable on the same link as the node.
	Interface string `yaml:"interface,omitempty"`
}

// Enabled reports whether this node should run kube-vip. An unset address means
// no VIP is managed here.
func (v VIP) Enabled() bool { return v.Address != "" }

// Network is the node's own addressing.
type Network struct {
	// Iface is the network interface to configure, e.g. "eth0". Required.
	Iface string `yaml:"iface"`
	// Mode is "dhcp" or "static". Required.
	Mode NetworkMode `yaml:"mode"`
	// CIDR is the node address in CIDR form. Required when Mode is static,
	// rejected otherwise so a leftover value cannot be mistaken for intent.
	CIDR string `yaml:"cidr,omitempty"`
	// Gateway is the default route. Required when Mode is static.
	Gateway string `yaml:"gateway,omitempty"`
	// DNS is an optional list of resolvers.
	DNS []string `yaml:"dns,omitempty"`
}

// NetworkMode is how the node obtains its address.
type NetworkMode string

const (
	NetworkDHCP   NetworkMode = "dhcp"
	NetworkStatic NetworkMode = "static"
)

// CNI selects the pod network.
type CNI struct {
	// Plugin is the CNI implementation. Optional; defaults to "flannel", so a
	// document that omits it behaves exactly as one that states it. "flannel"
	// and "cilium" are installed by the node; "none" means the cluster's CNI is
	// the operator's to install, and the node writes none. Anything else is
	// rejected rather than accepted and ignored.
	Plugin string `yaml:"plugin,omitempty"`
	// CIDR is the pod network, e.g. "10.244.0.0/16". Required.
	//
	// It is the pod network for WHICHEVER plugin is chosen: flannel is told the
	// range directly, and cilium reads it from the node's podCIDR annotation,
	// which kubeadm writes from the same value as the cluster's podSubnet. One
	// field, one pod network -- there is no second copy to keep in agreement.
	CIDR string `yaml:"cidr"`
}

// The cni.plugin values the schema accepts. "flannel" is the default and is
// installed by the node, as is "cilium"; "none" leaves the CNI to the operator,
// which is how a CNI that replaces kube-proxy is installed.
const (
	CNIFlannel = "flannel"
	CNICilium  = "cilium"
	CNINone    = "none"
)

var (
	versionRe  = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	tokenRe    = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
	caHashRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	nodeNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// VIPInterface is the interface kube-vip should announce on: the explicit
// vip.interface when given, otherwise the node's own interface. The VIP must be
// on the same link as the node's address, so defaulting to it is what makes the
// common case work without a second field to keep in agreement.
func (c *Config) VIPInterface() string {
	if c.Cluster.VIP.Interface != "" {
		return c.Cluster.VIP.Interface
	}
	return c.Network.Iface
}

// DefaultServiceCIDR and DefaultDNSDomain are kubeadm's own defaults.
//
// Reproduced rather than chosen: the kubelet is told a DNS address derived from
// the first, and if it disagreed with what kubeadm gave the API server, pods
// would be handed an address nothing answers on.
const (
	DefaultServiceCIDR = "10.96.0.0/12"
	DefaultDNSDomain   = "cluster.local"

	// DefaultImageRepository and DefaultBinaryBase are where Kubernetes
	// publishes its images and its binaries. The binary layout lives in
	// internal/k8sbin, shared with the launcher, so the schema and the program that
	// fetches cannot disagree about where a binary is.
	DefaultImageRepository = "registry.k8s.io"
	DefaultBinaryBase      = k8sbin.DefaultBase

	// dnsServiceIPOffset is where inside the service range kubeadm places the
	// cluster's DNS Service: the tenth address. Not the first, which is the
	// network address, and not the second, which is a gateway on most subnets.
	dnsServiceIPOffset = 10
)

// SupportedKubernetesMinorMin is the OLDEST Kubernetes minor this OS knows how
// to configure.
//
// A MINIMUM and no maximum, deliberately. What ties this project to a Kubernetes
// version is not the binary, which is fetched for whatever version is asked for,
// but the CONFIGURATION it writes: kubeadm's API is v1beta4 from Kubernetes 1.31,
// so a 1.30 node would be handed a document its kubeadm refuses, and the failure
// would come from kubeadm rather than from here.
//
// There is no upper bound because minor releases appear every ~4 months and a
// bound would mean rebuilding the OS image for each one -- the very thing this
// design exists to avoid. kubeadm keeps reading v1beta4 for many releases; if it
// ever stops, the incompatible version fails on the node with a kubeadm error
// naming the field, and THAT is the moment to raise the floor or add a ceiling.
// n.b. this is a floor to refuse the past, not a promise about the future.
const SupportedKubernetesMinorMin = 31

// KubernetesMinor extracts the minor number from a version like v1.31.4.
//
// The "v" is required, not tolerated: the schema's own validation requires it, so
// a version without one has been rejected before any caller gets here. Accepting
// it would make this function quietly more permissive than the format the rest of
// the system agrees on.
func KubernetesMinor(version string) (int, bool) {
	trimmed := strings.TrimPrefix(version, "v")
	if trimmed == version {
		return 0, false
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return minor, true
}

// ImageRepository is the base for the cluster's own images.
func (c *Config) ImageRepository() string {
	if c.Registry.Kubernetes != "" {
		return strings.TrimRight(c.Registry.Kubernetes, "/")
	}
	return DefaultImageRepository
}

// BinaryBase is the prefix the Kubernetes binaries hang under.
func (c *Config) BinaryBase() string {
	if c.Binaries.Base != "" {
		return strings.TrimRight(c.Binaries.Base, "/")
	}
	return DefaultBinaryBase
}

// BinaryURL is where a Kubernetes binary is fetched from.
//
// The rule itself lives in internal/k8sbin, shared with the launcher that performs
// the fetch: the schema validates a URL and the launcher builds one, and if they
// disagreed the failure would be a node that cannot start.
func (c *Config) BinaryURL(version, arch, name string) string {
	return k8sbin.URL(c.BinaryBase(), version, arch, name)
}

// ImageFor rewrites an image reference through the configured mirrors.
//
// The longest matching host wins, so a rule for ghcr.io covers
// ghcr.io/kube-vip/kube-vip without being repeated. An image whose host matches
// no rule is returned unchanged, which is what makes an unconfigured node behave
// exactly as it does today.
func (c *Config) ImageFor(ref string) string {
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || host == "" {
		return ref
	}

	best := ""
	var chosen RegistryMirror
	for _, m := range c.Registry.Mirrors {
		h := strings.Trim(m.Host, "/")
		if h == "" || h != host {
			continue
		}
		if len(h) > len(best) {
			best, chosen = h, m
		}
	}
	if best == "" {
		return ref
	}
	return strings.TrimRight(chosen.Replace, "/") + "/" + rest
}

// DNSServiceIP is the address of the cluster's DNS Service.
//
// Empty when ServiceCIDR is unusable, which Validate has already refused by the
// time a Config is in use.
func (c *Config) DNSServiceIP() string {
	_, network, err := net.ParseCIDR(c.Cluster.ServiceCIDR)
	if err != nil {
		return ""
	}
	ip := network.IP.To4()
	if ip == nil {
		return ""
	}
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, binary.BigEndian.Uint32(ip)+dnsServiceIPOffset)
	return out.String()
}

// EndpointHost is the control plane address without its port. It is what a
// worker connects to and, when a VIP is configured, the address that kube-vip
// owns.
func (c *Config) EndpointHost() string {
	host, _, _ := strings.Cut(c.Cluster.ControlPlaneEndpoint, ":")
	return host
}

// Load decodes and validates a vates-node.yaml document.
//
// Decoding is strict: an unknown key is an error. Doing this at load time, in
// one place, means every consumer gets a config that has already been proven
// coherent, instead of each of them re-checking a subset.
func Load(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parsing vates-node.yaml: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the document's internal coherence.
//
// It is a separate method rather than part of Load so the same rules can be
// applied to a Config built by any means.
func (c *Config) Validate() error {
	switch c.Role {
	case RoleMaster, RoleWorker:
	case "":
		return fmt.Errorf("role is required (master or worker)")
	default:
		return fmt.Errorf("role %q is not one of master, worker", c.Role)
	}

	if c.API.Port != 0 && (c.API.Port < 1 || c.API.Port > 65535) {
		return fmt.Errorf("api.port %d is not a TCP port (1-65535)", c.API.Port)
	}

	// The name here is the node identity the bootstrap provider states, and it
	// wins over the drive's meta-data. Refusing a bad one at the parse means the
	// failure names node.name, not a rejected node registration later.
	if c.Node.Name != "" {
		if net.ParseIP(c.Node.Name) != nil {
			return fmt.Errorf("node.name %q is an IP address; the node name must be stable while the address may change", c.Node.Name)
		}
		if !nodeNameRe.MatchString(c.Node.Name) {
			return fmt.Errorf("node.name %q is not usable as a Kubernetes node name (lowercase RFC 1123 subdomain)", c.Node.Name)
		}
	}

	// The PKI block is optional. A half-populated one is refused rather than
	// written to /etc/kubernetes: a key with no certificate, or a blob that is
	// not PEM, would surface on the node as an opaque x509 error.
	for _, ca := range []struct {
		name  string
		value CertificateAuthority
	}{
		{"pki.clusterCA", c.PKI.ClusterCA},
		{"pki.apiCA", c.PKI.APICA},
	} {
		if ca.value.Key != "" && ca.value.Cert == "" {
			return fmt.Errorf("%s.key is set without %s.cert", ca.name, ca.name)
		}
		if ca.value.Cert != "" && !strings.Contains(ca.value.Cert, "-----BEGIN CERTIFICATE-----") {
			return fmt.Errorf("%s.cert is not a PEM certificate", ca.name)
		}
		if ca.value.Key != "" && !strings.Contains(ca.value.Key, "PRIVATE KEY-----") {
			return fmt.Errorf("%s.key is not a PEM private key", ca.name)
		}
	}

	// The VIP is a control plane concern. Accepting it on a worker would let a
	// worker claim the cluster's endpoint for itself, which is never intended
	// and would be very hard to diagnose from the API server's point of view.
	if c.Cluster.VIP.Enabled() {
		if c.Role != RoleMaster {
			return fmt.Errorf("cluster.vip.address is set on a %s: only a master may own the virtual IP", c.Role)
		}
		if net.ParseIP(c.Cluster.VIP.Address) == nil {
			return fmt.Errorf("cluster.vip.address %q is not an IP address", c.Cluster.VIP.Address)
		}
	}

	if !versionRe.MatchString(c.Kubernetes.Version) {
		return fmt.Errorf("kubernetes.version %q must look like v1.31.0", c.Kubernetes.Version)
	}
	// Refused here, at the parse, rather than on the node: the templates this
	// system writes need kubeadm's v1beta4 configuration API, which starts at
	// v1.31, and a version below that would otherwise fail inside kubeadm, with a
	// message about a field rather than about a version nobody claimed to support.
	if minor, ok := KubernetesMinor(c.Kubernetes.Version); ok {
		if minor < SupportedKubernetesMinorMin {
			return fmt.Errorf("kubernetes.version %s is older than the oldest version this system "+
				"can configure, v1.%d.x (kubeadm's configuration API is v1beta4 from v1.%d, and "+
				"the documents written here are read by that API)",
				c.Kubernetes.Version, SupportedKubernetesMinorMin, SupportedKubernetesMinorMin)
		}
	}

	host, port, ok := strings.Cut(c.Cluster.ControlPlaneEndpoint, ":")
	if !ok || host == "" || port == "" {
		return fmt.Errorf("cluster.controlPlaneEndpoint %q must be host:port", c.Cluster.ControlPlaneEndpoint)
	}

	if c.Role == RoleWorker && c.Cluster.Token == "" {
		return fmt.Errorf("cluster.token is required for a worker, which uses it to join")
	}
	if c.Cluster.Token != "" && !tokenRe.MatchString(c.Cluster.Token) {
		return fmt.Errorf("cluster.token %q must look like abcdef.0123456789abcdef", c.Cluster.Token)
	}

	// A joining control plane verifies the cluster it is talking to with this
	// hash. Asking for its absence to be noticed later would mean accepting
	// whatever answers on the endpoint.
	if c.Cluster.CertificateKey != "" && c.Cluster.CACertHash == "" {
		return fmt.Errorf("cluster.caCertHash is required when cluster.certificateKey is set")
	}
	if c.Cluster.CACertHash != "" && !caHashRe.MatchString(c.Cluster.CACertHash) {
		return fmt.Errorf("cluster.caCertHash %q must be a sha256 in lowercase hex", c.Cluster.CACertHash)
	}

	// The service range and the DNS domain default to kubeadm's own values, and
	// are then validated like anything else: the kubelet is handed an address
	// derived from ServiceCIDR, so a value that does not parse would produce a
	// cluster whose pods are told to use an address nothing answers on.
	if c.Cluster.ServiceCIDR == "" {
		c.Cluster.ServiceCIDR = DefaultServiceCIDR
	}
	if _, _, err := net.ParseCIDR(c.Cluster.ServiceCIDR); err != nil {
		return fmt.Errorf("cluster.serviceCIDR %q is not a CIDR", c.Cluster.ServiceCIDR)
	}
	if c.Cluster.DNSDomain == "" {
		c.Cluster.DNSDomain = DefaultDNSDomain
	}

	if c.Network.Iface == "" {
		return fmt.Errorf("network.iface is required")
	}
	switch c.Network.Mode {
	case NetworkDHCP:
		// Address values in DHCP mode are unused. They are accepted rather than
		// refused because the provider legitimately writes them alongside
		// mode: dhcp, documenting the static case in the same document -- but
		// their being unused is announced through Warnings rather than being
		// silently dropped.
	case NetworkStatic:
		if c.Network.CIDR == "" {
			return fmt.Errorf("network.cidr is required when network.mode is static")
		}
		if c.Network.Gateway == "" {
			return fmt.Errorf("network.gateway is required when network.mode is static")
		}
	case "":
		return fmt.Errorf("network.mode is required (dhcp or static)")
	default:
		return fmt.Errorf("network.mode %q is not one of dhcp, static", c.Network.Mode)
	}

	// The pod network defaults to flannel: a document that omits cni.plugin is
	// the common case and must behave exactly as one that states it. The value
	// is defaulted here, in one place, so every check below -- and every
	// consumer of a loaded Config -- sees the effective value.
	if c.CNI.Plugin == "" {
		c.CNI.Plugin = CNIFlannel
	}
	switch c.CNI.Plugin {
	case CNIFlannel, CNICilium, CNINone:
	default:
		return fmt.Errorf("cni.plugin %q is not supported (flannel, cilium, or none to install the CNI from the cluster side)", c.CNI.Plugin)
	}
	if c.CNI.CIDR == "" {
		return fmt.Errorf("cni.cidr is required")
	}
	// kube-proxy is kubeadm's Service implementation. Neither flannel nor
	// cilium -- as installed here, the agent providing the pod network and
	// kube-proxy the Services, side by side -- replaces it, so disabling
	// kube-proxy there is a cluster whose Services silently do nothing.
	// Refused together rather than discovered later as a Service that never
	// connects.
	if c.Cluster.Proxy.Disabled && c.CNI.Plugin != CNINone {
		return fmt.Errorf("cluster.proxy.disabled requires cni.plugin: none (flannel and cilium, as installed here, do not replace kube-proxy)")
	}

	// A mirror rule is only useful if it can be applied. A half-written rule
	// would be accepted and then silently leave images pointing at the public
	// registry -- which is the failure this whole block exists to prevent, so it
	// is refused here rather than discovered as a slow pull from the internet on
	// a cluster that has none.
	for i, m := range c.Registry.Mirrors {
		if strings.Trim(m.Host, "/") == "" {
			return fmt.Errorf("registry.mirrors[%d].host is empty", i)
		}
		if strings.Trim(m.Replace, "/") == "" {
			return fmt.Errorf("registry.mirrors[%d].replace is empty", i)
		}
		if strings.Contains(m.Replace, "://") {
			return fmt.Errorf("registry.mirrors[%d].replace %q must not include a scheme", i, m.Replace)
		}
	}

	// One switch, not two. This check had been written twice, which is a way of
	// keeping a mistake in agreement with itself.
	switch c.Dashboard.Mode {
	case "", DashboardTUI, DashboardGUI:
	default:
		return fmt.Errorf("dashboard.mode %q is not one of tui, gui", c.Dashboard.Mode)
	}

	// The cloud provider is a two-value field on purpose. Kubernetes 1.31
	// removed the in-tree providers, so a name other than "external" would
	// select a provider the kubelet no longer has and fail far from here.
	switch c.Cloud.Provider {
	case "", CloudProviderExternal:
	default:
		return fmt.Errorf("cloud.provider %q is not supported (only %q; Kubernetes 1.31 "+
			"removed the in-tree cloud providers, so the kubelet takes \"external\" "+
			"or nothing at all)", c.Cloud.Provider, CloudProviderExternal)
	}
	// The manifests are the external provider's: applying them while the
	// kubelet still owns the lifecycle would leave two things deciding what
	// runs, and the failure would show up as a node that ignores its CCM.
	if len(c.Cloud.Manifests) > 0 && !c.UsesExternalCloudProvider() {
		return fmt.Errorf("cloud.manifests is set but cloud.provider is not %q: these are "+
			"the external provider's manifests, applied once the kubelet defers to it",
			CloudProviderExternal)
	}
	for i, m := range c.Cloud.Manifests {
		u, err := url.Parse(m)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("cloud.manifests[%d] %q must be an http or https URL", i, m)
		}
	}

	if c.Binaries.Base != "" {
		u, err := url.Parse(c.Binaries.Base)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("binaries.base %q must be an http or https URL", c.Binaries.Base)
		}
	}

	// A server name is written into chrony.conf, so anything that could start a
	// new directive -- whitespace, a newline -- is refused here rather than
	// pasted into a root daemon's configuration.
	for i, s := range c.Time.Servers {
		if strings.TrimSpace(s) == "" || strings.ContainsAny(s, " \t\r\n") {
			return fmt.Errorf("time.servers[%d] %q is not a host name or address", i, s)
		}
	}

	return nil
}

// Warnings returns non-fatal observations worth surfacing at boot, so that a
// defaulted value is announced rather than silently applied.
func (c *Config) Warnings() []string {
	var w []string
	if c.Role == RoleMaster && c.Cluster.Token == "" {
		w = append(w, "cluster.token is empty; this master will bootstrap a new cluster rather than join one")
	}
	// Unused values are announced rather than dropped in silence. The provider
	// writes these alongside mode: dhcp to document the static case.
	if c.Network.Mode == NetworkDHCP {
		if c.Network.CIDR != "" || c.Network.Gateway != "" {
			w = append(w, "network.cidr and network.gateway are set but network.mode is dhcp; they are unused")
		}
	}
	if c.Cluster.VIP.Enabled() && c.EndpointHost() != c.Cluster.VIP.Address {
		// Legitimate (an external load balancer in front of the VIP), but worth
		// stating so it is never a surprise.
		w = append(w, fmt.Sprintf("cluster.vip.address %s differs from the endpoint host %s",
			c.Cluster.VIP.Address, c.EndpointHost()))
	}
	return w
}
