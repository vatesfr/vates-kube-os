package vatescfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseWorker is a minimal document that must always validate. Each negative
// case below mutates exactly one thing from it, so a failure can only come from
// that change.
const baseWorker = `
role: worker
kubernetes:
  version: v1.31.0
cluster:
  controlPlaneEndpoint: "192.168.1.10:6443"
  token: "abcdef.0123456789abcdef"
network:
  iface: eth0
  mode: dhcp
cni:
  plugin: flannel
  cidr: "10.244.0.0/16"
`

const baseMaster = `
role: master
kubernetes:
  version: v1.31.0
cluster:
  controlPlaneEndpoint: "192.168.1.10:6443"
network:
  iface: eth0
  mode: static
  cidr: "192.168.1.20/24"
  gateway: "192.168.1.1"
cni:
  plugin: flannel
  cidr: "10.244.0.0/16"
`

func TestLoadAcceptsValidDocuments(t *testing.T) {
	for name, doc := range map[string]string{"worker": baseWorker, "master": baseMaster} {
		t.Run(name, func(t *testing.T) {
			c, err := Load([]byte(doc))
			if err != nil {
				t.Fatalf("Load() rejected a valid %s document: %v", name, err)
			}
			if c.Role != Role(name) {
				t.Errorf("Role = %q, want %q", c.Role, name)
			}
		})
	}
}

func TestLoadAcceptsDocumentPKI(t *testing.T) {
	// The CAPI path states the PKI in the document, since the hypervisor gives
	// no other channel. The schema accepts it and the node writes it.
	doc := strings.Replace(baseWorker, "role: worker\n", `role: worker
pki:
  clusterCA:
    cert: |
      -----BEGIN CERTIFICATE-----
      MIIB
      -----END CERTIFICATE-----
  apiCA:
    cert: |
      -----BEGIN CERTIFICATE-----
      MIIB
      -----END CERTIFICATE-----
    key: |
      -----BEGIN PRIVATE KEY-----
      MIIB
      -----END PRIVATE KEY-----
`, 1)

	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load() rejected a document with a PKI block: %v", err)
	}
	if strings.TrimSpace(c.PKI.ClusterCA.Cert) == "" {
		t.Error("pki.clusterCA.cert was not decoded")
	}
	if c.PKI.APICA.Key == "" {
		t.Error("pki.apiCA.key was not decoded")
	}
}

func TestLoadAcceptsNoneCNIAndProxyDisabled(t *testing.T) {
	// The pair a CNI that replaces kube-proxy (Cilium) needs: no CNI installed by
	// the node, and no kube-proxy either.
	doc := strings.Replace(baseWorker, "plugin: flannel", "plugin: none", 1)
	doc = strings.Replace(doc, "  token:", "  proxy:\n    disabled: true\n  token:", 1)
	if _, err := Load([]byte(doc)); err != nil {
		t.Fatalf("Load() rejected cni.plugin: none with cluster.proxy.disabled: %v", err)
	}
}

func TestLoadDefaultsTheCNIToFlannel(t *testing.T) {
	// Flannel is the cluster's default pod network: a document that omits
	// cni.plugin behaves exactly like one that states it, which is what keeps
	// every existing vates-node.yaml working when the field became optional.
	doc := strings.Replace(baseWorker, "  plugin: flannel\n", "", 1)
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load() rejected a document without cni.plugin: %v", err)
	}
	if c.CNI.Plugin != CNIFlannel {
		t.Errorf("cni.plugin = %q, want the default %q", c.CNI.Plugin, CNIFlannel)
	}
}

func TestLoadAcceptsCilium(t *testing.T) {
	// Cilium is the second CNI the node installs itself; it runs WITH
	// kube-proxy, so no cluster.proxy.disabled is involved.
	doc := strings.Replace(baseWorker, "plugin: flannel", "plugin: cilium", 1)
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load() rejected cni.plugin: cilium: %v", err)
	}
	if c.CNI.Plugin != CNICilium {
		t.Errorf("cni.plugin = %q, want %q", c.CNI.Plugin, CNICilium)
	}
}

// The bootstrap path of the feature: a MASTER document with cilium is the one
// a provider writes for a cluster that wants the second CNI, and it must
// validate with no other change.
func TestLoadAcceptsMasterWithCilium(t *testing.T) {
	doc := strings.Replace(baseMaster, "plugin: flannel", "plugin: cilium", 1)
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load() rejected a master with cni.plugin: cilium: %v", err)
	}
	if c.CNI.Plugin != CNICilium {
		t.Errorf("cni.plugin = %q, want %q", c.CNI.Plugin, CNICilium)
	}
}

func TestLoadDefaultsTheCNIOnTheMaster(t *testing.T) {
	// The default is role-independent: the common case is a MASTER document
	// (a provider that creates a cluster) that omits cni.plugin, and it must
	// come back flannel exactly as the worker one does.
	doc := strings.Replace(baseMaster, "  plugin: flannel\n", "", 1)
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load() rejected a master document without cni.plugin: %v", err)
	}
	if c.CNI.Plugin != CNIFlannel {
		t.Errorf("cni.plugin = %q, want the default %q", c.CNI.Plugin, CNIFlannel)
	}
}

// The example document that ships in the image and that a provider copies as
// its starting point must keep validating: it is a second definition of the
// schema, and the two can drift (a key renamed here, not there) without any
// other test noticing.
func TestImageExampleDocumentStillValidates(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "image", "config", "vates-node.yaml"))
	if err != nil {
		t.Fatalf("reading the example document: %v", err)
	}
	c, err := Load(doc)
	if err != nil {
		t.Fatalf("Load() rejected image/config/vates-node.yaml: %v", err)
	}
	if c.CNI.Plugin != CNIFlannel {
		t.Errorf("the example document's cni.plugin = %q, want the default %q", c.CNI.Plugin, CNIFlannel)
	}
}

// A mirror rule rewrites cilium's images like every other: the host is
// replaced, the path and the pin (tag AND digest) survive, so a mirror cannot
// silently serve a different build of the tag.
func TestImageForRewritesCiliumHosts(t *testing.T) {
	doc := baseWorker + `
registry:
  mirrors:
    - host: quay.io
      replace: "harbor.vates.local/mirror/quay.io"
`
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("the document with a quay.io mirror does not validate: %v", err)
	}
	cases := map[string]string{
		// The agent and operator pins as firstboot carries them.
		"quay.io/cilium/cilium:v1.20.1@sha256:ae9ea21f7427fe24bc6ea7247eb552157a1b0a431744045d3f641545ca71d11b":           "harbor.vates.local/mirror/quay.io/cilium/cilium:v1.20.1@sha256:ae9ea21f7427fe24bc6ea7247eb552157a1b0a431744045d3f641545ca71d11b",
		"quay.io/cilium/operator-generic:v1.20.1@sha256:6c3885fc7b629099fdbe2a5c87869c86feb825fa18fae299eac0f61918d16ecf": "harbor.vates.local/mirror/quay.io/cilium/operator-generic:v1.20.1@sha256:6c3885fc7b629099fdbe2a5c87869c86feb825fa18fae299eac0f61918d16ecf",
		// A host that merely STARTS with the rule's host is a different
		// registry and must be left alone.
		"quay.io.evil.example/thing:v1": "quay.io.evil.example/thing:v1",
	}
	for in, want := range cases {
		if got := c.ImageFor(in); got != want {
			t.Errorf("ImageFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantSub string
	}{
		{
			// The whole point of strict decoding: a typo must not be ignored.
			name:    "unknown key",
			doc:     strings.Replace(baseWorker, "role: worker", "role: worker\nrolename: typo", 1),
			wantSub: "field rolename not found",
		},
		{
			// A nested typo too, since most of the document is nested. The
			// inserted line keeps network's indentation, otherwise the document
			// stops being valid YAML and the test would pass for the wrong
			// reason -- a parse error rather than the strict-decode error it is
			// meant to prove.
			name:    "unknown nested key",
			doc:     strings.Replace(baseWorker, "  iface: eth0", "  iface: eth0\n  ifacer: oops", 1),
			wantSub: "field ifacer not found",
		},
		{
			name:    "missing role",
			doc:     strings.Replace(baseWorker, "role: worker\n", "", 1),
			wantSub: "role is required",
		},
		{
			name:    "bad role",
			doc:     strings.Replace(baseWorker, "role: worker", "role: controlplane", 1),
			wantSub: "not one of master, worker",
		},
		{
			name:    "node name is an IP",
			doc:     strings.Replace(baseWorker, "role: worker\n", "role: worker\nnode:\n  name: 10.0.2.15\n", 1),
			wantSub: "node.name",
		},
		{
			name:    "node name is not RFC 1123",
			doc:     strings.Replace(baseWorker, "role: worker\n", "role: worker\nnode:\n  name: Bad_Name\n", 1),
			wantSub: "node.name",
		},
		{
			name:    "pki key without cert",
			doc:     strings.Replace(baseWorker, "role: worker\n", "role: worker\npki:\n  clusterCA:\n    key: |\n      -----BEGIN PRIVATE KEY-----\n", 1),
			wantSub: "pki.clusterCA.key is set without pki.clusterCA.cert",
		},
		{
			name:    "pki cert is not PEM",
			doc:     strings.Replace(baseWorker, "role: worker\n", "role: worker\npki:\n  clusterCA:\n    cert: not-a-cert\n", 1),
			wantSub: "pki.clusterCA.cert is not a PEM certificate",
		},
		{
			name:    "missing version",
			doc:     strings.Replace(baseWorker, "  version: v1.31.0\n", "", 1),
			wantSub: "kubernetes.version",
		},
		{
			// The version becomes an image tag, so its shape matters.
			name:    "version without v",
			doc:     strings.Replace(baseWorker, "version: v1.31.0", "version: 1.31.0", 1),
			wantSub: "must look like v1.31.0",
		},
		{
			name:    "version without patch",
			doc:     strings.Replace(baseWorker, "version: v1.31.0", "version: v1.31", 1),
			wantSub: "must look like v1.31.0",
		},
		{
			name:    "endpoint without port",
			doc:     strings.Replace(baseWorker, `"192.168.1.10:6443"`, `"192.168.1.10"`, 1),
			wantSub: "must be host:port",
		},
		{
			name:    "worker without token",
			doc:     strings.Replace(baseWorker, `  token: "abcdef.0123456789abcdef"`+"\n", "", 1),
			wantSub: "cluster.token is required for a worker",
		},
		{
			name:    "malformed token",
			doc:     strings.Replace(baseWorker, "abcdef.0123456789abcdef", "not-a-token", 1),
			wantSub: "must look like abcdef.0123456789abcdef",
		},
		{
			name:    "missing iface",
			doc:     strings.Replace(baseWorker, "  iface: eth0\n", "", 1),
			wantSub: "network.iface is required",
		},
		{
			name:    "missing mode",
			doc:     strings.Replace(baseWorker, "  mode: dhcp\n", "", 1),
			wantSub: "network.mode is required",
		},
		{
			name:    "bad mode",
			doc:     strings.Replace(baseWorker, "mode: dhcp", "mode: automatic", 1),
			wantSub: "not one of dhcp, static",
		},
		{
			name:    "static without cidr",
			doc:     strings.Replace(baseWorker, "mode: dhcp", "mode: static\n  gateway: \"192.168.1.1\"", 1),
			wantSub: "network.cidr is required",
		},
		{
			name:    "static without gateway",
			doc:     strings.Replace(baseWorker, "mode: dhcp", "mode: static\n  cidr: \"192.168.1.20/24\"", 1),
			wantSub: "network.gateway is required",
		},
		{
			// An unimplemented CNI must be rejected, not accepted and ignored.
			name:    "unsupported cni",
			doc:     strings.Replace(baseWorker, "plugin: flannel", "plugin: calico", 1),
			wantSub: "is not supported",
		},
		{
			// kube-proxy is the Service implementation flannel relies on;
			// disabling it there is a cluster whose Services do nothing.
			name:    "proxy disabled with flannel",
			doc:     strings.Replace(baseWorker, "  token:", "  proxy:\n    disabled: true\n  token:", 1),
			wantSub: "cluster.proxy.disabled requires cni.plugin: none",
		},
		{
			// Same for cilium: it is installed here as a pod network beside
			// kube-proxy, not as a kube-proxy replacement, so the pair is as
			// wrong as the flannel one.
			name: "proxy disabled with cilium",
			doc: strings.Replace(
				strings.Replace(baseWorker, "plugin: flannel", "plugin: cilium", 1),
				"  token:", "  proxy:\n    disabled: true\n  token:", 1),
			wantSub: "cluster.proxy.disabled requires cni.plugin: none",
		},
		{
			name:    "missing cni cidr",
			doc:     strings.Replace(baseWorker, `  cidr: "10.244.0.0/16"`+"\n", "", 1),
			wantSub: "cni.cidr is required",
		},
		{
			name:    "malformed yaml",
			doc:     "role: [worker",
			wantSub: "parsing vates-node.yaml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.doc))
			if err == nil {
				t.Fatalf("Load() accepted an invalid document; it should have failed with %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error was %q, want it to contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestMasterWithoutTokenIsAllowed(t *testing.T) {
	// A master bootstrapping a new cluster has no token to join with.
	if _, err := Load([]byte(baseMaster)); err != nil {
		t.Fatalf("Load() rejected a master without a token: %v", err)
	}
}

func TestUnusedAddressFieldsInDHCPAreAnnouncedNotDropped(t *testing.T) {
	// The CAPI provider writes cidr and gateway alongside mode: dhcp, to
	// document the static case in the same document. That is accepted, but the
	// values being unused must be stated: silently ignoring them is the failure
	// mode this project keeps hitting.
	doc := strings.Replace(baseWorker, "  mode: dhcp",
		"  mode: dhcp\n  cidr: \"192.168.1.20/24\"\n  gateway: \"192.168.1.1\"", 1)

	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load() rejected the provider's real document shape: %v", err)
	}

	var warned bool
	for _, w := range c.Warnings() {
		if strings.Contains(w, "network.mode is dhcp") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("Warnings() = %v, want one saying cidr/gateway are unused in dhcp mode", c.Warnings())
	}
}

func TestVIPIsMasterOnly(t *testing.T) {
	// A worker claiming the cluster's endpoint would be very hard to diagnose
	// from the API server's side.
	doc := strings.Replace(baseWorker, "cluster:\n", "cluster:\n  vip:\n    address: \"192.168.1.10\"\n", 1)

	_, err := Load([]byte(doc))
	if err == nil {
		t.Fatal("Load() accepted a VIP on a worker")
	}
	if !strings.Contains(err.Error(), "only a master may own the virtual IP") {
		t.Errorf("error was %q, want it to explain the VIP is control-plane only", err)
	}
}

func TestVIPAddressMustBeAnIP(t *testing.T) {
	doc := strings.Replace(baseMaster, "cluster:\n", "cluster:\n  vip:\n    address: \"not-an-ip\"\n", 1)

	_, err := Load([]byte(doc))
	if err == nil {
		t.Fatal("Load() accepted a VIP that is not an address")
	}
	if !strings.Contains(err.Error(), "cluster.vip.address") {
		t.Errorf("error was %q, want it to name cluster.vip.address", err)
	}
}

func TestVIPInterfaceDefaultsToTheNodeInterface(t *testing.T) {
	doc := strings.Replace(baseMaster, "cluster:\n", "cluster:\n  vip:\n    address: \"192.168.1.10\"\n", 1)

	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if got := c.VIPInterface(); got != "eth0" {
		t.Errorf("VIPInterface() = %q, want it to default to network.iface (eth0)", got)
	}
	// The VIP equals the endpoint host in the common case, so there must be no
	// warning about them differing.
	for _, w := range c.Warnings() {
		if strings.Contains(w, "differs from the endpoint host") {
			t.Errorf("Warnings() = %v, want no mismatch warning when they match", c.Warnings())
		}
	}
}

func TestServiceCIDRAndDNSDomainDefaultToKubeadms(t *testing.T) {
	// These two are defaulted rather than required, and the kubelet is told the
	// DNS address derived from them. A missing default would not fail anything
	// loudly: it would produce a cluster whose pods are handed a resolver
	// nothing answers on, while every pod shows 1/1 Running.
	c, err := Load([]byte(baseWorker))
	if err != nil {
		t.Fatalf("the base worker document no longer validates: %v", err)
	}
	if c.Cluster.ServiceCIDR != DefaultServiceCIDR {
		t.Errorf("serviceCIDR = %q, want the default %q", c.Cluster.ServiceCIDR, DefaultServiceCIDR)
	}
	if c.Cluster.DNSDomain != DefaultDNSDomain {
		t.Errorf("dnsDomain = %q, want the default %q", c.Cluster.DNSDomain, DefaultDNSDomain)
	}
}

func TestDNSServiceIPIsTheTenthAddressOfTheServiceCIDR(t *testing.T) {
	// kubeadm places the cluster's DNS Service at the tenth address of the
	// service range, and the kubelet must be told the same one: the API server
	// and the kubelet derive it independently here, so the rule is asserted
	// rather than assumed.
	cases := map[string]string{
		"10.96.0.0/12":   "10.96.0.10",
		"172.20.0.0/16":  "172.20.0.10",
		"10.0.0.0/8":     "10.0.0.10",
		"192.168.0.0/16": "192.168.0.10",
	}
	for cidr, want := range cases {
		c := &Config{}
		c.Cluster.ServiceCIDR = cidr
		if got := c.DNSServiceIP(); got != want {
			t.Errorf("DNSServiceIP(%s) = %q, want %q", cidr, got, want)
		}
	}
}

func TestServiceCIDRIsValidated(t *testing.T) {
	// A range that does not parse would leave the kubelet with an empty DNS
	// address, which is the same silence this whole feature exists to remove.
	doc := strings.Replace(baseWorker, "  controlPlaneEndpoint: \"192.168.1.10:6443\"",
		"  controlPlaneEndpoint: \"192.168.1.10:6443\"\n  serviceCIDR: \"not-a-cidr\"", 1)
	if _, err := Load([]byte(doc)); err == nil {
		t.Error("a malformed cluster.serviceCIDR was accepted")
	}
}

func TestRegistryAndBinariesDefaultToThePublicSources(t *testing.T) {
	// Unconfigured must mean "exactly today's behaviour": a cluster with
	// internet access is the common case, and a default that changed it would
	// make the feature a breaking change.
	c, err := Load([]byte(baseWorker))
	if err != nil {
		t.Fatalf("the base worker document no longer validates: %v", err)
	}
	if got := c.ImageRepository(); got != DefaultImageRepository {
		t.Errorf("ImageRepository() = %q, want %q", got, DefaultImageRepository)
	}
	if got := c.BinaryBase(); got != DefaultBinaryBase {
		t.Errorf("BinaryBase() = %q, want %q", got, DefaultBinaryBase)
	}
	// And an image whose host matches no rule is left exactly as it was.
	ref := "docker.io/library/nginx:1.27"
	if got := c.ImageFor(ref); got != ref {
		t.Errorf("ImageFor(unchanged) = %q, want %q", got, ref)
	}
}

func TestImageForRewritesTheMatchingHostOnly(t *testing.T) {
	doc := baseWorker + `
registry:
  kubernetes: "harbor.vates.local/k8s/"
  mirrors:
    - host: ghcr.io
      replace: "harbor.vates.local/mirror/ghcr.io"
    - host: docker.io
      replace: "harbor.vates.local/mirror/docker.io"
`
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("the document with a registry block does not validate: %v", err)
	}

	cases := map[string]string{
		"ghcr.io/kube-vip/kube-vip:v1.0.0":  "harbor.vates.local/mirror/ghcr.io/kube-vip/kube-vip:v1.0.0",
		"docker.io/flannel/flannel:v0.26.1": "harbor.vates.local/mirror/docker.io/flannel/flannel:v0.26.1",
		"registry.k8s.io/pause:3.10.1":      "registry.k8s.io/pause:3.10.1",
		// A host that merely STARTS with a rule's host is a different registry.
		"ghcr.io.evil.example/thing:v1": "ghcr.io.evil.example/thing:v1",
	}
	for in, want := range cases {
		if got := c.ImageFor(in); got != want {
			t.Errorf("ImageFor(%q) = %q, want %q", in, got, want)
		}
	}

	// The trailing slash in the configuration must not produce a double slash.
	if got := c.ImageRepository(); got != "harbor.vates.local/k8s" {
		t.Errorf("ImageRepository() = %q, want it without a trailing slash", got)
	}
}

func TestBinaryURLIsTheLayoutKubernetesPublishes(t *testing.T) {
	// One rule, used by the launcher: {base}/{version}/bin/linux/{arch}/{name},
	// and the checksum beside it as {name}.sha256. A mirror only has to
	// reproduce this tree.
	c := &Config{}
	if got := c.BinaryURL("v1.31.4", "amd64", "kubelet"); got != "https://dl.k8s.io/release/v1.31.4/bin/linux/amd64/kubelet" {
		t.Errorf("BinaryURL() = %q", got)
	}

	c.Binaries.Base = "https://mirror.vates.local/kubernetes/release/"
	if got := c.BinaryURL("v1.31.4", "arm64", "kubeadm"); got != "https://mirror.vates.local/kubernetes/release/v1.31.4/bin/linux/arm64/kubeadm" {
		t.Errorf("BinaryURL() with a base = %q", got)
	}
}

func TestRegistryAndBinariesAreValidated(t *testing.T) {
	// A half-written mirror rule is accepted by kubeadm and silently ignored,
	// which on a cluster with no internet means a pull that hangs rather than a
	// message that says what is wrong.
	cases := map[string]string{
		"mirror without a host": `
registry:
  mirrors:
    - host: ""
      replace: "harbor/x"
`,
		"mirror without a replacement": `
registry:
  mirrors:
    - host: ghcr.io
      replace: ""
`,
		"replacement carries a scheme": `
registry:
  mirrors:
    - host: ghcr.io
      replace: "https://harbor/x"
`,
		"binary base is not a URL": `
binaries:
  base: "mirror.vates.local/kubernetes"
`,
		"binary base has no scheme": `
binaries:
  base: "ftp://mirror.vates.local"
`,
	}
	for name, block := range cases {
		if _, err := Load([]byte(baseWorker + block)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestKubernetesVersionMustBeInTheSupportedRange(t *testing.T) {
	// The binaries can be fetched for any version; the CONFIGURATION cannot be
	// written for any version. kubeadm's API is v1beta4 from 1.31, so a 1.30 node
	// would be handed a document its kubeadm refuses -- and the failure would come
	// from kubeadm, naming a field, rather than from here naming the version.
	//
	// A FLOOR, no ceiling: minor releases appear every ~4 months, and a ceiling
	// would mean rebuilding the OS image for each one. Newer versions are accepted
	// until a kubeadm refuses them.
	accept := []string{
		"v1.31.0", "v1.31.4", "v1.32.7", "v1.33.0", "v1.34.0",
		"v1.35.6", "v1.36.0", "v1.37.1", "v1.38.0", "v1.42.3",
	}
	refuse := []string{"v1.30.9", "v1.29.0", "v1.27.5"}

	for _, v := range accept {
		doc := strings.Replace(baseWorker, "version: v1.31.0", "version: "+v, 1)
		if _, err := Load([]byte(doc)); err != nil {
			t.Errorf("kubernetes.version %s was refused: %v", v, err)
		}
	}
	for _, v := range refuse {
		doc := strings.Replace(baseWorker, "version: v1.31.0", "version: "+v, 1)
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("kubernetes.version %s was accepted, outside the supported range", v)
		}
	}
}

func TestKubernetesMinorParsesTheVersion(t *testing.T) {
	for version, want := range map[string]int{
		"v1.31.0": 31, "v1.31.14": 31, "v2.7.3": 7,
	} {
		if got, ok := KubernetesMinor(version); !ok || got != want {
			t.Errorf("KubernetesMinor(%q) = %d, %v; want %d", version, got, ok, want)
		}
	}
	for _, bad := range []string{"", "1.31.0", "v1.31", "v1.x.0"} {
		if _, ok := KubernetesMinor(bad); ok {
			t.Errorf("KubernetesMinor(%q) claimed to parse", bad)
		}
	}
}

func TestDashboardModeDefaultsToTheTextOne(t *testing.T) {
	// Unconfigured must mean the graphical dashboard: a machine with a screen
	// should show it, and the console falls back to the text one by itself when
	// there is no DRM device or the renderer cannot take the screen. An explicit
	// dashboard.mode still wins.
	c, err := Load([]byte(baseWorker))
	if err != nil {
		t.Fatalf("the base worker document no longer validates: %v", err)
	}
	if got := c.DashboardModeValue(); got != DashboardGUI {
		t.Errorf("the default dashboard mode is %q, want %q", got, DashboardGUI)
	}
}

func TestDashboardModeIsOneOfTwo(t *testing.T) {
	for _, mode := range []string{"tui", "gui"} {
		doc := baseWorker + "\ndashboard:\n  mode: " + mode + "\n"
		c, err := Load([]byte(doc))
		if err != nil {
			t.Errorf("dashboard.mode %q was refused: %v", mode, err)
			continue
		}
		if got := c.DashboardModeValue(); got != DashboardMode(mode) {
			t.Errorf("dashboard.mode %q read back as %q", mode, got)
		}
	}

	// A misspelled mode must be refused rather than falling back to the default:
	// a silent fallback would leave an operator believing the graphical
	// dashboard is coming.
	//
	// "none" is the first entry on purpose, and it is the interesting one. It
	// used to be a value, and it left a login prompt on the console. It was
	// removed because that prompt is a local door this image cannot even open --
	// no account carries a password -- so it bought no rescue while advertising
	// the operating system and its version. tui or gui, and nothing else.
	for _, bad := range []string{"none", "GUI", "graphical", "gtk", "true"} {
		doc := baseWorker + "\ndashboard:\n  mode: " + bad + "\n"
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("dashboard.mode %q was accepted", bad)
		}
	}
}

// TestCloudProviderIsExternalOrNothing pins the cloud switch: setting it makes
// the kubelet defer the node's lifecycle to a CCM, and "external" is the only
// value Kubernetes still takes.
func TestCloudProviderIsExternalOrNothing(t *testing.T) {
	c, err := Load([]byte(baseWorker + "\ncloud:\n  provider: external\n"))
	if err != nil {
		t.Fatalf("Load() rejected provider: external: %v", err)
	}
	if !c.UsesExternalCloudProvider() {
		t.Error("UsesExternalCloudProvider() = false with provider: external")
	}

	// Unset means no CCM, which is today's behaviour and must stay so.
	c, err = Load([]byte(baseWorker))
	if err != nil {
		t.Fatalf("Load() rejected the base document: %v", err)
	}
	if c.UsesExternalCloudProvider() {
		t.Error("UsesExternalCloudProvider() = true with no cloud block")
	}

	// Kubernetes 1.31 removed the in-tree providers; any other name would
	// select one the kubelet no longer has.
	for _, bad := range []string{"aws", "xenorchestra", "external ", "EXTERNAL"} {
		if _, err := Load([]byte(baseWorker + "\ncloud:\n  provider: \"" + bad + "\"\n")); err == nil {
			t.Errorf("cloud.provider %q was accepted", bad)
		}
	}
}

// TestCloudManifestsRequireTheExternalProviderAndAURL guards the two ways the
// optional manifest list can be wrong: applied with nothing deferring to it, or
// naming something that is not an http(s) document.
func TestCloudManifestsRequireTheExternalProviderAndAURL(t *testing.T) {
	// Manifests are the external provider's: applying them while the kubelet
	// still owns the lifecycle leaves two things deciding what runs.
	noProvider := baseWorker + "\ncloud:\n  manifests:\n    - https://example.test/ccm.yaml\n"
	if _, err := Load([]byte(noProvider)); err == nil {
		t.Error("Load() accepted cloud.manifests without provider: external")
	}

	ok := baseWorker + "\ncloud:\n  provider: external\n  manifests:\n    - https://example.test/ccm.yaml\n"
	c, err := Load([]byte(ok))
	if err != nil {
		t.Fatalf("Load() rejected valid cloud.manifests: %v", err)
	}
	if len(c.Cloud.Manifests) != 1 {
		t.Errorf("Cloud.Manifests = %v, want one entry", c.Cloud.Manifests)
	}

	for _, bad := range []string{"file:///etc/passwd", "example.test/ccm.yaml", "ftp://example.test/x"} {
		doc := baseWorker + "\ncloud:\n  provider: external\n  manifests:\n    - " + bad + "\n"
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("cloud.manifests accepted %q", bad)
		}
	}
}

// TestNTPServersFallBackAndRefuseInjection pins the time section: absent servers
// fall back to the default pool, so a node with no explicit time configuration
// still synchronizes; the configured list wins when present; and a value that
// could start a new chrony directive is refused rather than pasted into a root
// daemon's configuration.
func TestNTPServersFallBackAndRefuseInjection(t *testing.T) {
	c, err := Load([]byte(baseWorker))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.NTPServers(); len(got) != len(DefaultNTPServers) || got[0] != DefaultNTPServers[0] {
		t.Errorf("default NTPServers = %v, want %v", got, DefaultNTPServers)
	}

	c, err = Load([]byte(baseWorker + "\ntime:\n  servers:\n    - ntp1.example\n    - 192.168.1.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.NTPServers(); len(got) != 2 || got[0] != "ntp1.example" {
		t.Errorf("configured NTPServers = %v, want the two configured servers", got)
	}

	for _, bad := range []string{"a\nb", "a b"} {
		doc := baseWorker + "\ntime:\n  servers:\n    - \"" + bad + "\"\n"
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("time.servers accepted %q", bad)
		}
	}
}
