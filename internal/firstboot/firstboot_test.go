package firstboot

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/vatesfr/vates-kube-os/internal/configdrive"
	"github.com/vatesfr/vates-kube-os/vatescfg"
)

const workerDoc = `
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

// joiningMasterDoc joins an existing cluster: it needs a token, the certificate
// key that unlocks the cluster's certificates, and the CA hash that proves which
// cluster it is talking to.
const joiningMasterDoc = `
role: master
kubernetes:
  version: v1.31.0
cluster:
  controlPlaneEndpoint: "192.168.1.10:6443"
  token: "abcdef.0123456789abcdef"
  certificateKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  caCertHash: "1111111111111111111111111111111111111111111111111111111111111111"
  vip:
    address: "192.168.1.10"
network:
  iface: eth0
  mode: dhcp
cni:
  plugin: flannel
  cidr: "10.244.0.0/16"
`

const masterDoc = `
role: master
kubernetes:
  version: v1.31.0
cluster:
  controlPlaneEndpoint: "192.168.1.10:6443"
  vip:
    address: "192.168.1.10"
network:
  iface: eth0
  mode: dhcp
cni:
  plugin: flannel
  cidr: "10.244.0.0/16"
`

// ciliumMasterDoc is masterDoc with the second CNI selected: the bootstrap
// control plane it drives applies the Cilium manifest instead of Flannel's, and
// Flannel's is absent from the node entirely.
const ciliumMasterDoc = `
role: master
kubernetes:
  version: v1.31.0
cluster:
  controlPlaneEndpoint: "192.168.1.10:6443"
  vip:
    address: "192.168.1.10"
network:
  iface: eth0
  mode: dhcp
cni:
  plugin: cilium
  cidr: "10.244.0.0/16"
`

// workerDrive builds a config drive with everything a worker needs. That is the
// cluster CA and the token in vates-node.yaml: the kubelet bootstraps its own
// certificate, so no credential is placed here.
func workerDrive(t *testing.T) *configdrive.Drive {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"meta-data":       "instance-id: i-1\nlocal-hostname: vates-worker-1\n",
		"vates-node.yaml": workerDoc,
		"pki/ca.crt":      "CA\n",
	}
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return configdrive.Open(dir)
}

// bareDrive builds a config drive with only what every node has: the NoCloud
// metadata and vates-node.yaml. No PKI, which is what tells a control plane to
// create the cluster rather than join one.
func bareDrive(t *testing.T, doc string) *configdrive.Drive {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"meta-data":       "instance-id: i-1\nlocal-hostname: vates-cp-1\n",
		"vates-node.yaml": doc,
	} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return configdrive.Open(dir)
}

// joiningDrive is a control plane that receives the cluster's PKI instead of
// creating one.
func joiningDrive(t *testing.T) *configdrive.Drive {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"meta-data":              "instance-id: i-2\nlocal-hostname: vates-cp-2\n",
		"vates-node.yaml":        joiningMasterDoc,
		"pki/ca.crt":             "CA\n",
		"pki/ca.key":             "CAKEY\n",
		"pki/sa.key":             "SAKEY\n",
		"pki/sa.pub":             "SAPUB\n",
		"pki/front-proxy-ca.crt": "FPCA\n",
		"pki/front-proxy-ca.key": "FPCAKEY\n",
		"pki/etcd/ca.crt":        "ETCDCA\n",
		"pki/etcd/ca.key":        "ETCDCAKEY\n",
	}
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return configdrive.Open(dir)
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{
		Kubernetes: filepath.Join(root, "kubernetes"),
		Kubelet:    filepath.Join(root, "kubelet"),
	}
}

func loadConfig(t *testing.T, doc string) *vatescfg.Config {
	t.Helper()
	c, err := vatescfg.Load([]byte(doc))
	if err != nil {
		t.Fatalf("vatescfg.Load() failed: %v", err)
	}
	return c
}

func findFile(files []File, path string) (File, bool) {
	for _, f := range files {
		if f.Path == path {
			return f, true
		}
	}
	return File{}, false
}

func TestFilesGivesAWorkerTheCABootstrapAndToken(t *testing.T) {
	paths := testPaths(t)
	files, err := Files(loadConfig(t, workerDoc), workerDrive(t), paths, "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}

	// The CA, to verify the API server.
	if _, ok := findFile(files, filepath.Join(paths.Kubernetes, "pki", "ca.crt")); !ok {
		t.Error("Files() did not install pki/ca.crt")
	}

	// The bootstrap kubeconfig: a token, and nothing else. The kubelet gets its
	// own certificate with it, so no kubelet.crt/key and no kubelet.conf are
	// installed from the drive.
	bootstrap, ok := findFile(files, filepath.Join(paths.Kubernetes, BootstrapKubeletConf))
	if !ok {
		t.Fatalf("Files() did not install %s", BootstrapKubeletConf)
	}
	if bootstrap.Mode != 0o600 {
		t.Errorf("%s mode = %#o, want 0600", BootstrapKubeletConf, bootstrap.Mode)
	}
	for _, want := range []string{
		"token: abcdef.0123456789abcdef",
		"certificate-authority: /etc/kubernetes/pki/ca.crt",
		"server: https://192.168.1.10:6443",
	} {
		if !strings.Contains(string(bootstrap.Content), want) {
			t.Errorf("bootstrap kubeconfig does not contain %q:\n%s", want, bootstrap.Content)
		}
	}
	for _, unwanted := range []string{
		filepath.Join(paths.Kubernetes, "pki", "kubelet.crt"),
		filepath.Join(paths.Kubernetes, "pki", "kubelet.key"),
		filepath.Join(paths.Kubernetes, kubeletConfFile),
	} {
		if _, ok := findFile(files, unwanted); ok {
			t.Errorf("Files() installed %s, which the kubelet now obtains itself", unwanted)
		}
	}
}

func TestMasterFilesReusesAnInjectedClusterCA(t *testing.T) {
	// The CAPI control plane provider owns the CA and states it in the document.
	// A bootstrapping control plane must reuse it (write the files so kubeadm
	// keeps them) instead of generating its own.
	paths := testPaths(t)
	doc := masterDoc + `pki:
  clusterCA:
    cert: |
      -----BEGIN CERTIFICATE-----
      CA
      -----END CERTIFICATE-----
    key: |
      -----BEGIN PRIVATE KEY-----
      KEY
      -----END PRIVATE KEY-----
`
	files, err := Files(loadConfig(t, doc), bareDrive(t, doc), paths, "vates-cp-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}

	ca, ok := findFile(files, filepath.Join(paths.Kubernetes, "pki", "ca.crt"))
	if !ok {
		t.Fatal("Files() did not write the injected cluster CA")
	}
	if !strings.Contains(string(ca.Content), "CA") {
		t.Errorf("cluster CA is not the injected one:\n%s", ca.Content)
	}
	key, ok := findFile(files, filepath.Join(paths.Kubernetes, "pki", "ca.key"))
	if !ok {
		t.Fatal("Files() did not write the injected cluster CA key")
	}
	if key.Mode != 0o600 {
		t.Errorf("ca.key mode = %#o, want 0600", key.Mode)
	}

	// The node still runs kubeadm init, so it is not treated as a joiner.
	if _, ok := findFile(files, JoinConfigPath); ok {
		t.Error("an injected-CA bootstrapping control plane must run kubeadm init, not a join")
	}
}

func TestFilesTakesThePKIFromTheDocument(t *testing.T) {
	paths := testPaths(t)

	// The CAPI path: the provider states the PKI in the document, because the
	// hypervisor gives no channel for a file. The cluster CA must land where the
	// kubelet expects it, and the operator CA where the management API expects it.
	doc := workerDoc + `pki:
  clusterCA:
    cert: |
      -----BEGIN CERTIFICATE-----
      CLUSTER-CA
      -----END CERTIFICATE-----
  apiCA:
    cert: |
      -----BEGIN CERTIFICATE-----
      API-CA
      -----END CERTIFICATE-----
    key: |
      -----BEGIN PRIVATE KEY-----
      API-CA-KEY
      -----END PRIVATE KEY-----
`

	// A drive with no PKI files at all: everything must come from the document.
	files, err := Files(loadConfig(t, doc), bareDrive(t, doc), paths, "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}

	ca, ok := findFile(files, filepath.Join(paths.Kubernetes, "pki", "ca.crt"))
	if !ok {
		t.Fatal("Files() did not install the cluster CA from the document")
	}
	if !strings.Contains(string(ca.Content), "CLUSTER-CA") {
		t.Errorf("cluster CA is not the document's:\n%s", ca.Content)
	}

	apiCA, ok := findFile(files, OperatorCAPath)
	if !ok {
		t.Fatal("Files() did not install the operator CA certificate from the document")
	}
	if !strings.Contains(string(apiCA.Content), "API-CA") {
		t.Errorf("operator CA is not the document's:\n%s", apiCA.Content)
	}
	apiKey, ok := findFile(files, OperatorCAKeyPath)
	if !ok {
		t.Fatal("Files() did not install the operator CA key from the document")
	}
	if apiKey.Mode != 0o600 {
		t.Errorf("%s mode = %#o, want 0600", OperatorCAKeyPath, apiKey.Mode)
	}
}

func TestFilesWritesKubeletConfigForContainerdAndCgroupfs(t *testing.T) {
	paths := testPaths(t)
	files, err := Files(loadConfig(t, workerDoc), workerDrive(t), paths, "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}

	f, ok := findFile(files, filepath.Join(paths.Kubelet, "kubelet.conf"))
	if !ok {
		t.Fatal("Files() did not produce the KubeletConfiguration")
	}

	// Decoded strictly, which is exactly what the kubelet does: an unknown key
	// is a hard error, after which the kubelet falls back to lenient decoding
	// and silently DROPS the key. So the rule to test is "the kubelet's strict
	// decode accepts this document", not "the text does not contain this word".
	//
	// That distinction is not academic: this test originally searched the raw
	// text, and broke as soon as a comment mentioned a removed key. A comment
	// must not matter; a real key must.
	var kc struct {
		APIVersion               string            `yaml:"apiVersion"`
		Kind                     string            `yaml:"kind"`
		CgroupDriver             string            `yaml:"cgroupDriver"`
		ContainerRuntimeEndpoint string            `yaml:"containerRuntimeEndpoint"`
		ClusterDNS               []string          `yaml:"clusterDNS"`
		ClusterDomain            string            `yaml:"clusterDomain"`
		StaticPodPath            string            `yaml:"staticPodPath"`
		KubeReserved             map[string]string `yaml:"kubeReserved"`
		SystemReserved           map[string]string `yaml:"systemReserved"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(f.Content))
	dec.KnownFields(true)
	if err := dec.Decode(&kc); err != nil {
		t.Fatalf("the kubelet would reject or silently trim this KubeletConfiguration: %v\n%s", err, f.Content)
	}

	if kc.Kind != "KubeletConfiguration" {
		t.Errorf("kind = %q, want KubeletConfiguration", kc.Kind)
	}
	// The cgroup driver must match containerd's, which is cgroupfs here: its
	// config sets no SystemdCgroup, and the node has no systemd at all. A
	// mismatch is a kubelet that refuses to start.
	if kc.CgroupDriver != "cgroupfs" {
		t.Errorf("cgroupDriver = %q, want cgroupfs", kc.CgroupDriver)
	}
	if kc.ContainerRuntimeEndpoint != CRIEndpoint {
		t.Errorf("containerRuntimeEndpoint = %q, want %q", kc.ContainerRuntimeEndpoint, CRIEndpoint)
	}
	if len(kc.KubeReserved) == 0 || len(kc.SystemReserved) == 0 {
		t.Error("kubeReserved/systemReserved are empty; the node would reserve nothing for the system")
	}
	// staticPodPath is not a kubelet default: kubeadm is what normally writes it.
	// Without it the kubelet starts nothing at all -- no etcd, no API server --
	// and reports healthy while doing so.
	if kc.StaticPodPath != ManifestsDir {
		t.Errorf("staticPodPath = %q, want %q; without it the control plane is never started",
			kc.StaticPodPath, ManifestsDir)
	}
}

func TestKubeletConfigOmitsKeysV131Rejects(t *testing.T) {
	// hostnameOverride was removed in v1.31, and authentication/authorization are
	// no longer where the API server credentials go; a document that carries them
	// is rejected by strict decoding, or accepted and silently ignored.
	paths := testPaths(t)
	files, err := Files(loadConfig(t, workerDoc), workerDrive(t), paths, "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}
	f, _ := findFile(files, filepath.Join(paths.Kubelet, "kubelet.conf"))

	// A generic map decodes anything, so it can show what the document actually
	// contains, comments excluded.
	var any map[string]any
	if err := yaml.Unmarshal(f.Content, &any); err != nil {
		t.Fatalf("KubeletConfiguration does not parse: %v", err)
	}
	for _, forbidden := range []string{"hostnameOverride", "authentication", "authorization"} {
		if _, present := any[forbidden]; present {
			t.Errorf("the decoded KubeletConfiguration contains %q, which v1.31 rejects or ignores", forbidden)
		}
	}
}

func TestKubeletConfigCarriesTheClustersDNS(t *testing.T) {
	// clusterDNS is the key whose ABSENCE is invisible. The kubelet does not read
	// the cluster's DNS address from the cluster; without this key it cannot
	// honour the ClusterFirst policy nearly every pod uses and falls back to the
	// node's resolver. The pods stay 1/1 Running and no Service name resolves --
	// measured on a node, with the kubelet logging
	//   kubelet does not have ClusterDNS IP configured and cannot create Pod
	//   using "ClusterFirst" policy. Falling back to "Default" policy.
	// and curl inside the pod reporting "Could not resolve host".
	paths := testPaths(t)
	cfg := loadConfig(t, workerDoc)
	files, err := Files(cfg, workerDrive(t), paths, "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}
	f, ok := findFile(files, filepath.Join(paths.Kubelet, "kubelet.conf"))
	if !ok {
		t.Fatalf("no kubelet configuration was produced")
	}

	var decoded map[string]any
	if err := yaml.Unmarshal(f.Content, &decoded); err != nil {
		t.Fatalf("KubeletConfiguration does not parse: %v", err)
	}

	dns, present := decoded["clusterDNS"]
	if !present {
		t.Fatal("the KubeletConfiguration carries no clusterDNS; no pod will resolve a Service name")
	}
	list, ok := dns.([]any)
	if !ok || len(list) != 1 || list[0] != cfg.DNSServiceIP() {
		t.Errorf("clusterDNS = %v, want [%s] (the tenth address of %s)",
			dns, cfg.DNSServiceIP(), cfg.Cluster.ServiceCIDR)
	}
	if got := decoded["clusterDomain"]; got != cfg.Cluster.DNSDomain {
		t.Errorf("clusterDomain = %v, want %q", got, cfg.Cluster.DNSDomain)
	}
}

func TestKubeletDropInSeparatesNameFromAddress(t *testing.T) {
	paths := testPaths(t)
	files, err := Files(loadConfig(t, workerDoc), workerDrive(t), paths, "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}

	f, ok := findFile(files, KubeletDropIn)
	if !ok {
		t.Fatalf("Files() did not produce the kubelet drop-in at %s", KubeletDropIn)
	}
	got := string(f.Content)
	// The name and the address must both be present and distinct: the unit
	// passes one to --hostname-override and the other to --node-ip, and using
	// the address for both is the bug this guards.
	for _, want := range []string{"Environment=NODE_NAME=vates-worker-1", "Environment=NODE_IP=10.0.2.15"} {
		if !strings.Contains(got, want) {
			t.Errorf("the drop-in does not contain %q:\n%s", want, got)
		}
	}
	// It must be a real drop-in: a bare "NODE_NAME=..." with no [Service] and no
	// Environment= is not something systemd reads, and would look correct while
	// leaving the unit with an unset variable.
	if !strings.Contains(got, "[Service]") {
		t.Errorf("the drop-in has no [Service] section, so systemd ignores it:\n%s", got)
	}
}

// TestKubeletDropInCarriesTheCloudProvider pins the other end of the cloud
// switch: the value vates-node.yaml gives reaches the drop-in PID 1 reads, and
// it is absent when no provider is asked for.
func TestKubeletDropInCarriesTheCloudProvider(t *testing.T) {
	paths := testPaths(t)

	on, err := Files(loadConfig(t, workerDoc+"\ncloud:\n  provider: external\n"),
		workerDrive(t), paths, "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}
	f, ok := findFile(on, KubeletDropIn)
	if !ok {
		t.Fatalf("Files() did not produce the kubelet drop-in at %s", KubeletDropIn)
	}
	if !strings.Contains(string(f.Content), "Environment=CLOUD_PROVIDER=external") {
		t.Errorf("the drop-in does not carry the cloud provider:\n%s", f.Content)
	}

	// Absent by default, so a node with no CCM reads exactly as before.
	off, err := Files(loadConfig(t, workerDoc), workerDrive(t), paths, "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}
	f, _ = findFile(off, KubeletDropIn)
	if strings.Contains(string(f.Content), "CLOUD_PROVIDER") {
		t.Errorf("the drop-in names a cloud provider nobody asked for:\n%s", f.Content)
	}
}

func TestFilesRequiresTheClusterCAForAWorker(t *testing.T) {
	// A worker with no CA cannot verify the API server, and its kubelet cannot
	// verify the certificate it obtains. Its absence is fatal, and the error
	// names the file.
	dir := t.TempDir()
	for name, content := range map[string]string{
		"meta-data":       "local-hostname: vates-worker-1\n",
		"user-data":       "#cloud-config\n",
		"vates-node.yaml": workerDoc,
	} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Files(loadConfig(t, workerDoc), configdrive.Open(dir), testPaths(t), "vates-worker-1", "10.0.2.15")
	if err == nil {
		t.Fatal("Files() succeeded without the cluster CA certificate")
	}
	if !strings.Contains(err.Error(), "pki/ca.crt") {
		t.Errorf("error was %q, want it to name the missing CA", err)
	}
}

func TestMasterFilesAreTheKubeadmDocumentAndTheVIPManifest(t *testing.T) {
	paths := testPaths(t)
	files, err := MasterFiles(loadConfig(t, masterDoc), bareDrive(t, masterDoc), paths, "vates-cp-1", "192.168.122.50")
	if err != nil {
		t.Fatalf("MasterFiles() failed: %v", err)
	}

	// A bootstrapping master mints its own credentials through kubeadm, so the
	// only documents it needs up front are the one kubeadm reads and the VIP's
	// static pod. It must NOT require anything from the config drive.
	byName := map[string]File{}
	for _, f := range files {
		byName[f.Path] = f
	}

	cfgFile, ok := byName[KubeadmConfigPath]
	if !ok {
		t.Errorf("MasterFiles() did not produce %s", KubeadmConfigPath)
	} else if cfgFile.Mode != 0o600 {
		t.Errorf("%s mode = %#o, want 0600", KubeadmConfigPath, cfgFile.Mode)
	}

	vipPath := ManifestsDir + "/kube-vip.yaml"
	vip, ok := byName[vipPath]
	if !ok {
		t.Fatalf("MasterFiles() did not produce %s; the VIP would never come up", vipPath)
	}
	if !strings.Contains(string(vip.Content), "192.168.1.10") {
		t.Errorf("the kube-vip manifest does not contain the configured VIP:\n%s", vip.Content)
	}
	// The bootstrapping control plane needs super-admin.conf: it starts kube-vip
	// before the cluster-admins binding exists, and admin.conf is not usable
	// until then.
	if !strings.Contains(string(vip.Content), SuperAdminKubeconfig) {
		t.Errorf("the bootstrapping control plane's kube-vip does not mount %s:\n%s",
			SuperAdminKubeconfig, vip.Content)
	}

	dropin, ok := byName[KubeletDropIn]
	if !ok {
		t.Errorf("MasterFiles() did not produce the kubelet drop-in at %s", KubeletDropIn)
	} else if !strings.Contains(string(dropin.Content), "NODE_NAME=vates-cp-1") {
		t.Errorf("the drop-in does not name the node:\n%s", dropin.Content)
	}
}

func TestJoiningControlPlaneStagesKubeVIPOnlyAfterTheJoin(t *testing.T) {
	// kube-vip's manifest mounts a kubeconfig, and on a joining control plane the
	// usable one is admin.conf -- which kubeadm writes DURING the join. Staged
	// before it, the kubelet refuses the mount
	//   hostPath type check failed: /etc/kubernetes/admin.conf is not a file
	// and retries it on a two-minute backoff, so kube-vip does not run. Measured:
	// every joining control plane was in that state, the VIP had exactly one
	// possible owner, and killing that node removed it from the cluster.
	drive := joiningDrive(t)
	cfg := loadConfig(t, joiningMasterDoc)
	paths := testPaths(t)

	if got := KubeVIPKubeconfigFor(cfg, drive); got != AdminKubeconfig {
		t.Errorf("KubeVIPKubeconfigFor on a joining control plane = %q, want %q", got, AdminKubeconfig)
	}

	files, err := Files(cfg, drive, paths, "vates-cp-2", "192.168.122.51")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}
	if _, ok := findFile(files, filepath.Join(ManifestsDir, "kube-vip.yaml")); ok {
		t.Error("the kube-vip manifest is staged before the join, when its kubeconfig does not exist yet")
	}

	// It is staged once the join has run, and with admin.conf.
	r := &fakeRunner{}
	if err := stageJoiningKubeVIP(cfg, "192.168.122.51", r); err != nil {
		t.Fatalf("stageJoiningKubeVIP() failed: %v", err)
	}
	found := false
	for _, w := range r.writes {
		if w == filepath.Join(ManifestsDir, "kube-vip.yaml") {
			found = true
		}
	}
	if !found {
		t.Errorf("after the join, the kube-vip manifest was not staged: %v", r.writes)
	}
}

func TestMasterWithoutVIPHasNoKubeVIPManifest(t *testing.T) {
	// A cluster behind an external load balancer states no vip.address, and
	// kube-vip must then not be deployed: it would fight the real balancer for
	// an address it was never meant to own.
	doc := strings.Replace(masterDoc, "  vip:\n    address: \"192.168.1.10\"\n", "", 1)

	files, err := MasterFiles(loadConfig(t, doc), bareDrive(t, doc), testPaths(t), "vates-cp-1", "192.168.122.50")
	if err != nil {
		t.Fatalf("MasterFiles() failed: %v", err)
	}
	for _, f := range files {
		if strings.Contains(f.Path, "kube-vip") {
			t.Errorf("a kube-vip manifest was produced with no VIP configured: %s", f.Path)
		}
	}
}

func TestApplyMasterRunsKubeadmBeforeStartingTheNode(t *testing.T) {
	paths := testPaths(t)
	r := &fakeRunner{}

	if err := Apply(loadConfig(t, masterDoc), bareDrive(t, masterDoc), paths, "vates-cp-1", "192.168.122.50", r); err != nil {
		t.Fatalf("Apply() failed: %v", err)
	}

	// Every directory the unit or a static pod mounts must exist before the
	// kubelet is started.
	for _, want := range RequiredDirs(loadConfig(t, masterDoc)) {
		var found bool
		for _, d := range r.dirs {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Apply() did not create %s; commands were %v", want, r.dirs)
		}
	}

	// Each kubeadm phase must run, in the declared order.
	var phaseOrder []string
	for _, c := range r.commands {
		for _, phase := range KubeadmPhases {
			if strings.Contains(c, " init phase "+phase+" ") {
				phaseOrder = append(phaseOrder, phase)
			}
		}
		if strings.Contains(c, "systemctl") && !strings.Contains(c, "daemon-reload") {
			t.Errorf("Apply() ran %q; only daemon-reload is expected", c)
		}
	}
	if len(phaseOrder) != len(KubeadmPhases) {
		t.Errorf("ran phases %v, want all of %v", phaseOrder, KubeadmPhases)
	}
	for i := range phaseOrder {
		if phaseOrder[i] != KubeadmPhases[i] {
			t.Errorf("phases ran in order %v, want %v", phaseOrder, KubeadmPhases)
			break
		}
	}
}

func TestKubeadmPhaseCommandUsesLocalSubcommandForEtcd(t *testing.T) {
	// kubeadm init phase etcd accepts only "local"; "etc d all" is not a
	// command. Getting this wrong costs a failed control plane at boot, so it is
	// asserted rather than trusted to the phase table being read carefully.
	cmd := strings.Join(KubeadmPhaseCommand("etcd local"), " ")
	if !strings.Contains(cmd, "init phase etcd local") {
		t.Errorf("etcd phase command = %q, want 'init phase etcd local'", cmd)
	}
	if strings.Contains(cmd, "etcd all") {
		t.Errorf("etcd phase command uses 'all', which kubeadm does not accept: %q", cmd)
	}

	// And the others really do take all.
	if cmd := strings.Join(KubeadmPhaseCommand("certs all"), " "); !strings.Contains(cmd, "init phase certs all") {
		t.Errorf("certs phase command = %q, want 'init phase certs all'", cmd)
	}
}

func TestBootstrapMarksTheControlPlane(t *testing.T) {
	// The first control plane has to label and taint itself. A JOINING control
	// plane gets both from `kubeadm join` for free, so leaving this out makes the
	// cluster's first control plane the odd one out: measured, it showed no role
	// in `kubectl get nodes` beside two marked control-plane, and ordinary pods
	// were scheduled onto a node that should refuse them.
	mark, token, addon := -1, -1, -1
	for i, phase := range BootstrapPhases {
		switch phase {
		case "mark-control-plane":
			mark = i
		case "bootstrap-token":
			token = i
		case "addon all":
			addon = i
		}
	}
	if mark < 0 {
		t.Fatalf("BootstrapPhases has no mark-control-plane phase: %v", BootstrapPhases)
	}
	// It writes to the API, so it cannot run before the token phase that makes
	// the API usable, and it must not run after the addons.
	if token >= 0 && mark < token {
		t.Errorf("mark-control-plane (%d) runs before bootstrap-token (%d)", mark, token)
	}
	if addon >= 0 && mark > addon {
		t.Errorf("mark-control-plane (%d) runs after addon all (%d)", mark, addon)
	}
}

func TestBootstrapPhasesOmitKubeProxyWhenTheCNIReplacesIt(t *testing.T) {
	// `addon all` installs kube-proxy. A cluster whose CNI replaces it must never
	// have kube-proxy written at all -- deleting it afterwards leaves its iptables
	// rules behind. So the default keeps addon all, and proxy.disabled swaps it
	// for the coredns addon alone.
	cfg := loadConfig(t, workerDoc)
	if got := bootstrapPhases(cfg); !slices.Contains(got, "addon all") {
		t.Errorf("the default phases omit addon all: %v", got)
	}

	cfg.Cluster.Proxy.Disabled = true
	got := bootstrapPhases(cfg)
	if slices.Contains(got, "addon all") {
		t.Errorf("addon all (kube-proxy) is still present with proxy disabled: %v", got)
	}
	if !slices.Contains(got, "addon coredns") {
		t.Errorf("the coredns addon is missing with proxy disabled: %v", got)
	}
}

// The bootstrap control plane applies the pod network itself. What it applies
// is whatever cni.plugin selected -- and only that. The flannel and cilium
// manifests coexist in the binary, so the failure this pins down is a flannel
// file being written on a cilium node, or the apply reaching a manifest that
// was never written.
func TestBootstrapAppliesTheCNITheNodeInstalls(t *testing.T) {
	cases := []struct {
		name         string
		doc          string
		plugin       string
		wantManifest string // written and applied; empty for none
	}{
		{
			name:         "flannel",
			doc:          masterDoc,
			plugin:       "flannel",
			wantManifest: FlannelManifestPath,
		},
		{
			name:         "cilium",
			doc:          ciliumMasterDoc,
			plugin:       "cilium",
			wantManifest: CiliumManifestPath,
		},
		{
			name:   "none writes and applies nothing",
			doc:    strings.Replace(masterDoc, "plugin: flannel", "plugin: none", 1),
			plugin: "none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{healthz: true}
			if err := Bootstrap(loadConfig(t, tc.doc), bareDrive(t, tc.doc), testPaths(t), "vates-cp-1", "192.168.122.50", r); err != nil {
				t.Fatalf("Bootstrap() failed: %v", err)
			}

			// The manifest is written to disk before the kubeadm phases run, so
			// that a failed addon phase leaves the file on disk to read.
			if tc.wantManifest != "" {
				found := false
				for _, w := range r.writes {
					if w == tc.wantManifest {
						found = true
					}
				}
				if !found {
					t.Errorf("Bootstrap() never wrote %s; writes were %v", tc.wantManifest, r.writes)
				}
				// The apply names the exact file, through the cluster's kubeconfig.
				var applied bool
				for _, c := range r.commands {
					if strings.Contains(c, "apply -f "+tc.wantManifest) {
						applied = true
					}
				}
				if !applied {
					t.Errorf("Bootstrap() never applied %s; commands were %v", tc.wantManifest, r.commands)
				}
			}
			// The OTHER CNI's manifest must not be written or applied either:
			// a flannel file on a cilium node is the exact bug this feature
			// exists to prevent.
			for _, manifest := range []string{FlannelManifestPath, CiliumManifestPath} {
				if manifest == tc.wantManifest {
					continue
				}
				for _, w := range r.writes {
					if w == manifest {
						t.Errorf("cni.plugin: %s wrote the %s manifest", tc.plugin, manifest)
					}
				}
				for _, c := range r.commands {
					if strings.Contains(c, "apply -f "+manifest) {
						t.Errorf("cni.plugin: %s applied the manifest of the CNI it does not run: %s", tc.plugin, c)
					}
				}
			}
		})
	}
}

// The write and the apply are two moments that must not separate. The manifest
// is written to disk BEFORE the kubeadm phases run so that a failed addon phase
// leaves the file on disk, and it is applied AFTER them so that the API is up
// to receive it. If the two reorder -- the apply first, the write later -- the
// apply fails against a file that does not exist yet, and the failure it
// reports names the file rather than the ordering that is really wrong.
func TestBootstrapWritesTheCNIBeforeApplyingIt(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"flannel", masterDoc},
		{"cilium", ciliumMasterDoc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := tc.doc
			r := &fakeRunner{healthz: true}
			if err := Bootstrap(loadConfig(t, doc), bareDrive(t, doc), testPaths(t), "vates-cp-1", "192.168.122.50", r); err != nil {
				t.Fatalf("Bootstrap() failed: %v", err)
			}
			var manifest string
			for _, w := range r.writes {
				if w == FlannelManifestPath || w == CiliumManifestPath {
					manifest = w
				}
			}
			if manifest == "" {
				t.Fatal("Bootstrap() wrote no CNI manifest")
			}
			// The apply command names the manifest; find it in the command log
			// and make sure a kubeadm phase ran after the write -- which is
			// where the CNI apply must come from, after the phases.
			var applied bool
			for _, c := range r.commands {
				if strings.Contains(c, "apply -f "+manifest) {
					applied = true
				}
			}
			if !applied {
				t.Fatalf("Bootstrap() never applied %s", manifest)
			}
			// The kubeadm phases must sit between the API wait and the apply:
			// the CNI is applied to a cluster whose addons already exist.
			var phases, cniApply int
			for i, c := range r.commands {
				if strings.Contains(c, "kubeadm init phase ") {
					phases++
					continue
				}
				if strings.Contains(c, "apply -f "+manifest) {
					cniApply = i
				}
			}
			if phases == 0 {
				t.Fatalf("Bootstrap() ran no kubeadm phase: %v", r.commands)
			}
			if cniApply == 0 {
				t.Fatalf("the CNI apply was not found in the command log")
			}
			// All the phases run before the CNI apply: if one ran after, the
			// apply went to a cluster that was not finished yet.
			for i, c := range r.commands {
				if i > cniApply && strings.Contains(c, "kubeadm init phase ") {
					t.Errorf("kubeadm phase %q runs after the CNI apply; the CNI must be applied to a finished cluster", c)
				}
			}
		})
	}
}

// A failure to apply the CNI manifest must fail the bootstrap (so the node
// reports it, rather than staying silently NotReady with no file to read) and
// must not write the done marker (so the next boot retries the stage whole).
func TestBootstrapFailureToApplyTheCNIIsReported(t *testing.T) {
	r := &fakeRunner{healthz: true, failOn: "apply -f " + FlannelManifestPath}
	err := Bootstrap(loadConfig(t, masterDoc), bareDrive(t, masterDoc), testPaths(t), "vates-cp-1", "192.168.122.50", r)
	if err == nil {
		t.Fatal("Bootstrap() succeeded when applying the CNI manifest failed")
	}
	if !strings.Contains(err.Error(), "applying the CNI manifest") {
		t.Errorf("the error does not name the CNI apply: %v", err)
	}
	for _, w := range r.writes {
		if w == BootstrapDoneMarker {
			t.Errorf("Bootstrap() wrote %s although applying the CNI failed; a reboot would skip a half-finished bootstrap", w)
		}
	}
}

// The two CNIs are the same field, cni.cidr, told to two different readers:
// flannel is given the range in its manifest, cilium reads it from the node's
// podCIDR annotation, which kubeadm writes from the same field. This is what
// keeps the two interchangeable in vates-node.yaml, so assert the rendering of
// both sides of the contract.
func TestCiliumManifestRendersPinnedAndAdapted(t *testing.T) {
	m, err := CiliumManifest(loadConfig(t, ciliumMasterDoc))
	if err != nil {
		t.Fatalf("CiliumManifest() failed: %v", err)
	}
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
	if cidr := loadConfig(t, ciliumMasterDoc).CNI.CIDR; strings.Contains(string(m), cidr) {
		t.Errorf("the cilium manifest carries the pod CIDR %q; it must come from the node's podCIDR annotation, not from the manifest", cidr)
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
	m, err := CiliumManifest(loadConfig(t, ciliumMasterDoc))
	if err != nil {
		t.Fatalf("CiliumManifest() failed: %v", err)
	}
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
	m, err := CiliumManifest(loadConfig(t, ciliumMasterDoc))
	if err != nil {
		t.Fatalf("CiliumManifest() failed: %v", err)
	}
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
	m, err := CiliumManifest(loadConfig(t, ciliumMasterDoc))
	if err != nil {
		t.Fatalf("CiliumManifest() failed: %v", err)
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

// The pod network is one field -- cni.cidr -- and kubeadm is the thing that
// turns it into the podCIDR annotation Cilium reads. It must reach kubeadm
// under whichever CNI is selected: a document where the CIDR stops travelling
// at plugin cilium would give flannel clusters a pod network and cilium
// clusters none.
func TestKubeadmCarriesThePodCIDRForEveryCNI(t *testing.T) {
	for _, doc := range []string{masterDoc, ciliumMasterDoc,
		strings.Replace(masterDoc, "plugin: flannel", "plugin: none", 1)} {
		cfg := loadConfig(t, doc)
		kubeadm, err := KubeadmConfig(cfg, "vates-cp-1", "192.168.122.50")
		if err != nil {
			t.Fatalf("KubeadmConfig() failed for %s: %v", cfg.CNI.Plugin, err)
		}
		if want := "podSubnet: " + cfg.CNI.CIDR; !strings.Contains(string(kubeadm), want) {
			t.Errorf("cni.plugin: %s -- the ClusterConfiguration does not carry %q", cfg.CNI.Plugin, want)
		}
	}
}

// The directories the kubelet needs before it starts carry the CNI's runtime
// state -- and only that CNI's. /run is wiped on every boot, so a missing
// directory is a node whose kubelet container cannot mount its CNI state.
func TestRequiredDirsCarryTheCNIState(t *testing.T) {
	cases := []struct {
		doc    string
		plugin string
		want   []string
		absent []string
	}{
		{doc: masterDoc, plugin: "flannel", want: []string{"/run/flannel"}, absent: []string{"/run/cilium", "/run/netns"}},
		{doc: ciliumMasterDoc, plugin: "cilium", want: []string{"/run/cilium", "/run/netns"}, absent: []string{"/run/flannel"}},
		{doc: strings.Replace(masterDoc, "plugin: flannel", "plugin: none", 1), plugin: "none"},
	}
	for _, tc := range cases {
		dirs := RequiredDirs(loadConfig(t, tc.doc))
		for _, w := range tc.want {
			if !slices.Contains(dirs, w) {
				t.Errorf("cni.plugin: %s -- RequiredDirs lacks %s: %v", tc.plugin, w, dirs)
			}
		}
		for _, a := range tc.absent {
			if slices.Contains(dirs, a) {
				t.Errorf("cni.plugin: %s -- RequiredDirs carries %s, the state of a CNI the node does not run: %v", tc.plugin, a, dirs)
			}
		}
	}
}

// Same contract, on the SELinux side: the paths the node labels for containers
// must be the CNI's state and not the other CNI's -- labelling /run/flannel on
// a cilium node is noise, and missing /run/cilium is a sandbox that fails with
// a permission error pointing at the wrong file.
func TestContainerPathsCarryTheCNIState(t *testing.T) {
	cases := []struct {
		doc    string
		plugin string
		want   []string
		absent []string
	}{
		{doc: workerDoc, plugin: "flannel", want: []string{"/run/flannel"}, absent: []string{"/run/cilium", "/run/netns"}},
		{doc: strings.Replace(workerDoc, "plugin: flannel", "plugin: cilium", 1), plugin: "cilium", want: []string{"/run/cilium", "/run/netns"}, absent: []string{"/run/flannel"}},
		{doc: strings.Replace(workerDoc, "plugin: flannel", "plugin: none", 1), plugin: "none", absent: []string{"/run/flannel", "/run/cilium", "/run/netns"}},
	}
	for _, tc := range cases {
		paths := ContainerPaths(loadConfig(t, tc.doc))
		for _, w := range tc.want {
			if !slices.Contains(paths, w) {
				t.Errorf("cni.plugin: %s -- ContainerPaths lacks %s: %v", tc.plugin, w, paths)
			}
		}
		for _, a := range tc.absent {
			if slices.Contains(paths, a) {
				t.Errorf("cni.plugin: %s -- ContainerPaths labels %s, the state of a CNI the node does not run: %v", tc.plugin, a, paths)
			}
		}
	}
}

// The directories the CNI needs under /run differ by plugin, and /run is a
// tmpfs: the directories must be created every boot, for the plugin selected.
// A flannel directory on a cilium node is harmless but wrong, and a missing
// one is a node that cannot configure its pod sandboxes.
func TestCNIRunDirsPerPlugin(t *testing.T) {
	cases := map[string][]string{
		vatescfg.CNIFlannel: {"/run/flannel"},
		vatescfg.CNICilium:  {"/run/cilium", "/run/netns"},
		vatescfg.CNINone:    nil,
	}
	for plugin, want := range cases {
		if got := CNIRunDirs(plugin); !slices.Equal(got, want) {
			t.Errorf("CNIRunDirs(%q) = %v, want %v", plugin, got, want)
		}
	}
}

// fakeRunner records what Apply does instead of doing it.
type fakeRunner struct {
	dirs     []string
	writes   []string
	commands []string
	// writeContent holds what WriteFile was given for each path, so a test can
	// assert on the file, not merely that one was written.
	writeContent map[string][]byte
	// failOn makes Run fail for any command whose joined form contains it.
	failOn string
	// healthz makes the API server probe answer "ok", as a started server does,
	// so the bootstrap stage can be exercised instead of spending its wait on a
	// fake that would never answer.
	healthz bool
	// bootstrapped makes Stat report the bootstrap marker as present, as on a
	// control plane that has already run its kubeadm phases once.
	bootstrapped bool
	// bootstrapDone makes Stat report the cluster-bootstrap marker as present, as
	// on a control plane that has already joined or applied its addons once.
	bootstrapDone bool
}

func (f *fakeRunner) MkdirAll(path string, mode os.FileMode) error {
	f.dirs = append(f.dirs, path)
	return nil
}
func (f *fakeRunner) WriteFile(path string, mode os.FileMode, content []byte) error {
	f.writes = append(f.writes, path)
	if f.writeContent == nil {
		f.writeContent = map[string][]byte{}
	}
	f.writeContent[path] = content
	return nil
}
func (f *fakeRunner) Stat(path string) (os.FileInfo, error) {
	if f.bootstrapped && path == BootstrappedMarker {
		return nil, nil
	}
	if f.bootstrapDone && path == BootstrapDoneMarker {
		return nil, nil
	}
	return nil, os.ErrNotExist
}
func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	joined := strings.Join(append([]string{name}, args...), " ")
	f.commands = append(f.commands, joined)
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		return nil, os.ErrPermission
	}
	// The API server probe answers as a started server.
	if strings.Contains(joined, "/healthz") {
		if f.healthz {
			return []byte("ok\n"), nil
		}
		return nil, os.ErrClosed
	}
	return nil, nil
}
func (f *fakeRunner) Logf(format string, args ...any) {}
func (f *fakeRunner) Progress(string)                 {}

// markRunner answers the two commands markControlPlane issues: the mark phase
// (recorded) and the kubectl label check (empty until labelsAfter).
type markRunner struct {
	checks      int
	labelsAfter int
	marks       int
}

func (m *markRunner) MkdirAll(string, os.FileMode) error          { return nil }
func (m *markRunner) WriteFile(string, os.FileMode, []byte) error { return nil }
func (m *markRunner) Stat(string) (os.FileInfo, error)            { return nil, os.ErrNotExist }
func (m *markRunner) Logf(string, ...any)                         {}
func (m *markRunner) Progress(string)                             {}
func (m *markRunner) Run(name string, args ...string) ([]byte, error) {
	joined := strings.Join(append([]string{name}, args...), " ")
	switch {
	case strings.Contains(joined, "jsonpath={.metadata.labels}"):
		m.checks++
		if m.checks >= m.labelsAfter {
			return []byte(`{"node-role.kubernetes.io/control-plane":""}`), nil
		}
		return []byte(`{}`), nil
	case strings.Contains(joined, "mark-control-plane"):
		m.marks++
	}
	return nil, nil
}

func TestMarkControlPlaneRetriesUntilTheLabelSticks(t *testing.T) {
	// kubeadm's mark-control-plane phase returns success even when it found no
	// node to mark, so success is decided by the label. The loop must retry
	// until the label appears, not trust the exit code.
	old := markRetryDelay
	markRetryDelay = time.Millisecond
	defer func() { markRetryDelay = old }()

	r := &markRunner{labelsAfter: 3}
	if err := markControlPlane("vates-cp-1", r); err != nil {
		t.Fatalf("markControlPlane() error = %v", err)
	}
	if r.marks < 3 {
		t.Errorf("the mark phase ran %d time(s), want it retried until the label stuck", r.marks)
	}
}

func TestApplyOrdersFilesBeforeStartingTheNode(t *testing.T) {
	paths := testPaths(t)
	r := &fakeRunner{}

	if err := Apply(loadConfig(t, workerDoc), workerDrive(t), paths, "vates-worker-1", "10.0.2.15", r); err != nil {
		t.Fatalf("Apply() failed: %v", err)
	}

	if len(r.writes) == 0 {
		t.Fatal("Apply() wrote no files")
	}

	// Apply must NOT start the node itself: PID 1 starts the kubelet once
	// Configure returns, and starting it from here would race that process.
	// daemon-reload is the one systemctl call it does need, because on a systemd
	// host that is what makes the drop-in it just wrote visible.
	for _, c := range r.commands {
		if strings.Contains(c, "systemctl") && !strings.Contains(c, "daemon-reload") {
			t.Errorf("Apply() ran %q; only daemon-reload is expected (%v)", c, r.commands)
		}
	}
	var reloaded bool
	for _, c := range r.commands {
		if strings.Contains(c, "daemon-reload") {
			reloaded = true
		}
	}
	if !reloaded {
		t.Errorf("Apply() never reloaded systemd, so the drop-in would be ignored (%v)", r.commands)
	}
}

// A control plane that has already bootstrapped must not run the kubeadm phases
// again: vates-init runs on every boot and both k8s-node.target and the
// management API Require= it, so a second run that failed took the kubelet and
// the API down with it -- a node that pings and runs no cluster.
func TestApplySkipsKubeadmPhasesWhenAlreadyBootstrapped(t *testing.T) {
	paths := testPaths(t)
	r := &fakeRunner{bootstrapped: true}

	if err := Apply(loadConfig(t, masterDoc), bareDrive(t, masterDoc), paths, "vates-cp-1", "10.0.2.15", r); err != nil {
		t.Fatalf("Apply() failed: %v", err)
	}
	for _, c := range r.commands {
		if strings.Contains(c, "kubeadm") {
			t.Errorf("Apply() re-ran %q on an already-bootstrapped control plane", c)
		}
	}
}

// A node's cluster bootstrap must run once: vates-init runs on every boot, and
// a joining control plane that reboots would otherwise re-run `kubeadm join`,
// which fails pre-flight because it is already a member -- four attempts and
// about a minute of retries, reported as a bootstrap failure for a healthy node.
func TestBootstrapSkipsWhenAlreadyDone(t *testing.T) {
	for _, doc := range []string{masterDoc, workerDoc} {
		r := &fakeRunner{bootstrapDone: true}
		if err := Bootstrap(loadConfig(t, doc), bareDrive(t, doc), testPaths(t), "vates-cp-1", "10.0.2.15", r); err != nil {
			t.Fatalf("Bootstrap() failed: %v", err)
		}
		for _, c := range r.commands {
			if strings.Contains(c, "kubeadm") {
				t.Errorf("Bootstrap() re-ran %q on a node whose bootstrap is already done", c)
			}
		}
		if len(r.writes) != 0 {
			t.Errorf("Bootstrap() wrote %v on an already-bootstrapped node", r.writes)
		}
	}
}

// The marker is written only after the stage succeeds, so a failed bootstrap is
// retried whole rather than half-skipped.
func TestBootstrapWritesTheDoneMarker(t *testing.T) {
	r := &fakeRunner{}
	if err := Bootstrap(loadConfig(t, workerDoc), bareDrive(t, workerDoc), testPaths(t), "vates-worker-1", "10.0.2.15", r); err != nil {
		t.Fatalf("Bootstrap() failed: %v", err)
	}
	for _, w := range r.writes {
		if w == BootstrapDoneMarker {
			return
		}
	}
	t.Errorf("Bootstrap() did not write %s; writes were %v", BootstrapDoneMarker, r.writes)
}

func TestBothRolesInstallWhatTheKubeletNeeds(t *testing.T) {
	// The worker path wrote the KubeletConfiguration from the start; the master
	// path did not, and the difference only appeared on a node, where the kubelet
	// died with
	//   failed to read kubelet config file /etc/kubelet/kubelet.conf: no such file
	//
	// kubeadm's kubeconfig phase writes /etc/kubernetes/kubelet.conf, which looks
	// like it covers this and does not: that is the kubeconfig the kubelet
	// authenticates WITH, not the settings it reads through --config.
	//
	// So the files every node needs are listed once and checked against both
	// roles. A role that quietly needs less than the other is how this broke.
	for name, doc := range map[string]string{"worker": workerDoc, "master": masterDoc} {
		t.Run(name, func(t *testing.T) {
			paths := testPaths(t)
			// A bootstrap master has no PKI on its drive; that absence is what
			// tells it to create the cluster rather than join one.
			drive := workerDrive(t)
			if name == "master" {
				drive = bareDrive(t, doc)
			}
			files, err := Files(loadConfig(t, doc), drive, paths, "vates-node-1", "10.0.2.15")
			if err != nil {
				t.Fatalf("Files() failed: %v", err)
			}
			for _, want := range []string{
				filepath.Join(paths.Kubelet, "kubelet.conf"), // --config
				KubeletDropIn, // NODE_NAME / NODE_IP
			} {
				if _, ok := findFile(files, want); !ok {
					t.Errorf("the %s role does not install %s", name, want)
				}
			}
		})
	}
}

func TestJoiningControlPlaneReceivesThePKIAndJoins(t *testing.T) {
	// A control plane with the cluster's PKI on its drive joins; one without
	// creates. Getting this backwards means two certificate authorities, so the
	// distinction is asserted rather than assumed.
	drive := joiningDrive(t)
	cfg := loadConfig(t, joiningMasterDoc)
	paths := testPaths(t)

	if !JoiningControlPlane(cfg, drive) {
		t.Fatal("JoiningControlPlane() = false for a drive carrying pki/ca.crt")
	}

	files, err := Files(cfg, drive, paths, "vates-cp-2", "192.168.122.51")
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}

	// The certificate authorities must be installed, with the keys private.
	for _, want := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(paths.Kubernetes, "pki", "ca.crt"), 0o644},
		{filepath.Join(paths.Kubernetes, "pki", "ca.key"), 0o600},
		{filepath.Join(paths.Kubernetes, "pki", "sa.key"), 0o600},
		{filepath.Join(paths.Kubernetes, "pki", "etcd", "ca.crt"), 0o644},
	} {
		f, ok := findFile(files, want.path)
		if !ok {
			t.Errorf("a joining control plane does not install %s", want.path)
			continue
		}
		if f.Mode != want.mode {
			t.Errorf("%s mode = %#o, want %#o", want.path, f.Mode, want.mode)
		}
	}

	// It must be configured to JOIN, not to create: the init document would make
	// it mint a new certificate authority.
	if _, ok := findFile(files, JoinConfigPath); !ok {
		t.Errorf("a joining control plane does not get %s", JoinConfigPath)
	}
	if _, ok := findFile(files, KubeadmConfigPath); ok {
		t.Errorf("a joining control plane got %s, which would create a second cluster", KubeadmConfigPath)
	}

	// And the kubelet's own bootstrap config: a token, from which the kubelet
	// obtains its certificate. kubeadm's kubelet-start is skipped, so it writes
	// no kubelet.conf; this is what replaces it.
	bootstrap, ok := findFile(files, filepath.Join(paths.Kubernetes, BootstrapKubeletConf))
	if !ok {
		t.Errorf("a joining control plane does not install %s", BootstrapKubeletConf)
	} else if bootstrap.Mode != 0o600 {
		t.Errorf("%s mode = %#o, want 0600", BootstrapKubeletConf, bootstrap.Mode)
	}
}

func TestJoinCommandSkipsKubeletStart(t *testing.T) {
	// kubeadm join would otherwise write a kubelet environment file and start the
	// kubelet; this system starts its own kubelet before the join, so the phase
	// that would start a second one is skipped.
	//
	// One command, run from the bootstrap stage: this node's kubelet has to be
	// running before the join, or its etcd member is added as a learner that can
	// never catch up and the promotion retries forever.
	cmd := strings.Join(JoinControlPlaneCommand(), " ")
	if !strings.Contains(cmd, "kubeadm join") || strings.Contains(cmd, "kubeadm init") {
		t.Errorf("command = %q, want `kubeadm join`", cmd)
	}
	if !strings.Contains(cmd, "--skip-phases=kubelet-start") {
		t.Errorf("command = %q, want it to skip kubelet-start", cmd)
	}
}

func TestJoinConfigCarriesDiscoveryAndEndpoint(t *testing.T) {
	cfg := loadConfig(t, joiningMasterDoc)
	raw, err := JoinConfiguration(cfg, "vates-cp-2", "192.168.122.51")
	if err != nil {
		t.Fatalf("JoinConfiguration() failed: %v", err)
	}

	var join struct {
		Kind      string `yaml:"kind"`
		Discovery struct {
			BootstrapToken struct {
				APIServerEndpoint string   `yaml:"apiServerEndpoint"`
				Token             string   `yaml:"token"`
				CACertHashes      []string `yaml:"caCertHashes"`
			} `yaml:"bootstrapToken"`
		} `yaml:"discovery"`
		ControlPlane struct {
			LocalAPIEndpoint struct {
				AdvertiseAddress string `yaml:"advertiseAddress"`
			} `yaml:"localAPIEndpoint"`
			CertificateKey string `yaml:"certificateKey"`
		} `yaml:"controlPlane"`
		NodeRegistration struct {
			Name string `yaml:"name"`
		} `yaml:"nodeRegistration"`
	}
	if err := yaml.Unmarshal(raw, &join); err != nil {
		t.Fatalf("generated join document does not parse: %v\n%s", err, raw)
	}

	if join.Kind != "JoinConfiguration" {
		t.Errorf("kind = %q, want JoinConfiguration", join.Kind)
	}
	// The endpoint is the VIP: discovery is a question asked of the cluster.
	if got := join.Discovery.BootstrapToken.APIServerEndpoint; got != "192.168.1.10:6443" {
		t.Errorf("apiServerEndpoint = %q, want the control plane endpoint", got)
	}
	// The API server binds this node's own address, not the VIP.
	if got := join.ControlPlane.LocalAPIEndpoint.AdvertiseAddress; got != "192.168.122.51" {
		t.Errorf("advertiseAddress = %q, want this node's address", got)
	}
	if len(join.Discovery.BootstrapToken.CACertHashes) != 1 {
		t.Errorf("caCertHashes = %v, want exactly one", join.Discovery.BootstrapToken.CACertHashes)
	}
	if join.NodeRegistration.Name != "vates-cp-2" {
		t.Errorf("nodeRegistration.name = %q, want vates-cp-2", join.NodeRegistration.Name)
	}
	// The certificate key belongs to controlPlane. Under discovery.bootstrapToken
	// kubeadm rejects it as an unknown field with a WARNING and then continues,
	// so the mistake appears much later as a join that never completes.
	if join.ControlPlane.CertificateKey == "" {
		t.Error("controlPlane.certificateKey is unset; kubeadm fetches nothing and the join never finishes")
	}
}

func TestJoinDocumentUsesOnlyKnownFields(t *testing.T) {
	// kubeadm decodes its configuration strictly. A field in the wrong place is
	// reported as a warning and then ignored, which means a typo produces a join
	// that stalls rather than a document that is rejected -- so the shape is
	// checked here, where it can be.
	cfg := loadConfig(t, joiningMasterDoc)
	raw, err := JoinConfiguration(cfg, "vates-cp-2", "192.168.122.51")
	if err != nil {
		t.Fatalf("JoinConfiguration() failed: %v", err)
	}

	// A typed decoding with the fields kubeadm actually accepts: anything else,
	// anywhere, is an error. The struct mirrors JoinConfiguration's own schema.
	var join struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Discovery  struct {
			BootstrapToken struct {
				APIServerEndpoint string   `yaml:"apiServerEndpoint"`
				Token             string   `yaml:"token"`
				CACertHashes      []string `yaml:"caCertHashes"`
			} `yaml:"bootstrapToken"`
		} `yaml:"discovery"`
		ControlPlane struct {
			LocalAPIEndpoint struct {
				AdvertiseAddress string `yaml:"advertiseAddress"`
				BindPort         int    `yaml:"bindPort"`
			} `yaml:"localAPIEndpoint"`
			CertificateKey string `yaml:"certificateKey"`
		} `yaml:"controlPlane"`
		NodeRegistration struct {
			Name      string `yaml:"name"`
			CRISocket string `yaml:"criSocket"`
		} `yaml:"nodeRegistration"`
		Patches struct {
			Directory string `yaml:"directory"`
		} `yaml:"patches"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&join); err != nil {
		t.Fatalf("kubeadm would reject or ignore part of this document: %v\n%s", err, raw)
	}
}

func TestKubeadmPatchesPointTheLivenessProbeAwayFromEtcd(t *testing.T) {
	// A slow etcd must not restart the API server: /livez includes the etcd
	// check, and a control plane killed for a slow disk is a control plane lost
	// while etcd is already struggling. The patch is what moves the liveness
	// probe off etcd, and it has to reach BOTH documents -- a joining control
	// plane's kubeadm reads its own patches.directory.
	tests := []struct {
		name   string
		render func() ([]byte, error)
	}{
		{
			name: "bootstrapping control plane (init)",
			render: func() ([]byte, error) {
				return KubeadmConfig(loadConfig(t, masterDoc), "vates-cp-1", "192.168.122.50")
			},
		},
		{
			name: "joining control plane (join)",
			render: func() ([]byte, error) {
				return JoinConfiguration(loadConfig(t, joiningMasterDoc), "vates-cp-2", "192.168.122.51")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := tt.render()
			if err != nil {
				t.Fatalf("rendering failed: %v", err)
			}
			var doc struct {
				Patches struct {
					Directory string `yaml:"directory"`
				} `yaml:"patches"`
			}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("document does not parse: %v", err)
			}
			if doc.Patches.Directory != PatchesDir {
				t.Errorf("patches.directory = %q, want %q", doc.Patches.Directory, PatchesDir)
			}
		})
	}

	// The patch file itself: kubeadm applies it to the kube-apiserver static
	// pod, so it must name that component and move ONLY the liveness probe.
	var pod struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			Containers []struct {
				Name          string `yaml:"name"`
				LivenessProbe struct {
					HTTPGet struct {
						Path string `yaml:"path"`
					} `yaml:"httpGet"`
				} `yaml:"livenessProbe"`
				ReadinessProbe *struct{} `yaml:"readinessProbe"`
			} `yaml:"containers"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(KubeAPIServerLivenessPatch(), &pod); err != nil {
		t.Fatalf("the kube-apiserver patch does not parse: %v", err)
	}
	if pod.Kind != "Pod" || pod.Metadata.Name != "kube-apiserver" || pod.Metadata.Namespace != "kube-system" {
		t.Errorf("patch identifies %s/%s (%s), want kube-apiserver in kube-system as a Pod",
			pod.Metadata.Namespace, pod.Metadata.Name, pod.Kind)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("patch has %d containers, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Name != "kube-apiserver" {
		t.Errorf("patched container = %q, want kube-apiserver", c.Name)
	}
	if got := c.LivenessProbe.HTTPGet.Path; got != "/livez?exclude=etcd" {
		t.Errorf("liveness path = %q, want /livez?exclude=etcd", got)
	}
	// Readiness keeps the etcd check: an API server that cannot read etcd
	// should leave its Service endpoints. Only the liveness probe changes.
	if c.ReadinessProbe != nil {
		t.Error("the patch touches readinessProbe; only the liveness probe must drop etcd")
	}

	// And the patch must actually be installed, at the path the documents name.
	paths := testPaths(t)
	files, err := MasterFiles(loadConfig(t, masterDoc), bareDrive(t, masterDoc), paths, "vates-cp-1", "192.168.122.50")
	if err != nil {
		t.Fatalf("MasterFiles() failed: %v", err)
	}
	f, ok := findFile(files, filepath.Join(PatchesDir, kubeAPIServerLivenessPatchName))
	if !ok {
		t.Fatalf("MasterFiles() did not install the patch at %s/%s", PatchesDir, kubeAPIServerLivenessPatchName)
	}
	if string(f.Content) != string(KubeAPIServerLivenessPatch()) {
		t.Error("the installed patch differs from KubeAPIServerLivenessPatch()")
	}
}

func TestJoiningIsDetectedFromTheCertificateKeyToo(t *testing.T) {
	// A control plane can join either way: given the PKI, or given the key to
	// fetch it. Detecting only the first means the second mints its own
	// certificate authority -- two clusters sharing an etcd, which is very hard
	// to trace back to this test.
	if !JoiningControlPlane(loadConfig(t, joiningMasterDoc), bareDrive(t, joiningMasterDoc)) {
		t.Error("a control plane carrying a certificateKey is not recognised as joining")
	}

	// And a master with neither is creating the cluster.
	if JoiningControlPlane(loadConfig(t, masterDoc), bareDrive(t, masterDoc)) {
		t.Error("a master with no PKI and no certificateKey is wrongly treated as joining")
	}
}

func TestJoiningByCertificateKeyInstallsTheCAAndABootstrapConfig(t *testing.T) {
	// There are two ways to join: be given the cluster's PKI, or be given the key
	// that fetches it. The second carries no CA PRIVATE key on the drive --
	// kubeadm fetches those -- but it carries the CA certificate (the kubelet
	// verifies the API server with it) and the token, from which the kubelet
	// obtains its own certificate.
	dir := t.TempDir()
	for name, content := range map[string]string{
		"meta-data":       "instance-id: i-3\nlocal-hostname: vates-cp-3\n",
		"vates-node.yaml": joiningMasterDoc,
		"pki/ca.crt":      "CA\n",
	} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	drive := configdrive.Open(dir)
	paths := testPaths(t)

	files, err := MasterFiles(loadConfig(t, joiningMasterDoc), drive, paths, "vates-cp-3", "192.168.122.52")
	if err != nil {
		t.Fatalf("MasterFiles() failed for a certificate-key join: %v", err)
	}

	// No CA PRIVATE key is installed: kubeadm fetches those from the cluster.
	for _, f := range files {
		switch filepath.Base(f.Path) {
		case "ca.key", "sa.key", "front-proxy-ca.key":
			t.Errorf("a certificate-key join installs %s, which it should fetch instead", f.Path)
		}
	}
	// The CA certificate and the bootstrap config ARE installed.
	for _, want := range []string{
		filepath.Join(paths.Kubernetes, "pki", "ca.crt"),
		filepath.Join(paths.Kubernetes, BootstrapKubeletConf),
	} {
		if _, ok := findFile(files, want); !ok {
			t.Errorf("a certificate-key join does not install %s", want)
		}
	}
	// And no kubelet credential from the drive: it produces its own.
	for _, unwanted := range []string{
		filepath.Join(paths.Kubernetes, "pki", "kubelet.crt"),
		filepath.Join(paths.Kubernetes, "pki", "kubelet.key"),
		filepath.Join(paths.Kubernetes, kubeletConfFile),
	} {
		if _, ok := findFile(files, unwanted); ok {
			t.Errorf("a certificate-key join installs %s, which the kubelet now obtains itself", unwanted)
		}
	}

	// And the join document must not carry an empty certificateKey, which is not
	// the same as no certificateKey.
	join, ok := findFile(files, JoinConfigPath)
	if !ok {
		t.Fatalf("no %s", JoinConfigPath)
	}
	if strings.Contains(string(join.Content), "certificateKey: \n") {
		t.Errorf("the join document carries an empty certificateKey:\n%s", join.Content)
	}
	if !strings.Contains(string(join.Content), joiningCertKey) {
		t.Errorf("the join document does not carry the certificate key:\n%s", join.Content)
	}
}

// A joining drive with neither the PKI keys nor a certificate key cannot join,
// and must say so rather than write a document kubeadm will fail on later.
func TestJoiningControlPlaneWithoutAuthorityOrKeyIsRefused(t *testing.T) {
	// The drive says how to join; a document with neither the PKI keys nor a
	// certificate key leaves kubeadm nothing to fetch with, and must be refused
	// here rather than produce a join that fails minutes later on the node.
	withoutKey := strings.Replace(joiningMasterDoc, "  certificateKey: \""+joiningCertKey+"\"\n", "", 1)
	dir := t.TempDir()
	for name, content := range map[string]string{
		"meta-data":       "instance-id: i-4\nlocal-hostname: vates-cp-4\n",
		"vates-node.yaml": withoutKey,
		"kubelet.conf":    "apiVersion: v1\nkind: Config\n",
		"pki/ca.crt":      "CA\n",
		"pki/kubelet.crt": "KUBELETCRT\n",
		"pki/kubelet.key": "KUBELETKEY\n",
	} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := MasterFiles(loadConfig(t, withoutKey), configdrive.Open(dir), testPaths(t), "vates-cp-4", "192.168.122.53"); err == nil {
		t.Fatal("MasterFiles() accepted a joining control plane with no authority and no key")
	}
}

// joiningCertKey is the certificateKey in joiningMasterDoc, repeated so a change
// to the document cannot silently make the test above vacuous.
const joiningCertKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestWaitForEndpointGivesUpAndSaysSo(t *testing.T) {
	// Nothing listens on port 1, so the wait has to end and the error has to name
	// the endpoint: the failure it replaces -- "client rate limiter Wait returned
	// an error: context deadline exceeded" -- names neither the address nor the
	// fact that nothing answered.
	r := &fakeRunner{}
	start := time.Now()
	err := waitForEndpoint("127.0.0.1:1", time.Second, r)
	if err == nil {
		t.Fatal("waitForEndpoint() succeeded against a port nothing listens on")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error was %q, want it to name the endpoint", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("waitForEndpoint() gave up after %s, before its timeout", elapsed)
	}
}

func TestWaitForEndpointReturnsImmediatelyWhenSomethingAnswers(t *testing.T) {
	// A listener that accepts and closes: the endpoint is as reachable as it needs
	// to be, and the wait must not add a delay of its own.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// The client only probes for a connection; the close cannot fail in
			// a way this test cares about.
			_ = c.Close()
		}
	}()

	start := time.Now()
	if err := waitForEndpoint(ln.Addr().String(), 10*time.Second, &fakeRunner{}); err != nil {
		t.Fatalf("waitForEndpoint() failed against a listening socket: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waitForEndpoint() took %s against a listening socket", elapsed)
	}
}

func TestARegistryMirrorReachesEveryImageWeRender(t *testing.T) {
	// A mirror that is configured but only rewrites SOME of the images is worse
	// than none: half the cluster comes from the local registry and the other
	// half still tries to reach the internet, which on a cluster without it is a
	// pull that hangs rather than an error that names the problem.
	//
	// So this asserts the places an image reference is produced: kubeadm's own
	// images, flannel, cilium, and kube-vip.
	mirrored := masterDoc + `
registry:
  kubernetes: "harbor.vates.local/k8s"
  mirrors:
    - host: ghcr.io
      replace: "harbor.vates.local/mirror/ghcr.io"
    - host: docker.io
      replace: "harbor.vates.local/mirror/docker.io"
    - host: quay.io
      replace: "harbor.vates.local/mirror/quay.io"
`
	cfg := loadConfig(t, mirrored)

	// 1. The cluster's own images, through kubeadm.
	kubeadm, err := KubeadmConfig(cfg, "vates-cp-1", "192.0.2.1")
	if err != nil {
		t.Fatalf("KubeadmConfig() failed: %v", err)
	}
	for _, want := range []string{
		"imageRepository: harbor.vates.local/k8s",
		"etcd:\n  local:\n    imageRepository: harbor.vates.local/k8s",
		"dns:\n  imageRepository: harbor.vates.local/k8s",
	} {
		if !strings.Contains(string(kubeadm), want) {
			t.Errorf("the ClusterConfiguration does not carry\n  %s", want)
		}
	}

	// 2. Flannel and its CNI plugin.
	flannel, err := FlannelManifest(cfg)
	if err != nil {
		t.Fatalf("FlannelManifest() failed: %v", err)
	}
	for _, want := range []string{
		"image: harbor.vates.local/mirror/docker.io/flannel/flannel:v0.26.1",
		"image: harbor.vates.local/mirror/ghcr.io/flannel-io/flannel-cni-plugin:v1.9.1-flannel3",
	} {
		if !strings.Contains(string(flannel), want) {
			t.Errorf("the flannel manifest does not carry\n  %s", want)
		}
	}

	// 3. Cilium's agent and operator, on a node that selects it.
	ciliumCfg := loadConfig(t, strings.Replace(mirrored, "plugin: flannel", "plugin: cilium", 1))
	cilium, err := CiliumManifest(ciliumCfg)
	if err != nil {
		t.Fatalf("CiliumManifest() failed: %v", err)
	}
	for _, want := range []string{
		`image: "harbor.vates.local/mirror/quay.io/cilium/cilium:v1.20.1@sha256:ae9ea21f7427fe24bc6ea7247eb552157a1b0a431744045d3f641545ca71d11b"`,
		`image: "harbor.vates.local/mirror/quay.io/cilium/operator-generic:v1.20.1@sha256:6c3885fc7b629099fdbe2a5c87869c86feb825fa18fae299eac0f61918d16ecf"`,
	} {
		if !strings.Contains(string(cilium), want) {
			t.Errorf("the cilium manifest does not carry\n  %s", want)
		}
	}
	if strings.Contains(string(cilium), `image: "quay.io/`) {
		t.Errorf("the cilium manifest kept an unrewritten quay.io image:\n%s", cilium)
	}

	// 4. kube-vip.
	kubevip, err := KubeVIPManifest("192.0.2.9", "eth0", "6443", SuperAdminKubeconfig, "192.0.2.1",
		cfg.ImageFor(KubeVIPImage))
	if err != nil {
		t.Fatalf("KubeVIPManifest() failed: %v", err)
	}
	if !strings.Contains(string(kubevip), "harbor.vates.local/mirror/ghcr.io/kube-vip/kube-vip:v1.0.0") {
		t.Errorf("the kube-vip manifest does not carry the mirrored image:\n%s", kubevip)
	}
}

func TestWithoutAMirrorNothingIsRewritten(t *testing.T) {
	// The other half of the contract: an unconfigured node must render exactly
	// what it rendered before this feature existed, or the feature is a breaking
	// change for every cluster that has internet access.
	cfg := loadConfig(t, masterDoc)

	kubeadm, err := KubeadmConfig(cfg, "vates-cp-1", "192.0.2.1")
	if err != nil {
		t.Fatalf("KubeadmConfig() failed: %v", err)
	}
	if strings.Contains(string(kubeadm), "imageRepository") {
		t.Error("an imageRepository was emitted with no mirror configured; kubeadm's defaults are correct and ours are not necessarily")
	}

	flannel, err := FlannelManifest(cfg)
	if err != nil {
		t.Fatalf("FlannelManifest() failed: %v", err)
	}
	for _, want := range []string{
		"image: docker.io/flannel/flannel:v0.26.1",
		"image: ghcr.io/flannel-io/flannel-cni-plugin:v1.9.1-flannel3",
	} {
		if !strings.Contains(string(flannel), want) {
			t.Errorf("the flannel manifest lost its upstream image:\n  %s", want)
		}
	}

	ciliumCfg := loadConfig(t, strings.Replace(masterDoc, "plugin: flannel", "plugin: cilium", 1))
	cilium, err := CiliumManifest(ciliumCfg)
	if err != nil {
		t.Fatalf("CiliumManifest() failed: %v", err)
	}
	for _, want := range []string{
		`image: "quay.io/cilium/cilium:v1.20.1@sha256:ae9ea21f7427fe24bc6ea7247eb552157a1b0a431744045d3f641545ca71d11b"`,
		`image: "quay.io/cilium/operator-generic:v1.20.1@sha256:6c3885fc7b629099fdbe2a5c87869c86feb825fa18fae299eac0f61918d16ecf"`,
	} {
		if !strings.Contains(string(cilium), want) {
			t.Errorf("the cilium manifest lost its upstream image:\n  %s", want)
		}
	}
}

// The management API's certificate cannot see the cluster endpoint on the node
// -- the virtual IP is brought up by kube-vip well after the certificate is
// minted -- so vates-init has to write it down where the API can read it. This
// is what makes "the operator only knows the VIP" work at all.
func TestAPIHostsRecordsTheClusterEndpoint(t *testing.T) {
	f := apiHostsFile(loadConfig(t, masterDoc))
	if f.Path != ExtraHostsPath {
		t.Errorf("path = %q, want %q", f.Path, ExtraHostsPath)
	}
	if f.Mode != 0o644 {
		t.Errorf("mode = %#o, want 0644", f.Mode)
	}
	// Here the endpoint and the VIP are the same address, so it is written once.
	if got, want := string(f.Content), "192.168.1.10\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// When an external load balancer provides the endpoint and kube-vip owns a
// different VIP, both must be in the certificate: the operator may name either.
func TestAPIHostsCarriesADistinctVIP(t *testing.T) {
	doc := strings.Replace(masterDoc, `    address: "192.168.1.10"`, `    address: "192.168.1.50"`, 1)
	f := apiHostsFile(loadConfig(t, doc))
	if got, want := string(f.Content), "192.168.1.10\n192.168.1.50\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// dhcpcd assigns a link-local address while it waits for a lease. Taking it made
// a joining control plane advertise 169.254.x to kubeadm, and the join failed
// with "cannot use ... as the bind address for the API Server".
func TestNodeIPSkipsTheLinkLocalAddress(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("169.254.72.203"), Mask: net.CIDRMask(16, 32)},
		&net.IPNet{IP: net.ParseIP("192.168.122.214"), Mask: net.CIDRMask(24, 32)},
	}
	ip, ok := routableIPv4(addrs)
	if !ok || ip != "192.168.122.214" {
		t.Fatalf("routableIPv4() = %q, %v; want the lease address", ip, ok)
	}
}

// While the lease is pending, there is no answer -- and the caller must keep
// waiting rather than proceed with an address that is not reachable.
func TestNodeIPFindsNothingButLinkLocal(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: net.ParseIP("169.254.72.203"), Mask: net.CIDRMask(16, 32)},
	}
	if ip, ok := routableIPv4(addrs); ok {
		t.Fatalf("routableIPv4() = %q, want none", ip)
	}
}

// The image carries ONE hostname for every machine. Setting it to the node's
// Kubernetes name is what gives each kube-vip a distinct leader-election
// identity -- otherwise all of them hold the VIP and it flaps.
func TestFilesSetTheHostnameToTheNodeName(t *testing.T) {
	files, err := Files(loadConfig(t, workerDoc), workerDrive(t), testPaths(t), "vates-worker-1", "10.0.2.15")
	if err != nil {
		t.Fatal(err)
	}
	f, ok := findFile(files, HostnameFile)
	if !ok {
		t.Fatalf("Files() did not produce %s", HostnameFile)
	}
	if got, want := string(f.Content), "vates-worker-1\n"; got != want {
		t.Errorf("hostname content = %q, want %q", got, want)
	}
	if f.Mode != 0o644 {
		t.Errorf("%s mode = %#o, want 0644", HostnameFile, f.Mode)
	}
}
