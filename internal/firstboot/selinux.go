package firstboot

import (
	"fmt"
	"strings"

	"github.com/vatesfr/vates-kube-os/vatescfg"
)

// SELinuxTypeForContainers is the file type container processes are allowed to
// read. The policy grants container_t access to container_file_t and to nothing
// else relevant here: kubernetes_file_t (the label /etc/kubernetes carries by
// default) and var_lib_t (the label /var/lib/etcd carries) are both denied.
const SELinuxTypeForContainers = "container_file_t"

// SELinuxState is what getenforce reported.
type SELinuxState int

const (
	// SELinuxUnknown means getenforce could not be run, so nothing is known.
	SELinuxUnknown SELinuxState = iota
	SELinuxDisabled
	SELinuxEnabled
)

func (s SELinuxState) String() string {
	switch s {
	case SELinuxDisabled:
		return "Disabled"
	case SELinuxEnabled:
		return "Enforcing or Permissive"
	default:
		return "unknown"
	}
}

// selinuxState asks getenforce.
//
// It is run from vates-init, which runs on the host and can therefore see the
// real state. The kubelet cannot: it runs in a container without /sys/fs/selinux,
// so getenforce answers "Disabled" there whatever the host says. That is why the
// labelling is done here rather than left to the kubelet's own volume
// relabelling, which silently never happens.
func selinuxState(r Runner) SELinuxState {
	out, err := r.Run("getenforce")
	if err != nil {
		return SELinuxUnknown
	}
	switch strings.TrimSpace(string(out)) {
	case "Enforcing", "Permissive":
		return SELinuxEnabled
	case "Disabled":
		return SELinuxDisabled
	}
	return SELinuxUnknown
}

// ContainerPaths are the host paths that containers must be able to reach.
//
// "Reach" and not "read": the CNI directories are written by a DaemonSet, and a
// container that cannot write them fails in a way that does not mention
// permissions at all.
//
// /etc/kubernetes always: every static pod mounts it read-only, and the
// kube-proxy DaemonSet does too, on workers as well as control planes.
//
// /var/lib/etcd on a control plane: etcd reads and writes its data there, and it
// is not a privileged container.
//
// /opt/cni/bin and /etc/cni/net.d always: the kube-flannel DaemonSet installs
// the CNI plugin binary and its configuration there from its init containers.
// Labelled etc_t and usr_t by default, they are writable only by host processes,
// and the init container answers
//
//	cp: can't create '/opt/cni/bin/flannel': File exists
//
// which reads like a leftover file rather than like a permission the policy
// withheld. Without these two, the node stays NotReady with "cni plugin not
// initialized" and no pod anywhere in the cluster can start.
//
// The pod network's runtime state under /run, always (see CNIRunDirs): the
// directories are the CNI's, so they are labelled with the CNI. flanneld writes
// the pod network's subnet file into /run/flannel, and the CNI plugin on the
// host reads it to configure every pod's sandbox. Labelled container_var_run_t,
// it is not writable by container_t, and flanneld reports
//
//	Failed to write subnet file: open /run/flannel/.subnet.env: permission denied
//
// while appearing to run perfectly -- the DaemonSet is 1/1 Running. What fails
// instead is every pod's sandbox, with a message pointing at the CNI plugin
// rather than at a directory permission. The Cilium agent keeps its state under
// /run/cilium for the same reason.
//
// /var/lib/kubelet always: the kubelet writes each pod's volumes under it --
// ConfigMap files, and the secret and downward-API items of the projected token
// volume -- and those files are bind-mounted INTO the pod's containers, which
// run as container_t. Labelled var_lib_t, they are unreadable, and the pod
// answers
//
//	cp: can't stat '/etc/kube-flannel/cni-conf.json': Permission denied
//
// This one is easy to dismiss as unnecessary, because the only container that
// mounts /var/lib/kubelet itself is the kubelet's own, and that one runs
// privileged. What matters is not the directory's direct consumer but the pod
// volume directories inside it.
func ContainerPaths(cfg *vatescfg.Config) []string {
	paths := []string{
		"/etc/kubernetes",
		"/opt/cni/bin",
		"/etc/cni/net.d",
		"/var/lib/kubelet",
		// The fetched Kubernetes binaries. A file the container wrote has the
		// directory's type, and executing it needs a type the container domain can
		// execute -- without it the launcher downloads a kubelet it is then not
		// allowed to run.
		BinariesDir,
	}
	// The pod network's runtime state is the CNI's, so a node labels only what
	// its own CNI writes (see CNIRunDirs).
	paths = append(paths, CNIRunDirs(cfg.CNI.Plugin)...)
	if cfg.Role == vatescfg.RoleMaster {
		paths = append(paths, EtcdDataDir)
	}
	return paths
}

// labelForContainers applies SELinuxTypeForContainers to the paths containers
// have to read.
//
// Done here, by the node's own configuration step, rather than left to the
// kubelet. The kubelet is supposed to relabel a pod's hostPath volumes to that
// pod's label, and on a normal node it does -- but relabelling is performed by
// exec'ing getenforce and chcon, and it decides SELinux is off when getenforce
// fails. In this system the kubelet runs in a container that has no
// /sys/fs/selinux, so getenforce answers "Disabled", the kubelet concludes there
// is nothing to relabel, and it says nothing about it. containerd, configured
// with enable_selinux = true, then applies a label regardless, and every control
// plane container dies with
//
//	open /etc/kubernetes/pki/sa.key: permission denied
//	open /var/lib/etcd: permission denied
//
// against files that are root-owned and look perfectly readable. Labelling them
// here fixes it in Enforcing mode, which is where it must work.
func labelForContainers(cfg *vatescfg.Config, r Runner) error {
	switch state := selinuxState(r); state {
	case SELinuxDisabled:
		r.Logf("SELinux is %s; no labelling needed", state)
		return nil
	case SELinuxUnknown:
		// Not silent. If SELinux is in fact enforcing and the paths are not
		// labelled, the control plane containers will be denied and the error
		// will point at a file rather than at this.
		r.Logf("WARNING: could not determine the SELinux state (getenforce failed); skipping labelling")
		return nil
	default:
		r.Logf("SELinux is %s; labelling the paths containers must read", state)
	}

	for _, p := range ContainerPaths(cfg) {
		if _, err := r.Run("chcon", "-R", "-t", SELinuxTypeForContainers, p); err != nil {
			return fmt.Errorf(
				"labelling %s as %s: %w "+
					"(containers run as container_t, and the policy allows them "+
					"container_file_t alone -- for reading as much as for writing; "+
					"without this, a control plane container dies on a permission "+
					"error against a file that looks perfectly readable, or a "+
					"DaemonSet fails to install its CNI plugin with \"File exists\")",
				p, SELinuxTypeForContainers, err)
		}
		r.Logf("labelled %s as %s", p, SELinuxTypeForContainers)
	}
	return nil
}
