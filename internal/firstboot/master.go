package firstboot

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vatesfr/vates-kube-os/internal/configdrive"
	"github.com/vatesfr/vates-kube-os/vatescfg"
)

// KubeadmPath and KubectlPath are where the launcher's faces live on the host.
//
// The Kubernetes binaries are not in the image: the launcher fetches them at
// first boot into /var/lib/vates/kubernetes, and these are the names it answers
// to on the host. Running kubeadm or kubectl is therefore an ordinary exec that
// the launcher turns into a fetch, verify and run. See docs/ARCHITECTURE.md.
const (
	KubeadmPath = "/usr/local/bin/kubeadm"
	KubectlPath = "/usr/local/bin/kubectl"
)

// KubeadmConfigPath is where vates-init writes the document the phases read.
//
// Not kubeadm's default name: being explicit means the file that produced a
// given cluster is unambiguous, and a stray kubeadm-config.yaml left by hand
// cannot silently take precedence.
const KubeadmConfigPath = "/etc/kubernetes/kubeadm.yaml"

// EtcdDataDir is where etcd keeps its data. It is a hostPath in the etcd static
// pod manifest, so it must exist before the kubelet starts that pod.
const EtcdDataDir = "/var/lib/etcd"

// PatchesDir is where kubeadm looks for the patch files named by
// `patches.directory` in the init and join documents. vates-init writes it
// before any kubeadm command runs, for both the bootstrapping phase list and
// the join.
const PatchesDir = "/etc/kubernetes/patches"

// kubeAPIServerPatchName is the patch file's name, in kubeadm's convention
// `<target>+<patchtype>.<extension>`: the kube-apiserver static pod, applied as
// a strategic merge.
const kubeAPIServerPatchName = "kube-apiserver+strategic.yaml"

// KubeAPIServerProbesPatch keeps etcd out of BOTH of the API server's probes.
//
// kubeadm's defaults are /livez for liveness and /readyz for readiness, and both
// include an etcd check. On a shared NFS SR an etcd round-trip costs ~20 ms
// against ~0.7 ms on a local disk (measured), reads pass 100 ms, and both checks
// answer 500. Measured on a three-control-plane demo: the etcd leader logged
// "leader is overloaded likely from slow disk", all three API servers answered 500
// to the readiness probe within the same second, and it produced ~77 Node
// `Unhealthy` events on an otherwise idle cluster.
//
// Those events are noise, and it is worth being precise about why, because the
// obvious reading -- "the API server fell out of the `kubernetes` Service
// endpoints" -- is WRONG. The `kubernetes` endpoints are not built from pod
// readiness. They are owned by the API server's own endpoint reconciler
// (`--endpoint-reconciler-type` defaults to `lease`): every API server refreshes a
// TTL'd lease record in etcd, and whichever reconciler runs rebuilds the address
// list from the live leases. Pod readiness is never consulted -- the Endpoints
// carries `endpointslice.kubernetes.io/skip-mirror: "true"` and the API server
// manages the EndpointSlice itself (observed `managedFields` manager:
// kube-apiserver). A readiness flap marked the Pod NotReady and fired events; it
// moved no traffic.
//
// What the checks did cost: that noise, and a real restart risk on the liveness
// side -- past its `failureThreshold: 8` x `periodSeconds: 10` a slow etcd
// restarts the API server, which cannot help an etcd that is merely slow.
//
// Removing them leaves nothing unsupervised:
//
//   - etcd has its OWN static-pod liveness probe (`/livez` on the metrics port,
//     `periodSeconds: 10`, `failureThreshold: 8`): a dead or hung etcd is
//     restarted by the kubelet, independently of the API server;
//   - the LEASE is what evicts an API server that can no longer reach its etcd.
//     kubeadm points each API server at its LOCAL etcd only
//     (`--etcd-servers=https://127.0.0.1:2379`); once that etcd is gone the lease
//     cannot be refreshed, it expires, and the other API servers drop the IP from
//     the endpoints. The safety property survives -- it was never the probe's.
//
// Two things are easy to get wrong here, and both were measured on a live API
// server:
//
//   - READINESS has TWO etcd checks, `etcd` and `etcd-readiness`. Excluding only
//     `etcd` leaves `etcd-readiness` running, and the readiness still answers 500
//     on a slow etcd -- it LOOKS patched and is not. Both must go;
//   - `exclude` takes one name per parameter and is NOT comma-separated:
//     `?exclude=etcd,etcd-readiness` matches nothing (the server warns "no
//     matches for ...") and excludes nothing. The parameter is repeated instead.
//
// The liveness probe carries only `etcd`, so one exclusion is enough there.
// Talos runs no probe at all on its API server; this is the same choice, kept
// narrow to preserve what kubeadm's probes were actually for.
func KubeAPIServerProbesPatch() []byte {
	return []byte(`# Written by vates-init. See KubeAPIServerProbesPatch for why both probes
# drop the etcd checks that kubeadm's default /livez and /readyz carry.
apiVersion: v1
kind: Pod
metadata:
  name: kube-apiserver
  namespace: kube-system
spec:
  containers:
  - name: kube-apiserver
    livenessProbe:
      httpGet:
        path: /livez?exclude=etcd
    readinessProbe:
      httpGet:
        path: /readyz?exclude=etcd&exclude=etcd-readiness
`)
}

// RequiredDirs are the host directories that must exist before the kubelet is
// started, for either role.
//
// These are not conveniences: each one is a hostPath a static pod manifest names,
// or a directory the kubelet or the launcher writes into. A hostPath that does
// not exist is a pod that never starts, and nothing else in the image creates
// them.
//
// The manifests directory matters especially for a control plane: a manifest
// written after the kubelet has started is eventually picked up, but the VIP must
// be there from the first scan, because the API server is reachable at it.
func RequiredDirs(cfg *vatescfg.Config) []string {
	dirs := []string{
		KubeletRootDir,
		// The kubelet reads its KubeletConfiguration from here through
		// --config.
		"/etc/kubelet",
		// Where this node's drop-in is written.
		filepath.Dir(KubeletDropIn),
		// Where the launcher caches the Kubernetes binaries it fetches. It is on
		// the writable /var, and the kubelet needs it at every start.
		BinariesDir,
	}
	// The pod network's runtime state lives on the tmpfs /run, which PID 1
	// mounts and which a reboot wipes, so it has to be recreated every boot.
	// It is the CNI's, so it is chosen with the CNI -- a node creates only what
	// its own CNI needs.
	dirs = append(dirs, CNIRunDirs(cfg.CNI.Plugin)...)
	if cfg.Role == vatescfg.RoleMaster {
		dirs = append(dirs, EtcdDataDir, ManifestsDir)
	}
	return dirs
}

// CNIRunDirs are the pod network's runtime directories, on the tmpfs /run.
//
// They are the CNI's, so they are chosen with the CNI:
//   - flanneld writes the pod network's subnet file into /run/flannel, and the
//     CNI plugin on the host reads it back to configure every pod's sandbox.
//   - the Cilium agent keeps its state under /run/cilium and the pod network
//     namespaces at /run/netns.
//   - "none" installs no CNI, so the node creates nothing for it.
//
// They are also labelled for containers (see ContainerPaths), and labelling a
// path that does not exist fails -- which is how a fresh node once failed to
// configure itself while an already-configured one worked.
func CNIRunDirs(plugin string) []string {
	switch plugin {
	case vatescfg.CNICilium:
		return []string{"/run/cilium", "/run/netns"}
	case vatescfg.CNINone:
		return nil
	default: // CNIFlannel
		return []string{"/run/flannel"}
	}
}

// MasterFiles computes what a bootstrapping control plane node needs on disk
// before the kubelet is started.
//
// "Bootstrapping" is the important word: this is the node that CREATES the
// cluster, so kubeadm generates the certificate authority and every certificate.
// Control plane nodes joining an existing cluster are a different case -- they
// must receive the cluster's PKI rather than mint their own, or there would be
// two certificate authorities and the cluster would split.
func MasterFiles(cfg *vatescfg.Config, drive *configdrive.Drive, paths Paths, nodeName, nodeIP string) ([]File, error) {
	kubeletConf, err := kubeletConfigFile(cfg, paths)
	if err != nil {
		return nil, err
	}

	var files []File
	if JoiningControlPlane(cfg, drive) {
		// A control plane joining an existing cluster is told how to reach it
		// rather than how to build one.
		if cfg.Cluster.Token == "" {
			return nil, fmt.Errorf("a joining control plane requires cluster.token: " +
				"kubeadm uses it for discovery, and the kubelet uses it to obtain its own certificate")
		}
		// The cluster CA is required on the drive, whether or not the drive also
		// carries the whole PKI: the kubelet verifies the API server with it
		// while it bootstraps, before kubeadm has fetched anything.
		if cfg.PKI.ClusterCA.Cert == "" && !drive.Has(pkiCACert) {
			return nil, fmt.Errorf("a joining control plane requires the cluster CA certificate " +
				"(pki.clusterCA.cert in vates-node.yaml, or pki/ca.crt on the config drive)")
		}
		if !drive.Has(pkiCAKey) && cfg.Cluster.CertificateKey == "" {
			return nil, fmt.Errorf("a joining control plane needs either the cluster's PKI " +
				"(pki/ca.key) or a certificate key in vates-node.yaml: without one, " +
				"kubeadm cannot obtain the control-plane certificates")
		}
		// Two ways to join, and the drive says which:
		//   - the cluster's PKI, keys included (pki/ca.key): kubeadm signs this
		//     node's certificates locally with those authorities
		//   - a certificate key (vates-node.yaml): kubeadm fetches them from the
		//     cluster
		if drive.Has(pkiCAKey) {
			pki, err := controlPlanePKIFiles(drive, paths)
			if err != nil {
				return nil, err
			}
			files = append(files, pki...)
		} else {
			content, err := clusterCACert(cfg, drive)
			if err != nil {
				return nil, err
			}
			files = append(files, File{
				Path:    filepath.Join(paths.Kubernetes, "pki", "ca.crt"),
				Mode:    0o644,
				Content: content,
			})
		}

		// The kubelet's credentials, on EITHER join path: it bootstraps its own
		// certificate from the token. kubeadm would write kubelet.conf through
		// its kubelet-start phase, which this system skips -- so without this the
		// containerised kubelet dies in a restart loop with
		//   invalid kubeconfig: stat /etc/kubernetes/kubelet.conf: no such file
		files = append(files, bootstrapKubeletConfFile(cfg, paths))

		joinCfg, err := JoinConfiguration(cfg, nodeName, nodeIP)
		if err != nil {
			return nil, err
		}
		files = append(files, File{
			Path:    JoinConfigPath,
			Mode:    0o600,
			Content: joinCfg,
		})
	} else {
		// A bootstrapping control plane normally generates the cluster CA. When
		// the provider states one in the document, the node REUSES it instead:
		// writing the files before kubeadm runs makes kubeadm keep them, so
		// every control plane shares one authority from the first machine -- the
		// model a CAPI control plane provider owns (it holds the CA, no node
		// does).
		if cfg.PKI.ClusterCA.Cert != "" {
			files = append(files, File{
				Path:    filepath.Join(paths.Kubernetes, "pki", "ca.crt"),
				Mode:    0o644,
				Content: ensureNewline(cfg.PKI.ClusterCA.Cert),
			})
			if cfg.PKI.ClusterCA.Key != "" {
				files = append(files, File{
					Path:    filepath.Join(paths.Kubernetes, "pki", "ca.key"),
					Mode:    0o600,
					Content: ensureNewline(cfg.PKI.ClusterCA.Key),
				})
			}
		}
		kubeadmCfg, err := KubeadmConfig(cfg, nodeName, nodeIP)
		if err != nil {
			return nil, err
		}
		files = append(files, File{
			Path:    KubeadmConfigPath,
			Mode:    0o600,
			Content: kubeadmCfg,
		})
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
				cfg.BinaryBase(), bootstrapFor(cfg, drive), cfg.Cloud.Provider),
		},
	)

	// The virtual IP is owned by a static pod, so its manifest goes in the same
	// directory as etcd's and the API server's. On a BOOTSTRAPPING control plane
	// it has to be present before the kubelet's first scan of that directory,
	// because the API server is reachable at the VIP and kube-vip is what puts it
	// there.
	//
	// A JOINING control plane is the exception, and not for tidiness: the
	// kubeconfig kube-vip needs there is admin.conf, which kubeadm writes DURING
	// the join -- so a manifest written now would name a file that does not exist
	// yet and the kubelet would refuse the mount. JoiningControlPlane stages it
	// once the join has run. See KubeVIPKubeconfigFor.
	if cfg.Cluster.VIP.Enabled() && !JoiningControlPlane(cfg, drive) {
		m, err := KubeVIPManifest(cfg.Cluster.VIP.Address, cfg.VIPInterface(), "6443", KubeVIPKubeconfigFor(cfg, drive), nodeIP, cfg.ImageFor(KubeVIPImage))
		if err != nil {
			return nil, err
		}
		files = append(files, File{
			Path:    filepath.Join(ManifestsDir, "kube-vip.yaml"),
			Mode:    0o600,
			Content: m,
		})
	}

	// The API server's probes must not depend on etcd, and kubeadm's defaults
	// both do. The patch is written for BOTH join paths: a joining control
	// plane's kubeadm join reads it from the join document's
	// patches.directory, exactly as the bootstrapping phases read it from the
	// init document's.
	files = append(files, File{
		Path:    filepath.Join(PatchesDir, kubeAPIServerPatchName),
		Mode:    0o644,
		Content: KubeAPIServerProbesPatch(),
	})

	return files, nil
}

// KubeadmPhaseCommand builds the command that runs one kubeadm init phase.
//
// The phases are run individually rather than through `kubeadm init` because
// `kubeadm init` also installs and starts a kubelet; this system starts its own
// kubelet as a host process, before the phase list runs.
//
// phase is a complete phase path such as "etcd local"; it is split into
// arguments here so the callers cannot forget that etcd has no `all`.
func KubeadmPhaseCommand(phase string) []string {
	args := []string{KubeadmPath, "init", "phase"}
	args = append(args, strings.Fields(phase)...)
	args = append(args, "--config", KubeadmConfigPath)
	return args
}
