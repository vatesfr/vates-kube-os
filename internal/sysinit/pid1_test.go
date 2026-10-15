package sysinit

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/vatesfr/vates-kube-os/internal/firstboot"
)

// TestContainerdReadyMessage pins what counts as containerd announcing it is
// ready. PID 1 uses it to start the kubelet only once the CRI can serve -- the
// fix for the one-shot InvalidDiskCapacity event on a fresh boot -- so a
// message that is not exactly READY=1 must not be mistaken for it.
func TestContainerdReadyMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"ready alone", "READY=1", true},
		{"ready with a trailing newline", "READY=1\n", true},
		{"a status before ready", "STATUS=booting\nREADY=1\n", true},
		{"a status after ready", "READY=1\nSTATUS=serving\n", true},
		{"a status only", "STATUS=starting", false},
		{"watchdog is not readiness", "WATCHDOG=1", false},
		{"not ready", "READY=0", false},
		{"ready with trailing text", "READY=1x", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := containerdReadyMessage([]byte(c.msg)); got != c.want {
				t.Errorf("containerdReadyMessage(%q) = %v, want %v", c.msg, got, c.want)
			}
		})
	}
}

// TestRebootAction pins the shutdown policy: which signal powers the machine
// off, which reboots it, and which does neither.
func TestRebootAction(t *testing.T) {
	cases := []struct {
		sig  syscall.Signal
		want string
	}{
		{syscall.SIGTERM, "poweroff"},
		{syscall.SIGINT, "poweroff"},
		{syscall.SIGPWR, "poweroff"},
		{syscall.SIGUSR2, "reboot"},
		{syscall.SIGCHLD, ""},
		{syscall.SIGHUP, ""},
	}
	for _, c := range cases {
		if got := rebootAction(c.sig); got != c.want {
			t.Errorf("rebootAction(%v) = %q, want %q", c.sig, got, c.want)
		}
	}
}

// TestPid1MountOrder guards the ordering constraint the table has: /dev must be
// mounted before its sub-mounts, and /sys before the filesystems mounted under
// it. The cgroup hierarchy is mounted separately (mountCgroupV2), after the
// table, so /sys must be in the table for it.
func TestPid1MountOrder(t *testing.T) {
	index := make(map[string]int, len(pid1Mounts))
	for i, m := range pid1Mounts {
		index[m.target] = i
	}
	if index["/dev"] > index["/dev/pts"] || index["/dev"] > index["/dev/shm"] {
		t.Error("/dev must be mounted before /dev/pts and /dev/shm")
	}
	if _, ok := index["/sys"]; !ok {
		t.Error("/sys must be in the mount table (the cgroup and bpf hierarchies are mounted under it)")
	}
	if index["/sys"] > index["/sys/fs/bpf"] {
		t.Error("/sys must be mounted before /sys/fs/bpf")
	}
}

// TestKubeletDefersToTheCloudProviderOnlyWhenAsked pins the mapping from the
// drop-in's CLOUD_PROVIDER to the kubelet's flag -- and its absence when no CCM
// is configured, which must read exactly as before.
func TestKubeletDefersToTheCloudProviderOnlyWhenAsked(t *testing.T) {
	env := map[string]string{"NODE_NAME": "n", "NODE_IP": "10.0.0.1"}
	if args := kubeletArgsFromEnv(env); slices.Contains(args, "--cloud-provider=external") {
		t.Errorf("--cloud-provider was set with no CLOUD_PROVIDER: %v", args)
	}

	env["CLOUD_PROVIDER"] = "external"
	if args := kubeletArgsFromEnv(env); !slices.Contains(args, "--cloud-provider=external") {
		t.Errorf("--cloud-provider is missing with CLOUD_PROVIDER=external: %v", args)
	}
}

// TestConsoleGetsItsModeFromTheDropIn pins a silent bug: the pid1 image has no
// systemd, so the DASHBOARD_MODE the drop-in carries reached nothing and
// `dashboard.mode: tui` on a machine with a screen kept drawing the graphical
// face. The mode the drop-in names must end up in the console's environment.
func TestConsoleGetsItsModeFromTheDropIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "10-mode.conf")
	if err := os.WriteFile(path, firstboot.ConsoleDropInFile("tui"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := consoleEnv(path); !slices.Contains(got, "DASHBOARD_MODE=tui") {
		t.Errorf("consoleEnv = %v, want DASHBOARD_MODE=tui", got)
	}
}

// TestConsoleKeepsItsDefaultWithoutADropIn is the other half: before the node
// has configured itself the drop-in is absent, and that must read as "say
// nothing" (nil), not as an empty mode the console would then have to reject.
func TestConsoleKeepsItsDefaultWithoutADropIn(t *testing.T) {
	if got := consoleEnv(filepath.Join(t.TempDir(), "absent.conf")); got != nil {
		t.Errorf("consoleEnv on a missing drop-in = %v, want nil", got)
	}
}

// TestKubeletArgsAreFlagsOnly pins that kubeletArgs returns the flags alone: PID
// 1 supplies the program as argv[0] (it is /usr/local/bin/kubelet, the launcher
// symlink), so a program path repeated here would reach the kubelet as a stray
// positional argument.
func TestKubeletArgsAreFlagsOnly(t *testing.T) {
	args := kubeletArgsFromEnv(map[string]string{})

	tests := []struct {
		name string
		want string
	}{
		{"the kubelet configuration", "--config=/etc/kubelet/kubelet.conf"},
		{"the root dir", "--root-dir=/var/lib/kubelet"},
		{"the kubeconfig", "--kubeconfig=/etc/kubernetes/kubelet.conf"},
		{"the client CA", "--client-ca-file=/etc/kubernetes/pki/ca.crt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !slices.Contains(args, tt.want) {
				t.Errorf("kubeletArgsFromEnv() is missing %q: %v", tt.want, args)
			}
		})
	}

	for _, a := range args {
		if a == "/usr/local/bin/kubelet" {
			t.Errorf("the program path must not be in the flags (startChild supplies argv[0]): %v", args)
		}
	}
}
