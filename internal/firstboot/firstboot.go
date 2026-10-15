package firstboot

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/configdrive"
	"github.com/vatesfr/vates-kube-os/vatescfg"
)

// Paths are the filesystem locations the node's configuration lives in. They
// are a struct rather than constants so tests can redirect them into a temp
// directory.
type Paths struct {
	// Kubernetes is where kubeconfigs and PKI go. The provider's kubelet.conf
	// refers to /etc/kubernetes/pki/{ca.crt,kubelet.crt,kubelet.key} by
	// absolute path, so this must not move without changing it too.
	Kubernetes string
	// Kubelet holds the KubeletConfiguration consumed by --config.
	Kubelet string
}

// DefaultPaths is the layout baked into the image.
func DefaultPaths() Paths {
	return Paths{
		Kubernetes: "/etc/kubernetes",
		Kubelet:    "/etc/kubelet",
	}
}

// File is a file to install, with its mode. Keeping the mode in the value rather
// than applying it afterwards means a private key cannot be written world-
// readable even momentarily.
type File struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// NodeIP returns the interface's first IPv4 address.
//
// The node's address is used as the kubelet's --node-ip. It must be the address
// the API server will reach it on. Guessing from a route or from the first
// non-loopback interface would pick the wrong one on a multi-homed machine,
// which is why the interface is named explicitly in vates-node.yaml.
func NodeIP(iface string) (string, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return "", fmt.Errorf("interface %s: %w", iface, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", fmt.Errorf("interface %s: %w", iface, err)
	}
	if ip, ok := routableIPv4(addrs); ok {
		return ip, nil
	}
	return "", fmt.Errorf("interface %s has no routable IPv4 address yet", iface)
}

// routableIPv4 picks the first usable IPv4 address, skipping link-local ones.
//
// The skip is the whole point. While dhcpcd waits for a lease it assigns an
// IPv4LL address (169.254/16), and taking the first IPv4 address that exists
// meant a joining control plane advertised 169.254.x to kubeadm. The join then
// failed with
//
//	cannot use "169.254.72.203" as the bind address for the API Server
//
// which names the address and not the race that produced it. Waiting for the
// lease is exactly what this function's caller is for; returning a link-local
// address defeats that wait.
func routableIPv4(addrs []net.Addr) (string, bool) {
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipnet.IP.To4()
		if v4 == nil || v4.IsLinkLocalUnicast() || v4.IsLoopback() || v4.IsUnspecified() {
			continue
		}
		return v4.String(), true
	}
	return "", false
}

// WaitNodeIP polls for the interface's IPv4 address until it appears.
//
// Waiting is necessary, not defensive: vates-init is ordered after
// network-online.target, but that target means the network is up, not that DHCP
// has finished. On the first boot the lease is usually still being acquired, and
// failing there would fail the unit on a node that is perfectly healthy -- just
// a second early.
//
// It polls for the condition rather than sleeping a fixed time, so a fast node
// is not slowed down and a slow one is not cut short.
func WaitNodeIP(iface string, timeout time.Duration, logf func(string, ...any)) (string, error) {
	return waitForIP(func() (string, error) { return NodeIP(iface) }, iface, timeout, logf)
}

// waitForIP is the polling loop, with the address lookup injected so it can be
// tested without a network interface that misbehaves on command.
func waitForIP(get func() (string, error), iface string, timeout time.Duration, logf func(string, ...any)) (string, error) {
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		ip, err := get()
		if err == nil {
			if attempt > 0 && logf != nil {
				logf("address on %s appeared: %s", iface, ip)
			}
			return ip, nil
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("no IPv4 address on %s after %s: %w", iface, timeout, err)
		}
		if attempt == 0 && logf != nil {
			logf("waiting for an IPv4 address on %s (DHCP may not have finished)", iface)
		}
		time.Sleep(time.Second)
	}
}

// KubeletRootDir is where the kubelet keeps its state. Nothing else creates it,
// and the kubelet refuses to start without it.
const KubeletRootDir = "/var/lib/kubelet"

// KubeletDropIn is where the per-node environment for the kubelet unit is
// written.
const KubeletDropIn = "/etc/systemd/system/k8s-kubelet.service.d/10-node.conf"

// KubeletDropInFile renders the drop-in that gives the kubelet its per-node
// values.
//
// There is no systemd here: PID 1 reads this file itself (see internal/sysinit)
// and turns the values into the kubelet's command line and environment. Keeping
// it a systemd-style drop-in is what lets the unit be static and identical on
// every machine, with only the machine's values arriving from the config drive:
//
//	NODE_NAME, NODE_IP           --hostname-override, --node-ip on the kubelet
//	KUBERNETES_VERSION           the version the launcher fetches
//	KUBERNETES_BINARY_BASE       where it fetches it from
//	KUBELET_BOOTSTRAP_KUBECONFIG the kubelet's --bootstrap-kubeconfig
//	CLOUD_PROVIDER               the kubelet's --cloud-provider, when set
//
// bootstrapKubeconfig is EMPTY on the node that creates the cluster, and a path
// on a node that joins. The first control plane has no token until its own
// bootstrap phase runs -- after the kubelet has started -- so it cannot
// bootstrap, and uses the kubelet.conf kubeadm wrote for it. A joining node has
// its token from the config drive, so its kubelet obtains its own certificate.
//
// cloudProvider is EMPTY unless vates-node.yaml asks for an external CCM; when
// it is set, PID 1 turns it into --cloud-provider on the kubelet. The line is
// omitted when empty so that an unconfigured node reads exactly as before.
func KubeletDropInFile(nodeName, nodeIP, k8sVersion, binaryBase, bootstrapKubeconfig, cloudProvider string) []byte {
	var cloud string
	if cloudProvider != "" {
		cloud = "Environment=CLOUD_PROVIDER=" + cloudProvider + "\n"
	}
	return fmt.Appendf(nil, `[Service]
# Written by vates-init and read by PID 1, the only thing that parses it (there
# is no systemd). Each Environment line becomes a flag or an environment
# variable of the kubelet process.
Environment=NODE_NAME=%s
Environment=NODE_IP=%s
Environment=KUBERNETES_VERSION=%s
Environment=KUBERNETES_BINARY_BASE=%s
Environment=KUBELET_BOOTSTRAP_KUBECONFIG=%s
%s`, nodeName, nodeIP, k8sVersion, binaryBase, bootstrapKubeconfig, cloud)
}

// ConsoleDropIn is where the console's mode is written.
//
// A drop-in for the same reason the kubelet's values are one: the unit is static
// and identical on every machine, and what differs between machines arrives from
// the config drive. The mode is the machine's, not the image's.
const ConsoleDropIn = "/etc/systemd/system/vates-console.service.d/10-mode.conf"

// ConsoleDropInFile renders the drop-in that tells the console what to show.
func ConsoleDropInFile(mode string) []byte {
	return fmt.Appendf(nil, `[Service]
# Written by vates-init, from dashboard.mode in vates-node.yaml.
#
# Read by /usr/local/bin/vates-console, which is the unit's ExecStart. The value
# reaches the script through its own environment, and systemd is what puts it
# there.
Environment=DASHBOARD_MODE=%s
`, mode)
}

// kubeletConfiguration is the KubeletConfiguration written to --config.
//
// Two keys that older Kubernetes accepted are deliberately absent:
// hostnameOverride and remote (the container runtime). Both were removed from
// KubeletConfiguration in v1.31. Setting them makes strict decoding fail with
// "unknown field", after which the kubelet falls back to lenient decoding and
// silently DROPS them -- a configuration that appears applied and is not.
// hostnameOverride is passed as a command line flag by the unit instead.
//
// The API server credentials are likewise not here. In v1.31
// authentication.webhook.kubeConfigFile and authorization.webhook.* no longer
// exist; the kubelet takes the kubeconfig through --kubeconfig. Without that
// flag it starts in "Standalone mode, no API client" and dies with "no client
// provided, cannot use webhook authentication".
// kubeletConfiguration renders the KubeletConfiguration written to --config.
//
// The document lives in templates/kubelet-config.yaml.tmpl. The comment there
// records which keys are deliberately absent and why, since those omissions are
// the parts most likely to be "fixed" back in by someone reading it later.
func kubeletConfiguration(criEndpoint, clusterDNS, dnsDomain string) ([]byte, error) {
	return render("kubelet-config.yaml.tmpl", struct {
		CRIEndpoint  string
		ClusterDNS   string
		DNSDomain    string
		CgroupDriver string
	}{criEndpoint, clusterDNS, dnsDomain, cgroupDriver()})
}

// cgroupDriver is the cgroup driver the kubelet must use: cgroupfs.
//
// Not systemd, and not a runtime check: this image has no systemd at all
// (`vates` is PID 1), so cgroupfs is the only driver, and it has
// to match containerd's -- which is cgroupfs here, its config setting no
// SystemdCgroup. The kubelet's cgroupfs driver runs on the cgroup v2 unified
// hierarchy this image mounts (internal/app/pid1.go, mountCgroupV2).
//
// It used to stat /run/systemd/system and answer systemd when that existed. That
// path exists on a systemd BUILD HOST, so the generated configuration -- and the
// test that checked it -- depended on where the build ran rather than on what the
// node is. On the node it always answered cgroupfs; the test only agreed because
// the host had systemd.
func cgroupDriver() string { return "cgroupfs" }

// CRIEndpoint is where containerd serves the CRI. The kubelet talks to it as a
// host process, so this is the host path.
const CRIEndpoint = "unix:///run/containerd/containerd.sock"

// Names repeated across this package's command builders. Naming them once keeps
// a typo in one builder from disagreeing with the others: the pki paths must
// match both the provider's drive and kubeadm's expectations.
const (
	pkiCACert = "pki/ca.crt"
	pkiCAKey  = "pki/ca.key"
)

// Files computes every file the node needs, without writing anything.
//
// The two roles differ in where their credentials come from, which is the whole
// of the difference:
//
//   - a worker is fully described by the config drive. The provider has already
//     issued its client certificate, so there is no join and no certificate
//     authority work: the kubelet authenticates with the kubeconfig it is given.
//   - a bootstrapping master mints the cluster's certificate authority itself,
//     through kubeadm. Its files are therefore the document kubeadm reads and
//     the static pod manifests, not any credential.
func Files(cfg *vatescfg.Config, drive *configdrive.Drive, paths Paths, nodeName, nodeIP string) ([]File, error) {
	var (
		files []File
		err   error
	)
	switch cfg.Role {
	case vatescfg.RoleWorker:
		files, err = workerFiles(cfg, drive, paths, nodeName, nodeIP)
	case vatescfg.RoleMaster:
		files, err = MasterFiles(cfg, drive, paths, nodeName, nodeIP)
	default:
		return nil, fmt.Errorf("role %q is not master or worker; it should have been rejected when the configuration was read", cfg.Role)
	}
	if err != nil {
		return nil, err
	}
	files = append(files, apiCAFiles(cfg, drive)...)
	files = append(files, apiHostsFile(cfg), hostnameFile(nodeName), apiPortDropIn(cfg))
	return files, nil
}

// APIPortDropIn is where the management API's port is recorded, so the unit can
// be told it without the image knowing.
const APIPortDropIn = "/etc/systemd/system/vates-api.service.d/10-port.conf"

// apiPortDropIn tells vates-api.service which port to listen on.
func apiPortDropIn(cfg *vatescfg.Config) File {
	return File{
		Path: APIPortDropIn,
		Mode: 0o644,
		Content: fmt.Appendf(nil, `[Service]
# Written by vates-init, from api.port in vates-node.yaml.
Environment=VATES_API_PORT=%d
`, cfg.APIPort()),
	}
}

// HostnameFile is where the OS hostname is recorded.
//
// Under /var, not /etc: the root is read-only, and this file is written by
// vates-init and read by PID 1 to set the kernel hostname. The /etc symlinks
// exist for paths an outside tool names by absolute path -- kubeadm's kubeconfig
// directory, the kubelet's config. This one is only ever named here, so it moves
// outright rather than through a symlink.
const HostnameFile = "/var/lib/vates/etc/hostname"

// hostnameFile sets the OS hostname to the Kubernetes node name.
//
// They are not the same by default: the image is built with ONE hostname for
// every machine ("vates-build"), and the node name is per-machine. That matters
// beyond tidiness -- kube-vip names its leader-election lock after os.Hostname(),
// so with one hostname for the whole cluster every kube-vip believes it holds
// the lock, every control plane advertises the virtual IP, and the VIP flaps
// between them. Seen from the host as an ARP entry that changes owner every
// couple of seconds, and from vateskctl as a different node answering each time.
func hostnameFile(nodeName string) File {
	return File{Path: HostnameFile, Mode: 0o644, Content: []byte(nodeName + "\n")}
}

// The management API's filesystem paths that firstboot writes and the API
// service reads. They live here, with the code that creates the files, rather
// than in internal/api: firstboot must not import the service, and the API can
// import firstboot.
const (
	// OperatorCAPath and OperatorCAKeyPath are the OPTIONAL certificate
	// authority supplied by the operator, which then governs the whole API: it
	// signs the server certificate AND verifies client certificates. Absent, the
	// cluster CA is used instead.
	OperatorCAPath    = "/etc/vates/api-ca.crt"
	OperatorCAKeyPath = "/etc/vates/api-ca.key"

	// ExtraHostsPath lists, one host per line, the names the server certificate
	// must also be valid for. See apiHostsFile.
	ExtraHostsPath = "/etc/vates/api-hosts"
)

// apiHostsFile tells the management API's certificate which additional names it
// must be valid for: the cluster endpoint, which is how an operator knows the
// cluster, and the virtual IP when it differs.
//
// Neither is an address of this node at the moment the certificate is minted --
// kube-vip brings the VIP up later -- which is exactly why it has to be written
// down here, from vates-node.yaml, rather than discovered from the interfaces.
func apiHostsFile(cfg *vatescfg.Config) File {
	var b strings.Builder
	host := cfg.EndpointHost()
	b.WriteString(host)
	b.WriteString("\n")
	if vip := cfg.Cluster.VIP.Address; vip != "" && vip != host {
		b.WriteString(vip)
		b.WriteString("\n")
	}
	return File{Path: ExtraHostsPath, Mode: 0o644, Content: []byte(b.String())}
}

// apiCAFiles installs the optional operator-supplied certificate authority.
//
// A node that generated its own cluster CA -- kubeadm does, on a bootstrapping
// control plane -- cannot hand that CA to an operator, so the operator could
// never authenticate to the API. Instead the operator generates a CA of its own,
// has the certificate AND its key injected here, and keeps the key. The node
// mints its server certificate from it, so one authority covers both directions
// of mutual TLS: the operator verifies the node, the node verifies the operator.
// The files are simply absent when no such CA was provided (the CAPI case).
func apiCAFiles(cfg *vatescfg.Config, drive *configdrive.Drive) []File {
	var files []File
	for _, f := range []struct {
		onDrive string
		path    string
		fromDoc string
		mode    os.FileMode
	}{
		{onDrive: "api-ca.crt", path: OperatorCAPath, fromDoc: cfg.PKI.APICA.Cert, mode: 0o644},
		{onDrive: "api-ca.key", path: OperatorCAKeyPath, fromDoc: cfg.PKI.APICA.Key, mode: 0o600},
	} {
		// The document wins when it carries the material: on the CAPI path it is
		// the only channel. The drive files are the direct-drive path.
		if f.fromDoc != "" {
			files = append(files, File{Path: f.path, Mode: f.mode, Content: ensureNewline(f.fromDoc)})
			continue
		}
		if !drive.Has(f.onDrive) {
			continue
		}
		content, err := drive.File(f.onDrive)
		if err != nil {
			continue
		}
		files = append(files, File{Path: f.path, Mode: f.mode, Content: content})
	}
	return files
}

// ensureNewline gives a PEM from the document a trailing newline, which a YAML
// block scalar usually keeps but a hand-written string may not. A PEM without
// one is parsed by some tools and not others; normalising here removes the
// difference.
func ensureNewline(s string) []byte {
	if strings.HasSuffix(s, "\n") {
		return []byte(s)
	}
	return []byte(s + "\n")
}

// clusterCACert is the cluster CA certificate, from the document when the
// provider stated it (the CAPI path), otherwise from the config drive (the
// direct-drive path).
func clusterCACert(cfg *vatescfg.Config, drive *configdrive.Drive) ([]byte, error) {
	if cfg.PKI.ClusterCA.Cert != "" {
		return ensureNewline(cfg.PKI.ClusterCA.Cert), nil
	}
	return drive.File(pkiCACert)
}

// kubeletConfFile is the file name Kubernetes gives to both the kubelet's
// kubeconfig and, reused, the KubeletConfiguration this project passes through
// --config. They live in different directories and mean different things, but
// the file is called kubelet.conf in both cases, so the name is written once.
const kubeletConfFile = "kubelet.conf"

// BootstrapKubeletConf is the kubeconfig the kubelet presents its TOKEN with,
// before it has a certificate of its own. The kubelet writes the resulting
// kubelet.conf itself.
const BootstrapKubeletConf = "bootstrap-kubelet.conf"

// bootstrapKubeconfigPath is what the kubelet unit is told to bootstrap from, or
// "" on the node that creates the cluster (which has no token until after its
// kubelet has started).
const bootstrapKubeconfigPath = "/etc/kubernetes/" + BootstrapKubeletConf

// bootstrapKubeletConfFile renders the token-based kubeconfig a joining node's
// kubelet bootstraps from.
//
// This is what TLS bootstrap buys: the node generates its own key and asks the
// API server for a certificate, so no operator mints one and no private key is
// ever transported. The token is the one in vates-node.yaml -- the same one
// kubeadm's discovery uses, and the bootstrap-token RBAC on the cluster already
// lets it request a kubelet certificate without anyone approving it by hand.
func bootstrapKubeletConfFile(cfg *vatescfg.Config, paths Paths) File {
	return File{
		Path: filepath.Join(paths.Kubernetes, BootstrapKubeletConf),
		Mode: 0o600,
		Content: fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority: /etc/kubernetes/pki/ca.crt
    server: https://%s
  name: vates
contexts:
- context:
    cluster: vates
    user: kubelet-bootstrap
  name: vates
current-context: vates
users:
- name: kubelet-bootstrap
  user:
    token: %s
`, cfg.Cluster.ControlPlaneEndpoint, cfg.Cluster.Token),
	}
}

// kubeletConfigFile is the KubeletConfiguration passed to the kubelet through
// --config, which BOTH roles need.
//
// It is easy to think kubeadm covers this on a control plane, because its
// kubeconfig phase writes /etc/kubernetes/kubelet.conf -- but that is the
// kubeconfig the kubelet authenticates WITH, not the settings it reads. Without
// this file the kubelet dies immediately with "failed to read kubelet config
// file /etc/kubelet/kubelet.conf: no such file or directory", which was exactly
// the master's first failure.
func kubeletConfigFile(cfg *vatescfg.Config, paths Paths) (File, error) {
	content, err := kubeletConfiguration(CRIEndpoint, cfg.DNSServiceIP(), cfg.Cluster.DNSDomain)
	if err != nil {
		return File{}, err
	}
	return File{
		Path:    filepath.Join(paths.Kubelet, kubeletConfFile),
		Mode:    0o644,
		Content: content,
	}, nil
}

// workerFiles is the worker half of Files.
//
// A worker's kubelet obtains its OWN certificate, through TLS bootstrap: it is
// given the cluster CA and a bootstrap token, and it asks the API server for a
// certificate. Nothing here signs anything, and no private key is transported.
func workerFiles(cfg *vatescfg.Config, drive *configdrive.Drive, paths Paths, nodeName, nodeIP string) ([]File, error) {
	var files []File

	ca, err := clusterCACert(cfg, drive)
	if err != nil {
		return nil, fmt.Errorf("worker node requires the cluster CA certificate (pki.clusterCA.cert in vates-node.yaml, or %s on the config drive): %w", pkiCACert, err)
	}
	files = append(files, File{
		Path:    filepath.Join(paths.Kubernetes, "pki", "ca.crt"),
		Mode:    0o644,
		Content: ca,
	})
	files = append(files, bootstrapKubeletConfFile(cfg, paths))

	kubeletConf, err := kubeletConfigFile(cfg, paths)
	if err != nil {
		return nil, err
	}

	files = append(files,
		kubeletConf,
		File{
			Path:    ConsoleDropIn,
			Mode:    0o644,
			Content: ConsoleDropInFile(string(cfg.DashboardModeValue())),
		},
		File{
			Path: KubeletDropIn,
			Mode: 0o644,
			Content: KubeletDropInFile(nodeName, nodeIP, cfg.Kubernetes.Version,
				cfg.BinaryBase(), bootstrapKubeconfigPath, cfg.Cloud.Provider),
		},
	)

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// Runner performs the side effects Apply needs. It is an interface so tests can
// assert on exactly which commands run, rather than installing a system.
type Runner interface {
	// MkdirAll creates a directory and its parents.
	MkdirAll(path string, mode os.FileMode) error
	// WriteFile installs content at path with mode, creating parents.
	WriteFile(path string, mode os.FileMode, content []byte) error
	// Stat reports a path's file information. A missing path is os.ErrNotExist.
	Stat(path string) (os.FileInfo, error)
	// Run executes a command, returning its combined output.
	Run(name string, args ...string) ([]byte, error)
	// Logf records a human-readable line.
	Logf(format string, args ...any)
	// Progress publishes what the node is doing, for the console's activity
	// line. Unlike Logf, it REPLACES the previous progress: it is a state, not
	// a history.
	Progress(text string)
}

// BinariesDir is where the launcher caches fetched Kubernetes binaries.
//
// On the host, under /var: the image is fixed and identical everywhere, and what
// it RUNS is decided per machine. The kubelet, kubeadm and kubectl all go through
// the launcher, so a version is fetched once for all of them.
//
// It has to exist and be labelled for the pod containers that read it.
const BinariesDir = "/var/lib/vates/kubernetes"

// BootstrappedMarker is written once a bootstrapping control plane has run its
// kubeadm phases successfully. Its presence is what keeps vates-init idempotent
// across reboots: the phases are not safe to repeat, and both k8s-node.target
// and vates-api.service Require= vates-init, so a failure here stops the kubelet
// AND the management API -- observed as a node that pings but runs no cluster.
const BootstrappedMarker = "/var/lib/vates/bootstrapped"

func bootstrapped(r Runner) bool {
	_, err := r.Stat(BootstrappedMarker)
	return err == nil
}

// BootstrapDoneMarker is written once the cluster-wide bootstrap stage has
// succeeded: the join for a joining control plane, the addons and the CNI for
// the one that creates the cluster.
//
// It is a different marker from BootstrappedMarker on purpose. That one guards
// the kubeadm INIT phases, which Configure runs before the kubelet exists; this
// one guards the stage that runs after the API server answers. A reboot must
// skip both, and a failure in either must be retried whole. It lives in /var,
// like the other, so an A/B update -- which replaces the system, not the
// machine's state -- does not undo it.
//
// Measured, not assumed: without this, a joining control plane that reboots
// re-runs `kubeadm join` and fails pre-flight for about a minute; see Bootstrap.
const BootstrapDoneMarker = "/var/lib/vates/bootstrap-done"

func bootstrapDone(r Runner) bool {
	_, err := r.Stat(BootstrapDoneMarker)
	return err == nil
}

// Apply writes the files and prepares the node; PID 1 starts the kubelet once
// this returns.
//
// The order matters: everything the kubelet needs is on disk before it starts.
func Apply(cfg *vatescfg.Config, drive *configdrive.Drive, paths Paths, nodeName, nodeIP string, r Runner) error {
	files, err := Files(cfg, drive, paths, nodeName, nodeIP)
	if err != nil {
		return err
	}

	// Every directory the kubelet or a static pod writes into must exist first:
	// a hostPath that is missing is a pod that never starts. Nothing else
	// creates them.
	for _, dir := range RequiredDirs(cfg) {
		if err := r.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	for _, f := range files {
		if err := r.WriteFile(f.Path, f.Mode, f.Content); err != nil {
			return fmt.Errorf("installing %s: %w", f.Path, err)
		}
	}
	r.Logf("installed %d file(s)", len(files))

	// The kubelet's drop-in was just written, and systemd does not notice a new
	// drop-in until it is told to re-read unit files. Without this the kubelet
	// starts with the unit as systemd saw it at boot -- with no NODE_NAME or
	// NODE_IP -- and fails on an unset variable.
	//
	// daemon-reload is safe to run from inside a unit: it re-reads unit files and
	// does not start, stop or restart anything, so it cannot deadlock against the
	// ordering that put this unit here.
	//
	// On a system without systemd (where `vates` is PID 1)
	// there is nothing to reload: the kubelet is started by PID 1 itself, after
	// this returns. Absence of systemctl is therefore expected, not an error.
	if _, err := exec.LookPath("systemctl"); err == nil {
		if _, err := r.Run("systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("reloading systemd after writing the kubelet drop-in: %w", err)
		}
		r.Logf("systemd reloaded to pick up the kubelet drop-in")
	} else {
		r.Logf("no systemd; the kubelet is started by PID 1")
	}

	// A bootstrapping control plane has its certificates and its static pod
	// manifests generated here, through kubeadm, before the kubelet is started.
	//
	// The ordering is forced, not a preference: the kubelet reads the manifests
	// directory when it starts, and the API server it launches is reachable at
	// the VIP, which kube-vip provides. Generating afterwards would mean starting
	// a kubelet that has nothing to run.
	if cfg.Role == vatescfg.RoleMaster {
		switch {
		case JoiningControlPlane(cfg, drive):
			// Nothing of kubeadm runs here. A control plane joining an existing
			// cluster runs its join from the bootstrap stage, once this node's
			// kubelet is up -- see JoinControlPlaneCommand for why that order is
			// forced rather than preferred.
			r.Logf("control plane joining: the join itself runs after the kubelet starts")
		case bootstrapped(r):
			// vates-init runs on EVERY boot, and k8s-node.target and the
			// management API both Require= it -- so re-running the kubeadm
			// phases here would fail on the second boot and take the kubelet and
			// the API down with it. The marker makes this idempotent.
			r.Logf("control plane already bootstrapped (%s): skipping the kubeadm init phases", BootstrappedMarker)
		default:
			for _, phase := range KubeadmPhases {
				start := time.Now()
				args := KubeadmPhaseCommand(phase)
				if _, err := r.Run(args[0], args[1:]...); err != nil {
					return fmt.Errorf("kubeadm phase %q: %w", phase, err)
				}
				r.Logf("kubeadm init phase %s (took %s)", phase, elapsed(start))
			}
			// Written only once every phase has succeeded, so an interrupted
			// bootstrap is retried whole rather than half-skipped.
			if err := r.WriteFile(BootstrappedMarker, 0o600, []byte(cfg.Kubernetes.Version+"\n")); err != nil {
				return fmt.Errorf("writing %s: %w", BootstrappedMarker, err)
			}
		}
	}

	// Nothing to point at a version: the image carries a launcher, and the version
	// travels in the environment. PID 1 starts the kubelet when this returns, and
	// the launcher fetches Kubernetes on its first run.
	r.Logf("the launcher will fetch Kubernetes %s", cfg.Kubernetes.Version)

	// Labelling comes after the kubeadm phases, because those are what create
	// the files under /etc/kubernetes that the containers will read.
	if err := labelForContainers(cfg, r); err != nil {
		return err
	}

	// Nothing is started from here, deliberately: PID 1 starts the kubelet once
	// Configure returns (see internal/sysinit). Starting it here would race the
	// process that is already about to.
	r.Logf("configuration complete; PID 1 will start the kubelet")
	return nil
}
