package firstboot

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/configdrive"
	"github.com/vatesfr/vates-kube-os/vatescfg"
)

// BootstrapPhases are the kubeadm phases that can only run once the API server
// answers.
//
// The order matters. `bootstrap-token` creates the kubeadm:cluster-admins
// binding, and until that binding exists admin.conf has no permissions -- which
// is the same fact that forced kube-vip to use super-admin.conf. The addon phase
// talks to the API as admin, so it has to come second.
var BootstrapPhases = []string{
	// Publishes the cluster's configuration: the kubeadm-config ConfigMap and
	// the RBAC that lets a joining node read it, and the kubelet-config
	// ConfigMap with the RBAC for that.
	//
	// It is easy to leave this out, because a node that only bootstraps never
	// reads it. A node that JOINS does: `kubeadm join` asks the cluster what it
	// is joining. Without this phase the answer is a 403,
	//   configmaps "kubeadm-config" is forbidden: User "system:bootstrap:..."
	//   cannot get resource "configmaps"
	// which reads like a token that is too weak rather than like a phase that
	// was never run.
	"upload-config all",
	// Publishes the cluster's certificates so a joining control plane can fetch
	// them with the certificate key. Without it every joining control plane has
	// to be handed the CA privately, and `kubeadm join --control-plane` fails
	// with "the cluster does not have any uploaded certificates".
	"upload-certs --upload-certs",
	// Creates the bootstrap token, the cluster-info ConfigMap and the RBAC that
	// lets a node ask to join at all. It also creates the cluster-admins binding
	// that makes admin.conf usable, which the addon phase below depends on.
	"bootstrap-token",
	// Labels this node control-plane and taints it, which is what the ROLES
	// column of `kubectl get nodes` reads and what keeps ordinary workloads off
	// it.
	//
	// A JOINING control plane gets both from `kubeadm join`, which runs the same
	// phase -- but only from a mark this system does not leave to the join alone
	// (see MarkControlPlaneCommand and markControlPlane). Leaving this out
	// produces a cluster whose first control plane differs from the others: it
	// shows no role, and pods are placed on it. Measured, not inferred: with
	// this phase absent, `kubectl get nodes` printed vates-cp-1 with an empty
	// ROLES column beside the joining control planes marked control-plane.
	//
	// It has to run here rather than with the earlier phases: it writes to the
	// API, which only answers once the kubelet has started the static pods.
	"mark-control-plane",
	"addon all",
}

// bootstrapPhases returns the phases this machine runs: BootstrapPhases minus
// kube-proxy when the cluster's CNI replaces it. `addon all` installs kube-proxy;
// `addon coredns` installs only CoreDNS. A cluster whose CNI replaces
// kube-proxy must never have kube-proxy written at all -- deleting it afterwards
// leaves its iptables rules behind, which is why the choice is made here and not
// by a cleanup step.
//
// Only cni.plugin: none reaches that branch: the flannel and cilium the node
// installs itself both run alongside kube-proxy, and Validate refuses the
// other combinations (see the proxy.disabled check).
func bootstrapPhases(cfg *vatescfg.Config) []string {
	if !cfg.Cluster.Proxy.Disabled {
		return BootstrapPhases
	}
	phases := make([]string, 0, len(BootstrapPhases))
	for _, p := range BootstrapPhases {
		if p != "addon all" {
			phases = append(phases, p)
		}
	}
	return append(phases, "addon coredns")
}

// cniManifest returns the manifest of the CNI this node installs itself, and
// the path it is written to before being applied.
//
// flannel and cilium are both the node's to install: the pod network is applied
// here, on the bootstrapping control plane, and the DaemonSet (and the operator,
// for cilium) carries it to every node that joins. none is not: the CNI is the
// operator's to install from the cluster side, and the node writes nothing, so
// the path comes back empty and the caller says so out loud.
//
// Both manifests are embedded and pinned -- see FlannelManifest and
// CiliumManifest -- and the pod network they are told is the same field,
// cni.cidr: flannel is given the range directly, and cilium reads it from the
// node's podCIDR annotation, which kubeadm writes from it.
func cniManifest(cfg *vatescfg.Config) (path string, manifest []byte, err error) {
	switch cfg.CNI.Plugin {
	case vatescfg.CNIFlannel:
		manifest, err = FlannelManifest(cfg)
		return FlannelManifestPath, manifest, err
	case vatescfg.CNICilium:
		manifest, err = CiliumManifest(cfg)
		return CiliumManifestPath, manifest, err
	default: // CNINone
		return "", nil, nil
	}
}

// JoinAttempts is how many times a control plane's join is retried.
//
// More than one because the join is racy by construction -- see the retry loop
// in Bootstrap -- and few enough that a genuine failure still surfaces quickly.
const JoinAttempts = 4

// JoinRetryDelay is the pause between join attempts, long enough for a local
// etcd to start and catch up with the cluster.
const JoinRetryDelay = 20 * time.Second

// BootstrapTimeout is how long to wait for the API server after the kubelet has
// started the control plane's static pods.
//
// Generous, and deliberately so: etcd has to elect itself, the API server has to
// answer, and kube-vip has to claim the VIP -- all on a machine that is starting
// containers for the first time. Five minutes was measured to be too tight when
// anything upstream has been slow, and the failure it produces is misleading:
// the bootstrap gives up, the API server answers a moment later, and the node
// looks like a control plane that never got its addons.
const BootstrapTimeout = 10 * time.Minute

// Bootstrap completes a control plane node once its API server is reachable.
//
// This is deliberately a separate step from configuring a node, and a separate
// systemd unit: everything here needs a working API server, and the API server
// is a static pod that only starts once the kubelet is running. Configuration
// ends by starting the kubelet; this begins after it has worked.
//
// It runs ONCE per node: BootstrapDoneMarker is the guard, written only after the
// whole stage has succeeded. Without it a reboot redoes work that is already
// done, and measured, that is not merely slow -- it is a failure. A joining
// control plane that reboots re-runs `kubeadm join`, which now fails pre-flight
// (the node is already a member, its manifests exist, its kubelet already holds
// :10250), retries four times, spends about a minute, and reports "bootstrap
// failed" for a node that is perfectly healthy. The node that CREATES the
// cluster pays the mirror toll: it re-uploads the configuration, mints a fresh
// bootstrap token, and re-applies the addons and the CNI.
func Bootstrap(cfg *vatescfg.Config, drive *configdrive.Drive, paths Paths, nodeName, nodeIP string, r Runner) error {
	if bootstrapDone(r) {
		r.Logf("cluster bootstrap already done (%s): skipping", BootstrapDoneMarker)
		return nil
	}
	if err := bootstrapStage(cfg, drive, paths, nodeName, nodeIP, r); err != nil {
		return err
	}
	// Written only once the whole stage has succeeded, so an interrupted
	// bootstrap is retried whole rather than half-skipped.
	if err := r.WriteFile(BootstrapDoneMarker, 0o600, []byte(cfg.Kubernetes.Version+"\n")); err != nil {
		return fmt.Errorf("writing %s: %w", BootstrapDoneMarker, err)
	}
	return nil
}

// bootstrapStage is the work of Bootstrap, before the idempotency guard: it
// either joins an existing cluster (a joining control plane) or creates the
// cluster-wide pieces (the node that bootstrapped), and nothing on a worker.
func bootstrapStage(cfg *vatescfg.Config, drive *configdrive.Drive, paths Paths, nodeName, nodeIP string, r Runner) error {
	if cfg.Role != vatescfg.RoleMaster {
		// The cluster-wide addons are applied once, by a control plane. A worker
		// has nothing to do here, and saying so is better than a silent success
		// that looks like it did something.
		r.Logf("role %s: nothing to bootstrap, cluster-wide addons are applied by a control plane", cfg.Role)
		return nil
	}

	// A control plane that JOINED finishes here, not by applying addons: the
	// cluster already has them. What is left is to promote this machine's etcd
	// member from learner to voter, which could only happen once its own etcd was
	// running -- that is, once the kubelet this stage follows had started it.
	if JoiningControlPlane(cfg, drive) {
		// waitForEndpoint, NOT waitForAPI: this node has no kubeconfig yet --
		// kubeadm writes one during the join below -- so a check that reads one
		// fails with
		//   error: stat /etc/kubernetes/super-admin.conf: no such file or directory
		// which names a file rather than the ordering mistake that is really
		// there. The endpoint answering is all this needs to know.
		r.Progress("waiting for the control plane endpoint")
		if err := waitForEndpoint(cfg.Cluster.ControlPlaneEndpoint, BootstrapTimeout, r); err != nil {
			return err
		}
		if err := joinControlPlane(nodeName, r); err != nil {
			return err
		}
		return stageJoiningKubeVIP(cfg, nodeIP, r)
	}

	r.Progress("waiting for the API server")
	if err := waitForAPI(BootstrapTimeout, r); err != nil {
		return err
	}
	r.Progress("applying the cluster configuration")

	// The CNI manifest is written before the addons are applied so that, if the
	// addon phase fails, the file that explains what should be running is
	// already on disk to be read and applied by hand. A CNI the node does not
	// install (cni.plugin: none) writes nothing: the operator installs it.
	cniPath, cniData, err := cniManifest(cfg)
	if err != nil {
		return err
	}
	if cniPath != "" {
		if err := r.WriteFile(cniPath, 0o644, cniData); err != nil {
			return fmt.Errorf("writing the CNI manifest: %w", err)
		}
		r.Logf("wrote the CNI manifest to %s", cniPath)
	}

	phasesStart := time.Now()
	for _, phase := range bootstrapPhases(cfg) {
		start := time.Now()
		args := KubeadmPhaseCommand(phase)
		if _, err := r.Run(args[0], args[1:]...); err != nil {
			return fmt.Errorf("kubeadm phase %q: %w", phase, err)
		}
		r.Logf("kubeadm init phase %s (took %s)", phase, elapsed(start))
	}
	r.Logf("bootstrap phases took %s in total", elapsed(phasesStart))

	// The CNI itself. Without it the node stays NotReady -- the kubelet reports
	// "network plugin is not ready" -- no pod can be scheduled, and CoreDNS,
	// which the addon phase just created, never gets an address. For cilium the
	// same is true while the operator creates the CRDs the agent waits on, which
	// is why the manifest carries the operator: in this chart the CRDs are no
	// longer a file to apply, they are created by it. With cni.plugin: none the
	// node installs nothing and says so: it stays NotReady until the operator
	// applies the cluster's CNI, which is the contract.
	if cniPath != "" {
		r.Progress("applying the pod network")
		start := time.Now()
		apply := kubectlArgs("--kubeconfig="+SuperAdminKubeconfig,
			"apply", "-f", cniPath)
		if _, err := r.Run(apply[0], apply[1:]...); err != nil {
			return fmt.Errorf("applying the CNI manifest: %w", err)
		}
		r.Logf("applied the CNI manifest (took %s)", elapsed(start))
	} else {
		r.Progress("no CNI applied (cni.plugin: " + cfg.CNI.Plugin + "); the cluster's CNI is installed from the cluster side")
		r.Logf("the node stays NotReady until the cluster's CNI is installed")
	}

	// The console dashboard's read-only access to events, on every node. Written
	// to disk first, like the CNI manifest, so that a failure to apply leaves the
	// file that says what should exist.
	if err := r.WriteFile(NodeDashboardRBACPath, 0o644, NodeDashboardRBAC()); err != nil {
		return fmt.Errorf("writing the dashboard RBAC: %w", err)
	}
	r.Progress("granting the console read access")
	grantStart := time.Now()
	grant := kubectlArgs("--kubeconfig="+SuperAdminKubeconfig,
		"apply", "-f", NodeDashboardRBACPath)
	if _, err := r.Run(grant[0], grant[1:]...); err != nil {
		return fmt.Errorf("applying the dashboard RBAC: %w", err)
	}
	r.Logf("granted the nodes read access to events (%s) (took %s)", "vates:node-dashboard", elapsed(grantStart))

	// The cloud provider's own manifests, when vates-node.yaml names them.
	// Optional and normally ABSENT under CAPI, where a ClusterResourceSet
	// carries them -- and their credentials -- from the cluster side. Where
	// they are present, they are applied last and by URL, so a node booted
	// with no cluster-side applier still gets its CCM and CSI. Applied from
	// the cluster's admin kubeconfig, the same as the CNI and the RBAC above.
	for i, m := range cfg.Cloud.Manifests {
		r.Progress(fmt.Sprintf("applying the cloud manifests (%d/%d)", i+1, len(cfg.Cloud.Manifests)))
		apply := kubectlArgs("--kubeconfig="+SuperAdminKubeconfig, "apply", "-f", m)
		if _, err := r.Run(apply[0], apply[1:]...); err != nil {
			return fmt.Errorf("applying cloud.manifests[%d] (%s): %w", i, m, err)
		}
		r.Logf("applied the cloud manifest %s", m)
	}

	return nil
}

// joinControlPlane runs `kubeadm join` for this control plane and then applies
// the control-plane mark through kubeadm's own phase.
//
// Split from Bootstrap so the sequence -- join, retried, then mark-control-plane
// -- can be asserted without a network or a cluster.
//
// The join is retried, because it contains a race that is Kubernetes' own and
// not a defect of this system: its etcd phase announces this machine as a
// LEARNER, writes the etcd manifest -- which the kubelet then turns into a
// running etcd -- and promotes the learner IMMEDIATELY, without waiting for it
// to have started. A promotion of a learner that is not yet in sync fails with
//
//	can only promote a learner member which is in sync with leader
//
// even though etcd comes up a second later. Everything before that point is
// idempotent, so running the join again finds the member already announced, the
// manifests already written and etcd running, and the promotion succeeds.
// Bounded, and the last error is reported.
func joinControlPlane(nodeName string, r Runner) error {
	args := JoinControlPlaneCommand()
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= JoinAttempts; attempt++ {
		r.Progress(fmt.Sprintf("joining the cluster (attempt %d of %d)", attempt, JoinAttempts))
		out, err := r.Run(args[0], args[1:]...)
		if err == nil {
			r.Logf("kubeadm join (control plane), attempt %d (took %s)", attempt, elapsed(start))
			// The join succeeding is not the same as this node's control plane
			// being up: kubeadm promotes this machine's etcd member from learner
			// to voter, but the API server static pod starts as soon as its
			// manifest is written, which can be before the promotion finishes.
			// Wait for this node's OWN API server to report ready -- /readyz
			// includes the etcd check -- so the control plane is not called
			// done while its etcd is still a learner.
			if err := waitForJoinedControlPlane(r); err != nil {
				r.Logf("%v; continuing, the join itself succeeded", err)
			}
			return markControlPlane(nodeName, r)
		}
		lastErr = err
		r.Logf("kubeadm join attempt %d did not complete: %v", attempt, err)
		_ = out
		if attempt < JoinAttempts {
			time.Sleep(JoinRetryDelay)
		}
	}
	return fmt.Errorf("kubeadm join (control plane) after %d attempts: %w", JoinAttempts, lastErr)
}

// JoinedControlPlaneReadyTimeout bounds the wait, after a join, for this node's
// own API server to report ready. Generous, because it is best-effort and a slow
// etcd is exactly the condition this system has been seen under.
const JoinedControlPlaneReadyTimeout = 90 * time.Second

// waitForJoinedControlPlane waits for the API server this node just joined with
// to answer /readyz, which includes the etcd check.
//
// It reads admin.conf, the kubeconfig kubeadm writes DURING the join -- the
// first credential a joined control plane has, and why this runs here and not
// when the node was configured. A learner member answers
//
//	etcdserver: rpc not supported for learner
//
// until kubeadm promotes it, and /readyz fails for that window; waiting on
// /readyz is waiting for the promotion, plus for the rest of the control plane.
//
// Best-effort: the join has already succeeded, so returning an error here would
// turn a working control plane into a failed bootstrap over a slow disk. A
// timeout is reported and the caller continues.
func waitForJoinedControlPlane(r Runner) error {
	deadline := time.Now().Add(JoinedControlPlaneReadyTimeout)
	args := kubectlArgs("--kubeconfig=/etc/kubernetes/admin.conf", "get", "--raw", "/readyz")
	for {
		out, err := r.Run(args[0], args[1:]...)
		if err == nil && strings.Contains(string(out), "ok") {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("this control plane's own API server did not report ready within %s", JoinedControlPlaneReadyTimeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// MarkAttempts and MarkRetryDelay bound the wait for the node to be registered
// before the control-plane mark can stick.
//
// kubeadm's phase patches the node through apiclient.PatchNode, which gives up
// WITHOUT an error when the node does not yet carry the kubernetes.io/hostname
// label: it polls to its timeout and returns nil. So the phase can exit 0
// having marked nothing, and the loop below decides success by the LABEL, not
// the exit code. This is the race the join itself loses (see
// MarkControlPlaneCommand): this system starts the kubelet before the join, so
// when the node appears is its own timing.
const (
	MarkAttempts   = 12
	MarkRetryDelay = 15 * time.Second
)

// markAttempts and markRetryDelay are the values the loop uses; tests shrink the
// delay so the retry can be asserted without waiting.
var (
	markAttempts   = MarkAttempts
	markRetryDelay = MarkRetryDelay
)

// markControlPlane applies the control-plane label and taint to this node,
// through kubeadm's own mark-control-plane phase, and verifies it took.
//
// The phase is retried until the node carries node-role.kubernetes.io/control-plane,
// because the phase itself reports success even when it found no node to mark.
func markControlPlane(nodeName string, r Runner) error {
	args := MarkControlPlaneCommand()
	var lastErr error
	for attempt := 1; attempt <= markAttempts; attempt++ {
		r.Progress("marking the node as a control plane")
		if _, err := r.Run(args[0], args[1:]...); err != nil {
			lastErr = err
			r.Logf("mark-control-plane attempt %d did not complete: %v", attempt, err)
		}
		if hasControlPlaneLabel(nodeName, r) {
			r.Logf("node marked as a control plane")
			return nil
		}
		if attempt < markAttempts {
			time.Sleep(markRetryDelay)
		}
	}
	if lastErr != nil {
		return fmt.Errorf("kubeadm join phase control-plane-join mark-control-plane: %w", lastErr)
	}
	return fmt.Errorf("node %s was not marked as a control plane after %d attempts", nodeName, markAttempts)
}

// hasControlPlaneLabel reports whether the node carries the control-plane role
// label. The label's VALUE is empty, so its presence is what is checked: a
// jsonpath on the value cannot tell "" from absent.
func hasControlPlaneLabel(nodeName string, r Runner) bool {
	args := kubectlArgs("--kubeconfig="+AdminKubeconfig,
		"get", "node", nodeName, "-o", "jsonpath={.metadata.labels}")
	out, err := r.Run(args[0], args[1:]...)
	return err == nil && strings.Contains(string(out), "node-role.kubernetes.io/control-plane")
}

// stageJoiningKubeVIP installs the kube-vip static pod on a control plane that
// has just joined the cluster.
//
// After the join, not before, because of what the manifest mounts: kube-vip
// reads admin.conf, and kubeadm writes that file DURING the join. A manifest
// staged earlier names a file that does not exist yet, the kubelet refuses the
// mount --
//
//	hostPath type check failed: /etc/kubernetes/admin.conf is not a file
//
// -- and retries it on a two-minute backoff. The pod does not run until then,
// which is exactly what left the VIP with a single possible owner: measured,
// killing that one node removed the control plane's address entirely.
func stageJoiningKubeVIP(cfg *vatescfg.Config, nodeIP string, r Runner) error {
	if !cfg.Cluster.VIP.Enabled() {
		return nil
	}
	m, err := KubeVIPManifest(cfg.Cluster.VIP.Address, cfg.VIPInterface(), "6443", AdminKubeconfig, nodeIP, cfg.ImageFor(KubeVIPImage))
	if err != nil {
		return err
	}
	if err := r.WriteFile(filepath.Join(ManifestsDir, "kube-vip.yaml"), 0o600, m); err != nil {
		return fmt.Errorf("writing the kube-vip manifest: %w", err)
	}
	r.Logf("staged the kube-vip static pod, now that the join has written admin.conf")
	return nil
}

// waitForAPI polls the control plane's health endpoint until it answers.
//
// It polls through kubectl from the same image the control plane runs from, with
// the same kubeconfig kube-vip uses, so it exercises exactly the path everything
// else will: the endpoint from vates-node.yaml, reached through the VIP.
func waitForAPI(timeout time.Duration, r Runner) error {
	deadline := time.Now().Add(timeout)
	start := time.Now()
	args := kubectlArgs("--kubeconfig="+SuperAdminKubeconfig, "get", "--raw", "/healthz")

	for attempt := 0; ; attempt++ {
		out, err := r.Run(args[0], args[1:]...)
		if err == nil && strings.Contains(string(out), "ok") {
			if attempt > 0 {
				r.Logf("the API server answered after %d attempt(s), %s", attempt+1, elapsed(start))
			}
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the API server did not answer within %s\n"+
				"  Its address is the control plane endpoint in vates-node.yaml, reached\n"+
				"  through the VIP. If the VIP is not up, kube-vip is the thing to look\n"+
				"  at: kubectl -n kube-system get pod -l k8s-app=kube-vip, and its log\n"+
				"  under /var/log/pods",
				timeout)
		}
		if attempt == 0 {
			r.Logf("waiting for the API server at the control plane endpoint")
		}
		time.Sleep(5 * time.Second)
	}
}

// kubectlArgs runs the node's kubectl, through the launcher.
func kubectlArgs(args ...string) []string {
	return append([]string{KubectlPath}, args...)
}
