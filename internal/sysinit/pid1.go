package sysinit

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/configdrive"
	"github.com/vatesfr/vates-kube-os/internal/console/tty"
	"github.com/vatesfr/vates-kube-os/internal/dashboard"
	"github.com/vatesfr/vates-kube-os/internal/firstboot"
	"github.com/vatesfr/vates-kube-os/internal/hostinfo"
	"github.com/vatesfr/vates-kube-os/internal/update/ab"
)

// pid1Mount is one entry of the pseudo-filesystem table.
type pid1Mount struct {
	source string
	target string
	fstype string
	data   string
	flags  uintptr
}

// pid1Mounts is what has to be mounted before containerd, the kubelet and the
// console can work. Order matters: /dev before /dev/pts and /dev/shm, /sys
// before /sys/fs/cgroup and /sys/fs/bpf.
var pid1Mounts = []pid1Mount{
	{"proc", "/proc", "proc", "", 0},
	{"sysfs", "/sys", "sysfs", "", 0},
	// bpffs, for Cilium. Its agent pins its BPF maps and programs under
	// /sys/fs/bpf, and it expects the filesystem to be there: without it the
	// agent's first pin fails. A distribution mounts it at boot (systemd does);
	// with `vates` as PID 1, PID 1 does.
	{"bpf", "/sys/fs/bpf", "bpf", "mode=0700", 0},
	{"devtmpfs", "/dev", "devtmpfs", "mode=0755", syscall.MS_NOSUID},
	{"devpts", "/dev/pts", "devpts", "mode=0620,ptmxmode=0666", 0},
	{"tmpfs", "/dev/shm", "tmpfs", "mode=1777", syscall.MS_NOSUID | syscall.MS_NODEV},
	{"tmpfs", "/run", "tmpfs", "mode=0755", syscall.MS_NOSUID | syscall.MS_NODEV},
	{"tmpfs", "/tmp", "tmpfs", "mode=1777", syscall.MS_NOSUID | syscall.MS_NODEV},
}

const (
	pid1Containerd     = "/usr/bin/containerd"
	pid1ContainerdConf = "/etc/containerd/config.toml"
	// Where containerd listens. PID 1 waits for it before starting the kubelet,
	// which talks to the CRI on this socket.
	pid1ContainerdSocket = "/run/containerd/containerd.sock"
	// vates-console, not vates-dashboard: it picks the graphical face when there
	// is a DRM card and falls back to the text one otherwise. One image, both.
	pid1Console = "/usr/local/bin/vates-console"
	// The kubelet, started by PID 1 as a host process. It is a symlink to the
	// launcher, which fetches the version vates-node.yaml asks for, verifies it,
	// caches it under /var and execs it. There is no container and no image.
	pid1Kubelet = "/usr/local/bin/kubelet"
	// The management API (mTLS gRPC), the thing that replaces SSH: a child of
	// PID 1.
	pid1API = "/usr/local/bin/vates-api"
	// The Xen guest agent (the Containerfile's `guest` pass): the Rust
	// gitlab.com/xen-project/xen-guest-agent. It detects the guest OS itself and
	// publishes the guest's view of itself to XenStore for Xen Orchestra. Not
	// needed for providerID -- the CCM maps a node by SystemUUID -- but it is
	// what makes the VM's IP, its OS and a clean shutdown visible to the
	// hypervisor.
	pid1XenGuestAgent = "/usr/sbin/xen-guest-agent"
	// The boot splash: the wordmark and the bring-up steps, drawn on the screen
	// before the console takes it over. It is Plymouth replaced by this binary,
	// nothing more.
	pid1Splash = "/usr/local/bin/vates-splash"
)

// Pid1 is the entry point when the binary is PID 1. It never returns: it either
// runs forever or reboots the machine.
func Pid1(_ []string) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("pid1: not PID 1 (pid %d); refusing to supervise", os.Getpid())
	}
	setPath()

	if err := mountPseudo(); err != nil {
		return err
	}
	// Before anything writes to /var: the container store, the kubelet's state
	// and this process's own log must land on the /var partition, not on the
	// root filesystem the mount would hide.
	mountVar()
	mountCNIOverlays()
	redirectStdio()
	assertReadOnlyRoot()
	// Flush what has been written so far: the read-only probe is the node's
	// first word about itself, and a machine that loses power a moment later
	// must still have said it. The alternative is what a test measured -- a
	// pid1.log of zero bytes, because a hard power-off does not flush the
	// guest's page cache.
	syscall.Sync()
	applySysctls()
	// On a machine with no screen, the serial console is the only place the
	// bring-up can be read, and the quieting the sysctl just applied is there to
	// protect a dashboard that does not exist. Un-mute it for the boot;
	// startConsole mutes it again when the dashboard takes the console over.
	if !tty.HasScreen() {
		setConsoleLoglevel(7)
		kmsg("console: no screen; the serial console carries the boot log")
	}
	makeRShared("/")
	setHostname()
	logDevices()
	logPlatform()
	bootStep("mount", "ok")

	// The splash comes first: it owns the screen until the console is ready, so
	// the kernel's own chatter is replaced by what is actually happening.
	startSplash()

	bootStep("containerd", "wait")
	if err := startContainerd(); err != nil {
		kmsg("containerd: %v", err)
		bootStep("containerd", "fail")
	} else {
		// The kubelet, started a few steps below, talks to the CRI on this
		// socket, and its own static pods are containers containerd runs, so the
		// CRI has to be READY to serve before it starts -- not merely listening
		// (see waitContainerd).
		waitContainerd(30 * time.Second)
		bootStep("containerd", "ok")
	}
	bootStep("network", "wait")
	bringUpNetwork()
	bootStep("network", "ok")
	startGuestAgent()
	bootStep("configure", "wait")
	dashboard.SetPhase("configuring the node")
	configureNode()
	bootStep("configure", "ok")
	startNTP()
	dashboard.SetPhase("starting the kubelet")
	bootStep("kubelet", "wait")
	startKubelet()
	bootStep("kubelet", "ok")
	go bootstrapNode()
	startAPI()
	stopSplash()
	startConsole()
	// The OS is up: make this slot's boot entry permanent, so it is no longer a
	// trial that would be rolled back. Best-effort; see ab.go.
	blessBoot()

	return supervise()
}

// makeRShared makes a mount recursive-shared, as systemd does for / at boot.
//
// The kubelet is a host process now, so its mounts and containerd's already
// share one namespace and nothing here needs the propagation the container era
// required. The call is kept because systemd makes / shared for its own reasons
// and removing it is a separate, measurable change -- but it is no longer load
// bearing for pod volumes.
func makeRShared(path string) {
	if err := syscall.Mount("", path, "", syscall.MS_SHARED|syscall.MS_REC, ""); err != nil {
		kmsg("mount: make-rshared %s: %v", path, err)
	}
}

// cgroupV2Controllers are the controllers the kubelet, containerd and runc use.
var cgroupV2Controllers = []string{"cpu", "cpuset", "io", "memory", "pids", "hugetlb"}

// mountCgroupV2 mounts the cgroup v2 unified hierarchy and enables the
// controllers the kubelet, containerd and runc need.
//
// v2, not v1: the kernel marks v1 deprecated ("cgroup v1 is in maintenance
// mode"), and the earlier v2 failure was not the v2 design -- it was
// CONFIG_CFS_BANDWIDTH missing from the kernel, which removes cpu.max on v2 just
// as it removes cpu.cfs_period_us on v1. With the kernel fixed, v2 is the modern
// layout and is what systemd-based systems use.
//
// On systemd, systemd enables these controllers at boot. With `vates` as PID 1
// there is no systemd, so PID 1 does it. Each controller is written on its own:
// a kernel without one must not stop the others.
func mountCgroupV2() {
	_ = os.MkdirAll("/sys/fs/cgroup", 0o755)
	if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
		kmsg("cgroup2: mount /sys/fs/cgroup: %v", err)
		return
	}
	const path = "/sys/fs/cgroup/cgroup.subtree_control"
	for _, c := range cgroupV2Controllers {
		if err := os.WriteFile(path, []byte("+"+c), 0); err != nil {
			kmsg("cgroup2: +%s: %v", c, err)
		}
	}
}

// setPath gives the init process a usable PATH. The kernel starts init with
// almost no environment, so exec.LookPath would only look in the current
// directory and miss /sbin/udhcpc and /usr/sbin/ip.
func setPath() {
	_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
}

// mountPseudo mounts every entry of the table that is not already mounted and
// whose target exists or can be created. A device or a base that already
// provides one of them (a kernel with /dev pre-mounted, for instance) is not an
// error.
func mountPseudo() error {
	for _, m := range pid1Mounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return fmt.Errorf("pid1: mkdir %s: %w", m.target, err)
		}
		if isMountpoint(m.target) {
			continue
		}
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ENODEV) {
				continue
			}
			return fmt.Errorf("pid1: mount %s on %s: %w", m.fstype, m.target, err)
		}
	}
	mountCgroupV2()
	return nil
}

// isMountpoint reports whether path is already a mount point, by reading
// /proc/self/mountinfo. Reading the file avoids re-mounting what an initramfs
// or the base already mounted, which a bare mount would either duplicate or
// fail on.
func isMountpoint(path string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[4] == path {
			return true
		}
	}
	return false
}

// bringUpNetwork brings the node's interface up and gives it an address.
//
// The address comes from the config drive when it carries a network-config, and
// from the DHCP client otherwise. That order is deliberate: a control plane's
// identity is tied to its address -- etcd records the peer URL it joined with,
// and kubeadm writes it into the API server's own manifest -- so a lease that
// can move is a control plane that can be lost on the next reboot. The provider
// that created the machine knows its address; the DHCP server only knows a MAC.
//
// busybox's udhcpc does the lease for now -- it is the last busybox dependency
// that matters, and replacing it with a Go client is a later step.
func bringUpNetwork() {
	// Bring loopback up first. The kubelet binds its healthz server and other
	// endpoints to 127.0.0.1, and with lo down that bind fails ("cannot assign
	// requested address"); nothing else here depends on it, which is why it was
	// missed.
	_ = exec.Command("ip", "link", "set", "lo", "up").Run()

	iface := firstEthernet()
	if iface == "" {
		kmsg("network: no ethernet interface")
		return
	}
	_ = exec.Command("ip", "link", "set", iface, "up").Run()

	if static := staticFromConfigDrive(); static != nil {
		if err := applyStaticNetwork(static); err != nil {
			kmsg("network: applying the config drive's addressing to %s: %v", static.Iface, err)
		} else {
			kmsg("network: %s configured from the config drive (%s)",
				static.Iface, strings.Join(static.Addresses, ", "))
			return
		}
	}

	if _, err := exec.LookPath("udhcpc"); err != nil {
		kmsg("network: %s is up but there is no udhcpc for a lease", iface)
		return
	}
	cmd := exec.Command("udhcpc", "-i", iface, "-b", "-q", "-n", "-t", "10")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		kmsg("network: udhcpc on %s: %v", iface, err)
		return
	}
	kmsg("network: lease obtained on %s", iface)
}

// staticFromConfigDrive reads the addressing the config drive asks for, or nil
// when there is no drive, no network-config, or one that asks for DHCP.
//
// The drive is read here rather than later because the address has to exist
// before anything that needs the network, and the drive is a CD-ROM that is
// already attached -- so a bounded retry is enough to cover the device node
// appearing. A node built by hand has no drive at all, which is not an error:
// the DHCP client below is the fallback.
func staticFromConfigDrive() *configdrive.StaticNetwork {
	deadline := time.Now().Add(configDriveWait)
	for {
		drive, err := configdrive.Find()
		if err == nil {
			static, err := drive.StaticNetwork()
			if err != nil {
				kmsg("network: reading the config drive's network-config: %v", err)
				return nil
			}
			return static
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// configDriveWait bounds the wait for the config drive before the node falls
// back to DHCP. Long enough for a CD-ROM to appear, short enough not to delay a
// node that has no drive.
const configDriveWait = 10 * time.Second

// applyStaticNetwork gives the interface the addresses, the default route and
// the resolvers the config drive asked for.
func applyStaticNetwork(network *configdrive.StaticNetwork) error {
	if _, err := net.InterfaceByName(network.Iface); err != nil {
		return fmt.Errorf("interface %s: %w", network.Iface, err)
	}
	for _, addr := range network.Addresses {
		if err := runIP("addr", "add", addr, "dev", network.Iface); err != nil {
			return err
		}
	}
	if network.Gateway != "" {
		if err := runIP("route", "add", "default", "via", network.Gateway, "dev", network.Iface); err != nil {
			return err
		}
	}
	if len(network.Nameservers) > 0 {
		if err := writeResolvers(network.Nameservers); err != nil {
			return err
		}
	}
	return nil
}

// runIP runs one `ip` command, returning its output with the error so a failed
// bring-up says what the kernel refused.
func runIP(args ...string) error {
	cmd := exec.Command("ip", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeResolvers replaces /etc/resolv.conf, which the image symlinks onto /run
// so a read-only root can be written through it.
func writeResolvers(servers []string) error {
	var b strings.Builder
	for _, server := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", server)
	}
	if err := os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing /etc/resolv.conf: %w", err)
	}
	return nil
}

// firstEthernet returns the first non-loopback interface name.
func firstEthernet() string {
	ents, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if e.Name() != "lo" {
			return e.Name()
		}
	}
	return ""
}

// configureNode runs the first-boot configuration from the config drive, if
// there is one. It is `vates-init configure`, in this same process: no
// distribution, no unit to order it. A node with no config drive is not an
// error -- it is a console-only boot.
func configureNode() {
	if _, err := os.Stat("/usr/local/bin/vates-init"); err != nil {
		kmsg("configure: no vates-init: %v", err)
		return
	}
	kmsg("configure: running vates-init configure")
	if err := firstboot.Configure(firstboot.ConfigureOptions{}); err != nil {
		kmsg("configure failed: %v", err)
		// Surface the reason where an operator looks: the console reads the phase
		// and shows it as its activity line. Without this a failed configure
		// leaves the screen saying only "waiting for the API server", which is
		// the symptom and never the cause.
		dashboard.SetPhase("configure failed: " + err.Error())
		return
	}
	kmsg("configure complete")
}

// startKubelet starts the kubelet, as a child of PID 1.
//
// It is a host process now, not a container: /usr/local/bin/kubelet is a symlink
// to the launcher, which fetches the version vates-node.yaml asks for, verifies
// it, caches it under /var and execs it. Nothing stands between PID 1 and the
// node agent, and no containerd and no image are involved.
//
// The environment carries KUBERNETES_VERSION and KUBERNETES_BINARY_BASE, which
// Configure set in this process from the document; the launcher reads them.
func startKubelet() {
	if _, err := os.Stat(pid1Kubelet); err != nil {
		kmsg("kubelet: %s: %v", pid1Kubelet, err)
		return
	}
	if err := startChild("kubelet", pid1Kubelet, kubeletArgs()...); err != nil {
		kmsg("kubelet: %v", err)
		return
	}
	kubeletMu.Lock()
	kubeletStartedAt = time.Now()
	kubeletMu.Unlock()
}

// bootstrapNode runs the cluster-wide bootstrap, as the systemd unit did after
// the kubelet had started.
//
// It runs on EVERY node, and neither a gate nor a wait is wanted here.
//
// Not gated on /etc/kubernetes/manifests: a control plane that JOINS has none
// yet -- `kubeadm join` is what writes them -- so gating on the directory skips
// the join on exactly the machines that need it, and they come up as ordinary
// nodes with no error anywhere. firstboot.Bootstrap already returns immediately
// for a worker, so there is nothing to protect against.
//
// Not preceded by a wait for the LOCAL API server: a joining control plane has
// no local API server until the join itself starts one, so waiting for it first
// is a deadlock -- on the Bootstrapping node it is redundant, since Bootstrap's
// own waitForAPI already waits on the control plane endpoint.
func bootstrapNode() {
	kmsg("bootstrap: running vates-init bootstrap")
	if err := firstboot.BootstrapNode(firstboot.BootstrapOptions{}); err != nil {
		kmsg("bootstrap failed: %v", err)
		dashboard.SetPhase("bootstrap failed: " + err.Error())
		return
	}
	kmsg("bootstrap complete")
	// Not "ready": bootstrap done is not the node Ready. The kubelet may still
	// be pulling the control plane's images. The console hides the activity line
	// once the node itself reports Ready, so this is what it shows until then.
	dashboard.SetPhase("waiting for the node to become ready")
}

// startAPI starts the node's management API. It is the only way to ask the node
// anything -- there is no shell and no sshd -- and it is what an operator or
// vateskctl talks to. Configure has written its certificate material by now.
func startAPI() {
	if _, err := os.Stat(pid1API); err != nil {
		kmsg("api: %s: %v", pid1API, err)
		return
	}
	if err := startChild("vates-api", pid1API); err != nil {
		kmsg("api: %v", err)
	}
}

// startGuestAgent brings up the Xen guest agent: the Rust `xen-guest-agent`,
// which detects the OS itself and publishes to XenStore, one process. (The Go
// xe-guest-utilities it replaces needed a separate xe-linux-distribution run and
// an OS cache first.)
//
// Best effort, and a no-op off Xen: the agent writes to XenStore, which only
// exists in a Xen guest, and it would exit at once elsewhere (the libvirt test
// cluster). The daemon is a supervised child, so the SIGTERM a clean shutdown
// sends to every child reaches it.
func startGuestAgent() {
	// Best effort, and a no-op off Xen: the agent writes to XenStore, which only
	// exists in a Xen guest, and it would exit at once elsewhere (the libvirt test
	// cluster). Platform() keys on /proc/xen -- the kernel's own detection of Xen
	// -- so this is the same gate as before, but named and shared with the rest
	// of the platform policy. The daemon is a supervised child, so the SIGTERM a
	// clean shutdown sends to every child reaches it.
	if hostinfo.Detect() != hostinfo.PlatformXen {
		return
	}
	if _, err := os.Stat(pid1XenGuestAgent); err != nil {
		kmsg("xen-guest-agent: %s: %v", pid1XenGuestAgent, err)
		return
	}
	// --stderr: the agent logs to syslog by default, and the node runs no syslog
	// daemon, so its logs would go nowhere. To stderr, they land in the child's
	// log (/var/log/vates/xen-guest-agent.log), which `vateskctl logs` returns.
	if err := startChild("xen-guest-agent", pid1XenGuestAgent, "--stderr"); err != nil {
		kmsg("xen-guest-agent: %v", err)
	}
}

// kubeletArgs builds the kubelet's command-line flags. The per-node values
// (NODE_NAME, NODE_IP, the bootstrap kubeconfig) are read from the drop-in
// configure wrote, so there is one source for them rather than a second copy
// here. The program itself is pid1Kubelet, which startChild supplies as argv[0].
func kubeletArgs() []string {
	return kubeletArgsFromEnv(dropInEnv(firstboot.KubeletDropIn))
}

// kubeletArgsFromEnv builds the kubelet's flags from the drop-in environment.
// Split from kubeletArgs so the mapping -- which value becomes which flag -- is
// testable without a file on disk.
func kubeletArgsFromEnv(env map[string]string) []string {
	args := []string{
		"--config=/etc/kubelet/kubelet.conf",
		"--root-dir=/var/lib/kubelet",
		"--kubeconfig=/etc/kubernetes/kubelet.conf",
		"--client-ca-file=/etc/kubernetes/pki/ca.crt",
	}
	if v := env["NODE_NAME"]; v != "" {
		args = append(args, "--hostname-override="+v)
	}
	if v := env["NODE_IP"]; v != "" {
		args = append(args, "--node-ip="+v)
	}
	if v := env["KUBELET_BOOTSTRAP_KUBECONFIG"]; v != "" {
		args = append(args, "--bootstrap-kubeconfig="+v)
	}
	// The kubelet stands down for an external cloud controller manager: it
	// stops managing the node's own lifecycle (labels, providerID, deletion)
	// and leaves it to the CCM. Reading it from the drop-in keeps the value a
	// property of the machine, written by vates-init from vates-node.yaml.
	if v := env["CLOUD_PROVIDER"]; v != "" {
		args = append(args, "--cloud-provider="+v)
	}
	return args
}

// dropInEnv reads Environment=KEY=VALUE lines from a systemd drop-in.
func dropInEnv(path string) map[string]string {
	m := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		kv, ok := strings.CutPrefix(strings.TrimSpace(line), "Environment=")
		if !ok {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

// consoleEnv is what the console child is started with: this process's own
// environment, plus the mode the machine chose.
//
// The mode is the machine's, not the image's, and vates-init writes it for the
// console the same way it writes the kubelet's values -- as a systemd drop-in
// (firstboot.ConsoleDropIn). A pid1 image has no systemd to read that drop-in
// and put DASHBOARD_MODE into the console's environment, which is what the
// drop-in assumes, so this process reads it instead, exactly as it reads the
// kubelet's drop-in for the kubelet's flags. Nothing to say is nil, which makes
// the child inherit this environment and keep the console's own default.
func consoleEnv(dropIn string) []string {
	mode := dropInEnv(dropIn)["DASHBOARD_MODE"]
	if mode == "" {
		return nil
	}
	return append(os.Environ(), "DASHBOARD_MODE="+mode)
}

// logDevices records what the console will find, in the kernel log: whether a
// DRM card and a framebuffer exist. This is the first thing to look at when the
// picture does not come up, and it is invisible from the console itself.
func logDevices() {
	for _, p := range []string{"/dev/dri/card0", "/dev/fb0"} {
		if _, err := os.Stat(p); err == nil {
			kmsg("device present: %s", p)
		} else {
			kmsg("device absent: %s (%v)", p, err)
		}
	}
}

// logPlatform records the hypervisor the node detected, in the kernel log. It is
// the first thing to look at when a node misbehaves on one hypervisor but not
// another -- and it is what startGuestAgent and the console's serial face key on.
func logPlatform() {
	kmsg("platform: %s", hostinfo.Detect().Name())
}

// bootLines is the walk through bring-up, and bootSplash the splash child that
// paints it. Neither must be read while it is being written, hence the mutex.
//
// bootPhaseAt remembers when each phase was marked "wait", so the "ok" (or
// "fail") that closes it can report how long it took. The boot is measured, not
// felt: "the boot is slow" is not actionable, "the network phase took 31 s" is.
// The durations land in pid1.log, next to the phase's own output, where an
// offline read of the disk finds them.
var (
	bootMu      sync.Mutex
	bootLines   []splashStep
	bootSplash  int
	bootPhaseAt = map[string]time.Time{}
)

// bootStep records one phase of bring-up and rewrites the status file the splash
// reads. A label already present is updated in place, so a phase can be marked
// "wait" and then "ok" without appearing twice. Closing a phase logs its
// duration, which is the measurement the whole boot's optimization rests on.
func bootStep(label, state string) {
	bootMu.Lock()
	updated := false
	for i := range bootLines {
		if bootLines[i].label == label {
			bootLines[i].state = state
			updated = true
			break
		}
	}
	if !updated {
		bootLines = append(bootLines, splashStep{label: label, state: state})
	}
	var elapsed time.Duration
	closed := false
	switch state {
	case "wait":
		bootPhaseAt[label] = time.Now()
	case "ok", "fail":
		if t0, ok := bootPhaseAt[label]; ok {
			elapsed = time.Since(t0)
			closed = true
		}
	}
	bootMu.Unlock()
	if closed {
		fmt.Printf("phase %-10s %-4s %s\n", label, state, elapsed.Round(time.Millisecond))
		kmsg("phase %s: %s in %s", label, state, elapsed.Round(time.Millisecond))
	}
	writeBootStatus()
}

// bootDone tells the splash the console is about to take the screen over.
func bootDone() {
	bootMu.Lock()
	bootLines = append(bootLines, splashStep{label: "done", state: "ok"})
	bootMu.Unlock()
	writeBootStatus()
}

func writeBootStatus() {
	bootMu.Lock()
	var b strings.Builder
	for _, s := range bootLines {
		fmt.Fprintf(&b, "%s|%s\n", s.label, s.state)
	}
	data := b.String()
	bootMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(BootStatusFile), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(BootStatusFile, []byte(data), 0o644)
}

// startSplash launches the boot screen and records it as a supervised child.
func startSplash() {
	if _, err := os.Stat(pid1Splash); err != nil {
		return
	}
	cmd := exec.Command(pid1Splash)
	if err := cmd.Start(); err != nil {
		kmsg("splash: %v", err)
		return
	}
	bootMu.Lock()
	bootSplash = cmd.Process.Pid
	bootMu.Unlock()
	childrenMu.Lock()
	children[cmd.Process.Pid] = "splash"
	childrenMu.Unlock()
	kmsg("splash: started (pid %d)", cmd.Process.Pid)
}

// stopSplash marks the boot done and waits for the splash to release the DRM
// device, so the console can take it: two programs drawing the same card is a
// race, and the loser is the picture.
func stopSplash() {
	bootDone()
	bootMu.Lock()
	pid := bootSplash
	bootSplash = 0
	bootMu.Unlock()
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 40; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// setHostname applies the hostname vates-init recorded (firstboot.HostnameFile,
// under /var because the root is read-only). Failure is not fatal: a node with
// the default hostname still runs, and refusing to boot over it would be worse.
func setHostname() {
	data, err := os.ReadFile(firstboot.HostnameFile)
	if err != nil {
		return
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return
	}
	_ = syscall.Sethostname([]byte(name))
}

// containerdNotifySocket is where PID 1 listens for containerd's systemd-style
// readiness notification.
//
// containerd is a `Type=notify` service: under systemd it sends READY=1 once
// every plugin -- the CRI and its snapshotter included -- has loaded, and
// systemd starts the kubelet only after that. This image runs no systemd, so
// PID 1 offers the socket and waits for the same message (see waitContainerd).
const containerdNotifySocket = "/run/vates/notify"

// containerdReady is set once containerd has announced READY=1.
var containerdReady atomic.Bool

// containerdNotifyConn is the readiness socket, or nil when it could not be
// created -- the wait then falls back to the CRI socket existing. It is written
// by startContainerd, before the goroutine that reads it runs.
var containerdNotifyConn *net.UnixConn

// containerdReadyMessage reports whether a systemd notification datagram is the
// readiness one. sd_notify sends newline-separated KEY=VALUE lines; containerd
// sends "READY=1" when it has finished starting.
func containerdReadyMessage(msg []byte) bool {
	for line := range strings.SplitSeq(string(msg), "\n") {
		if line == "READY=1" {
			return true
		}
	}
	return false
}

// listenContainerdReadiness flags the first READY=1 and then stops: the only
// question is whether containerd has finished starting.
func listenContainerdReadiness(conn *net.UnixConn) {
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if containerdReadyMessage(buf[:n]) {
			containerdReady.Store(true)
			return
		}
	}
}

// waitContainerd blocks until containerd is READY TO SERVE, not merely
// listening: it waits for the READY=1 notification containerd sends when every
// plugin, the CRI included, has loaded (see containerdNotifySocket).
//
// It is a dependency wait, not a delay: it returns as soon as the notification
// arrives, and only a runtime that never announces pays the timeout.
//
// The socket alone was the earlier criterion, and it was measured to be too
// weak in two ways. configure's kubelet-image check runs `ctr` before the
// kubeadm phases; a socket that exists but is not yet serving failed it with
// "cannot access socket", the phases never ran, no CA and no static-pod
// manifests were written, and bootstrap waited for an API server that could
// never start. And the kubelet's first image garbage collection, started on a
// socket that is listening but whose plugins are not up, reads a capacity of 0
// on the image filesystem and records a one-shot `InvalidDiskCapacity` event.
//
// When the readiness socket could not be created, it falls back to the CRI
// socket existing, which is all a daemon without the notification can offer.
func waitContainerd(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		if containerdReady.Load() {
			return
		}
		if containerdNotifyConn == nil {
			if _, err := os.Stat(pid1ContainerdSocket); err == nil {
				return
			}
		}
		if !time.Now().Before(deadline) {
			kmsg("containerd: not ready after %s; continuing without it", timeout)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startContainerd launches the container runtime. It is not re-started here on
// exit: a runtime that dies is a bug worth seeing, and the reap loop reports
// it. Starting it before the console means the console can show CRI state.
//
// The readiness socket is created first and named to containerd through
// NOTIFY_SOCKET, so waitContainerd waits for the daemon to say it is ready
// rather than only for its socket to appear.
func startContainerd() error {
	if _, err := os.Stat(pid1Containerd); err != nil {
		return err
	}
	return startChildEnv("containerd", containerdEnviron(), pid1Containerd, "--config", pid1ContainerdConf)
}

// containerdEnviron creates the readiness socket and returns containerd's
// environment, or nil (the inherited environment) when the socket could not be
// created: a machine that cannot offer the notification keeps the weaker
// wait instead of refusing to boot.
func containerdEnviron() []string {
	if err := os.MkdirAll(filepath.Dir(containerdNotifySocket), 0o755); err != nil {
		kmsg("containerd: readiness socket: %v", err)
		return nil
	}
	_ = os.Remove(containerdNotifySocket)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: containerdNotifySocket, Net: "unixgram"})
	if err != nil {
		kmsg("containerd: readiness socket: %v", err)
		return nil
	}
	containerdNotifyConn = conn
	go listenContainerdReadiness(conn)
	return append(os.Environ(), "NOTIFY_SOCKET="+containerdNotifySocket)
}

// setConsoleLoglevel sets what reaches the consoles. The sysctl takes four
// numbers; only the first (the console level) and the fourth (its default) are
// of interest, the other two are left where the image put them. A message
// prints when its level is LOWER than this, so 7 lets emerg..info through -- a
// normal boot log -- and 3 keeps only emerg/alert/crit, which is what the
// dashboard wants.
func setConsoleLoglevel(level int) {
	_ = os.WriteFile("/proc/sys/kernel/printk",
		[]byte(fmt.Sprintf("%d 4 1 %d\n", level, level)), 0o644)
}

// consoleTTY picks the terminal the console draws on. A machine with a DRM card
// gets the VGA virtual console, as it always did. A machine with none -- a VM
// with no VGA, a headless one -- still has a console, the serial or Xen one,
// which /dev/console resolves to (hvc0 under Xen, ttyS0 under KVM); the text
// dashboard is the one face that works there.
//
// The choice is a plain stat, not a wait: the DRM drivers are built in, and
// SYSFB_SIMPLEFB gives a card from the firmware framebuffer in any case, so
// card0 is present before PID 1 runs whenever there is a screen at all. Nothing
// here may add latency to a boot this short.
func consoleTTY() (dev string, vga bool) {
	if tty.HasScreen() {
		return "/dev/tty1", true
	}
	// No screen: the serial console, the one an operator reaches out of band
	// (XCP-ng's xe console, libvirt's virsh console). hvc0 is Xen's console,
	// ttyS0 the emulated UART; both are named before /dev/console because which
	// device /dev/console IS depends on registration order, and it can still be
	// the dead VGA VT on a machine that has no screen.
	for _, d := range []string{"/dev/hvc0", "/dev/ttyS0"} {
		if _, err := os.Stat(d); err == nil {
			return d, false
		}
	}
	return "/dev/console", false
}

// startConsole launches the dashboard on the machine's console. It gets a real
// controlling terminal (Setsid + Setctty), not just a redirected stdout: a
// termcap UI needs a tty to size itself and read keys, and without one it either
// fails or draws nowhere. Failures go to /dev/kmsg, because the console cannot
// report its own absence anywhere the console would be seen.
func startConsole() {
	if _, err := os.Stat(pid1Console); err != nil {
		kmsg("console: %s: %v", pid1Console, err)
		return
	}
	dev, vga := consoleTTY()
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		kmsg("console: open %s: %v", dev, err)
		return
	}
	// Only a virtual console has one to activate; on the serial/Xen console the
	// ioctl has nothing to switch.
	if vga {
		activateVT(f.Fd(), 1)
	} else {
		// The dashboard owns the serial console from here: quiet the kernel
		// again, so its lines and the CNI's do not print over it.
		setConsoleLoglevel(3)
	}

	cmd := exec.Command(pid1Console)
	cmd.Stdin = f
	cmd.Stdout = f
	// The mode reaches the console through its environment, the way the drop-in
	// assumes systemd would deliver it; there is no systemd here, so it is read
	// and passed on (consoleEnv).
	cmd.Env = consoleEnv(firstboot.ConsoleDropIn)
	// stderr goes to the kernel log, not the tty: when the graphical console
	// falls back to text, its reason is written to stderr, and it would
	// otherwise be printed to the very console it failed to become.
	pr, pw, perr := os.Pipe()
	if perr == nil {
		cmd.Stderr = pw
	} else {
		cmd.Stderr = f
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		// Ctty is a descriptor number *in the child*. Stdin/Stdout/Stderr are
		// the tty, so they are 0, 1 and 2 there; naming the parent's fd here is
		// what fails with "Setctty set but Ctty not valid in child".
		Ctty: 0,
	}
	if err := cmd.Start(); err != nil {
		kmsg("console: start: %v", err)
		return
	}
	if perr == nil {
		if err := pw.Close(); err != nil {
			kmsg("console: close the stderr pipe: %v", err)
		}
		go func() {
			sc := bufio.NewScanner(pr)
			for sc.Scan() {
				kmsg("console stderr: %s", sc.Text())
			}
		}()
	}
	kmsg("console: started (pid %d) on %s", cmd.Process.Pid, dev)
	consoleMu.Lock()
	consoleStartedAt = time.Now()
	consoleMu.Unlock()
	childrenMu.Lock()
	children[cmd.Process.Pid] = "console"
	childrenMu.Unlock()
}

// restartConsole brings the console back after it exited on its own.
//
// Rate-limited: a console that starts and dies at once -- a mode the kernel
// refuses, a card that went away -- would otherwise fork as fast as the kernel
// reaps, filling the log with the same reason. One restart a second is still
// immediate to someone standing at the machine, and it is bounded.
func restartConsole() {
	consoleMu.Lock()
	wait := time.Until(consoleStartedAt.Add(time.Second))
	consoleMu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
	// The machine may have begun shutting down while this waited.
	if shuttingDown.Load() {
		return
	}
	kmsg("console: restarting")
	startConsole()
}

// kubeletMu guards kubeletStartedAt, the last time the kubelet was launched, so
// that a crash loop is rate-limited the way the console's is.
var (
	kubeletMu        sync.Mutex
	kubeletStartedAt time.Time
)

// restartKubelet brings the kubelet back after it exited on its own.
//
// The kubelet is the node: a crash must not leave a machine that pings but runs
// no cluster, which is what would happen without this. Rate-limited like the
// console, so a kubelet that dies at once does not fork as fast as the kernel
// reaps, and NOT restarted during shutdown, where the child was stopped on
// purpose.
func restartKubelet() {
	kubeletMu.Lock()
	wait := time.Until(kubeletStartedAt.Add(time.Second))
	kubeletMu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
	if shuttingDown.Load() {
		return
	}
	kmsg("kubelet: restarting")
	startKubelet()
}

// vtActivate is the Linux VT_ACTIVATE ioctl (from linux/vt.h). The argument is a
// value, not a pointer, so no unsafe is needed.
const vtActivate = 0x5606

func activateVT(fd uintptr, n int) {
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, vtActivate, uintptr(n))
}

// kmsg writes one line to the kernel log, which is visible on a serial console
// and in `dmesg` even when the userspace console shows nothing.
func kmsg(format string, args ...any) {
	f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	// The kernel log is best effort: there is nowhere else to report a failure
	// to write to it -- or to close it, which is why the close error is dropped.
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "vates-pid1: "+format+"\n", args...)
}

// rebootAction maps a signal to what the machine should do. It is pure so the
// policy can be tested without being PID 1.
func rebootAction(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTERM, syscall.SIGINT, syscall.SIGPWR:
		return "poweroff"
	case syscall.SIGUSR2:
		return "reboot"
	}
	return ""
}

// supervise waits for signals forever. SIGCHLD reaps zombies; a shutdown signal
// stops the children and powers the machine off.
func supervise() error {
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT, syscall.SIGPWR, syscall.SIGUSR2)

	for s := range sigs {
		sysSig, ok := s.(syscall.Signal)
		if !ok {
			continue
		}
		if sysSig == syscall.SIGCHLD {
			reap()
			continue
		}
		switch rebootAction(sysSig) {
		case "poweroff":
			shutdown(syscall.LINUX_REBOOT_CMD_POWER_OFF)
		case "reboot":
			shutdown(syscall.LINUX_REBOOT_CMD_RESTART)
		}
	}
	return nil
}

// children maps a supervised child's pid to a name, so the single reaper can say
// what exited.
var (
	childrenMu sync.Mutex
	children   = map[int]string{}
)

// shuttingDown is set once PID 1 has begun powering off or rebooting. It stops
// the console from being restarted while the children are being stopped: the
// console is the one child that is brought back, and it must not be brought back
// on top of a machine that is going down.
var shuttingDown atomic.Bool

// consoleMu guards consoleStartedAt, the last time the console was launched.
var (
	consoleMu        sync.Mutex
	consoleStartedAt time.Time
)

// pid1LogDir holds one file per supervised service. Nothing writes to the
// console: the splash owns the screen until the dashboard does, and a daemon
// that prints on it would draw over both.
const pid1LogDir = "/var/log/vates"

// redirectStdio points PID 1's own stdout and stderr at a file on disk.
//
// Init is started with the console as its output, so without this every line the
// configure and bootstrap steps print lands on the screen the splash is
// painting -- and, worse, nowhere that survives: the steps that fail run in this
// process, and there is no shell to read the kernel ring buffer afterwards.
// Explicit kmsg() calls still go to the kernel log; this keeps the program's own
// output where an operator (or an offline read of the disk) can find it.
//
// /var/lib/vates, not /var/log: /var/log is a symlink to /tmp on this image, and
// /tmp is a tmpfs PID 1 mounts -- a log written there is gone on the next boot,
// and gone the moment the machine is powered off, which is exactly when it is
// wanted. /var/lib is on the /var partition, which persists.
const pid1LogFile = "/var/lib/vates/pid1.log"

func redirectStdio() {
	if err := os.MkdirAll(filepath.Dir(pid1LogFile), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(pid1LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	// Closing the original handle says nothing: fds 1 and 2 are duplicates made
	// with Dup2 below and remain open on the same file.
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())
	_ = syscall.Dup2(fd, 1)
	_ = syscall.Dup2(fd, 2)
}

// roProbe is the path assertReadOnlyRoot tries to create. At the root of the
// read-only filesystem, so nothing else has a reason to be there.
const roProbe = "/.vates-readonly-probe"

// assertReadOnlyRoot checks the property this whole layout exists to have: the
// root filesystem cannot be written -- while the paths a workload MUST be able
// to write stay writable.
//
// It does not read the mount options. A remount, or a kernel command line that
// lost its `ro`, still READS as read-only while the write succeeds -- so the
// check writes, and reports the effect, exactly as the plan in
// docs/IMMUTABILITY.md says a check must. The result goes to this process's
// output, which redirectStdio has already pointed at /var/lib/vates/pid1.log,
// where an offline read of the disk finds it without a login.
//
// The second probe is the other half of the same property: /opt/cni/bin is an
// overlay, and a CNI installer has to be able to write there. A root that is
// read-only because the overlays are missing would be a node whose CNI can never
// install, which is not the same thing at all.
func assertReadOnlyRoot() {
	err := os.WriteFile(roProbe, []byte("x"), 0o600)
	switch {
	case err == nil:
		_ = os.Remove(roProbe)
		fmt.Println("root: WRITABLE -- the read-only root is NOT enforced")
		kmsg("root: WRITABLE (the read-only root is NOT enforced)")
	case errors.Is(err, syscall.EROFS):
		fmt.Println("root: read-only (ok)")
	default:
		// Not writable, but not EROFS either (EACCES, ENOSPC, ...). Worth
		// saying, because it is not the reason the write failed.
		fmt.Printf("root: not writable, but not EROFS: %v\n", err)
	}

	const cniProbe = "/opt/cni/bin/.vates-writable-probe"
	if err := os.WriteFile(cniProbe, []byte("x"), 0o600); err == nil {
		_ = os.Remove(cniProbe)
		fmt.Println("cni: /opt/cni/bin writable (ok)")
	} else {
		fmt.Printf("cni: /opt/cni/bin NOT writable: %v\n", err)
	}

	// The third probe is the same property for the path a control-plane pod
	// declares as a `DirectoryOrCreate` hostPath: containerd creates it on the
	// host before the container can start. Its default sits under /usr/libexec,
	// which a read-only root cannot create, so the image makes it a symlink into
	// /var whose target PID 1 created in mountVar a moment ago
	// (board/vates/overlay/usr/libexec, internal/app/grow.go). A node that
	// forgot this is a node whose controller-manager never leaves
	// CreateContainerError, with the failure inside the CRI and not in the pod's
	// events -- measured, and the reason this probe exists.
	const flexvolDir = "/usr/libexec/kubernetes/kubelet-plugins/volume/exec"
	flexvolProbe := flexvolDir + "/.vates-writable-probe"
	if err := os.WriteFile(flexvolProbe, []byte("x"), 0o600); err == nil {
		_ = os.Remove(flexvolProbe)
		fmt.Printf("flexvol: %s writable (ok)\n", flexvolDir)
	} else {
		fmt.Printf("flexvol: %s NOT writable: %v\n", flexvolDir, err)
	}

	// Which slot booted, for A/B. The PARTUUID is the whole answer: which one it
	// is (a or b) is a property of the boot entries, read where they are needed.
	if uuid, err := ab.RootPartUUID(); err == nil {
		fmt.Printf("root: PARTUUID %s\n", uuid)
	}
}

// pid1SysctlFiles are read in this order, as systemd-sysctl does.
var pid1SysctlFiles = []string{"/etc/sysctl.d/99-vates.conf"}

// applySysctls writes the drop-ins under /etc/sysctl.d to /proc/sys.
//
// On a systemd distribution systemd-sysctl does this. With `vates` as PID 1
// nothing does it, and the config sits on disk unapplied -- which is invisible
// until something needs it. What needed it: `kubeadm join` refuses to run with
//
//	[ERROR FileContent--proc-sys-net-ipv4-ip_forward]:
//	/proc/sys/net/ipv4/ip_forward contents are not set to 1
//
// so a joining control plane retried its four attempts and never joined, while
// the bootstrapping control plane (whose phases skip the preflight) worked. It
// is also what makes kube-proxy's bridge rules match, so pod traffic is not
// forwarded across the CNI bridge without it.
//
// Best effort per key: a key whose module is not loaded does not exist, and one
// absent key must not stop the others.
func applySysctls() {
	for _, path := range pid1SysctlFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			proc := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
			if err := os.WriteFile(proc, []byte(value), 0o644); err != nil {
				kmsg("sysctl: %s=%s: %v", key, value, err)
			}
		}
	}
}

// childLog opens the log file for a supervised child, falling back to the
// kernel log if the directory cannot be created.
func childLog(name string) *os.File {
	if err := os.MkdirAll(pid1LogDir, 0o755); err != nil {
		if f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0); err == nil {
			return f
		}
		return os.Stdout
	}
	f, err := os.OpenFile(filepath.Join(pid1LogDir, name+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return os.Stdout
	}
	return f
}

// startChild starts a supervised child and records it for the reaper. It does
// NOT call exec.Cmd.Wait: PID 1 must reap every child itself, and a generic
// Wait4(-1) racing a Cmd.Wait on the same pid makes one of them fail with
// ECHILD ("waitid: no child processes").
func startChild(name, path string, args ...string) error {
	return startChildEnv(name, nil, path, args...)
}

// startChildEnv is startChild with a chosen environment for the child. nil
// means "inherit PID 1's", which is every child but containerd: containerd has
// to be TOLD the readiness socket's address through NOTIFY_SOCKET, and handing
// that variable to the kubelet or the console as well would be a fact about the
// manager leaking into children that have nothing to do with it.
func startChildEnv(name string, env []string, path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	log := childLog(name)
	cmd.Stdout = log
	cmd.Stderr = log
	err := cmd.Start()
	// Close the parent's copy: the child holds its own. os.Stdout is the
	// fallback and must never be closed.
	if log != os.Stdout {
		if cerr := log.Close(); cerr != nil {
			kmsg("child log: %s: close: %v", name, cerr)
		}
	}
	if err != nil {
		return err
	}
	childrenMu.Lock()
	children[cmd.Process.Pid] = name
	childrenMu.Unlock()
	kmsg("%s: started (pid %d)", name, cmd.Process.Pid)
	return nil
}

// reap collects every child that has exited, without blocking, and names it.
// PID 1 is responsible for this or the process table fills with zombies.
func reap() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		childrenMu.Lock()
		name := children[pid]
		delete(children, pid)
		childrenMu.Unlock()
		if name == "" {
			name = "child"
		}
		kmsg("%s exited (pid %d)", name, pid)
		// A supervised service that dies is a bring-up that will not finish.
		// Say so on the console's activity line, where an operator is looking,
		// rather than only on the kernel log -- the screen otherwise keeps
		// showing "waiting for the API server", which is the symptom.
		if name != "console" && !shuttingDown.Load() {
			dashboard.SetPhase(fmt.Sprintf("%s exited; see its log", name))
		}
		// The console is the machine's face and must not be left dead. It
		// ignores Ctrl-C itself; this is the backstop for a crash, a bad mode,
		// or a signal it does catch. It is NOT restarted during shutdown, where
		// the child was stopped on purpose.
		if name == "console" && !shuttingDown.Load() {
			restartConsole()
		}
		// The kubelet holds the node together; a crash must not leave it dead.
		if name == "kubelet" && !shuttingDown.Load() {
			restartKubelet()
		}
	}
}

// shutdown stops the children, flushes the filesystems and reboots or powers
// off. The Reboot call does not return.
func shutdown(cmd int) {
	shuttingDown.Store(true)
	_ = syscall.Kill(-1, syscall.SIGTERM)
	syscall.Sync()
	_ = syscall.Reboot(cmd)
	// If Reboot failed (no permission, no initramfs hook), fall back to a hard
	// exit so the kernel can panic and be restarted by the hypervisor rather
	// than hang.
	os.Exit(1)
}
