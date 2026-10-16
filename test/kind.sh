#!/bin/bash
# Prove the embedded CNI manifest works, on a kind cluster.
#
# kind runs Kubernetes nodes as containers, so this needs no image build and no
# KVM: it runs on a CI runner. What it proves is the MANIFEST the node applies
# -- rendered by the node's own code (test/kind/render), pins and all -- on a
# two-node cluster with kube-proxy and no CNI of its own: the pod network comes
# up, DNS and a ClusterIP Service answer across nodes, and pod-to-pod traffic
# crosses nodes. What it does not prove is the OS around it (sysinit, firstboot,
# the kernel): that is still `make cluster CNI=...`.
#
#   test/kind.sh                 # CNI=cilium
#   CNI=flannel test/kind.sh
#   KEEP=1 test/kind.sh          # leave the cluster up to look at it
#
# kind, kubectl and go must be on PATH; KIND_EXPERIMENTAL_PROVIDER=podman is
# passed through to kind.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

CNI="${CNI:-cilium}"
NAME="${KIND_CLUSTER:-vates-kind-${CNI}}"
DOC="${DOC:-image/config/vates-node.yaml}"
WORK="$(mktemp -d)"
KUBECONFIG="${WORK}/kubeconfig"
export KUBECONFIG
# The test workloads: one image that serves HTTP and has a client in it.
AGNHOST="registry.k8s.io/e2e-test-images/agnhost:2.53"

say() { printf '\n==> %s\n' "$*"; }
die() { echo "FATAL: $*" >&2; exit 1; }

case "${CNI}" in
  cilium) ds_ns=kube-system ds=cilium ;;
  flannel) ds_ns=kube-flannel ds=kube-flannel-ds ;;
  *) die "CNI=${CNI} is not one of flannel, cilium" ;;
esac

# What went wrong, before the cluster is torn down: the CNI's pods and their
# events are the whole story when a sandbox cannot be created.
dump() {
  echo "--- the cluster at the failure:" >&2
  kubectl get nodes -o wide >&2 || true
  kubectl get pods -A -o wide >&2 || true
  kubectl -n "${ds_ns}" describe pods >&2 || true
  kubectl -n "${ds_ns}" logs "ds/${ds}" --all-containers --tail=80 >&2 || true
  kubectl get events -A --sort-by=.lastTimestamp 2>/dev/null | tail -40 >&2 || true
}
cleanup() {
  local rc=$?
  [ "${rc}" -ne 0 ] && dump
  if [ -z "${KEEP:-}" ]; then
    kind delete cluster --name "${NAME}" >/dev/null 2>&1 || true
  else
    echo "KEEP is set: the cluster ${NAME} stays up (KUBECONFIG=${KUBECONFIG})"
  fi
  exit "${rc}"
}

# A kind node shares the host's kernel and cannot load modules into it. The OS
# loads these at boot (image/config/modules-load.d/10-vates.conf); here the host
# must have them, or flanneld exits on "Failed to check br_netfilter".
for m in nf_conntrack br_netfilter overlay; do
  [ -d "/sys/module/${m}" ] || modprobe "${m}" 2>/dev/null ||
    die "kernel module ${m} is not loaded on this host (modprobe ${m})"
done

say "rendering the ${CNI} manifest the node applies (${DOC})"
go run ./test/kind/render "${DOC}" "${CNI}" >"${WORK}/cni.yaml"

# The pod network is the document's cni.cidr: kubeadm hands each node a slice
# of it as its podCIDR, which is what Cilium's ipam: kubernetes reads.
pod_cidr="$(sed -n '/^cni:/,/^[^ ]/s/^  cidr: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "${DOC}")"
[ -n "${pod_cidr}" ] || die "no cni.cidr in ${DOC}"

cat >"${WORK}/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: "${pod_cidr}"
nodes:
- role: control-plane
- role: worker
EOF

say "creating the kind cluster ${NAME} (no CNI, pod network ${pod_cidr})"
trap cleanup EXIT
kind create cluster --name "${NAME}" --config "${WORK}/kind.yaml" --kubeconfig "${KUBECONFIG}" --wait 0s

# The OS ships the generic CNI plugins under /opt/cni/bin (build/recipes/
# runtime/cni.sh); kindest/node only ships its own few, without the bridge
# plugin flannel delegates to. Put the same pinned release on every node.
IFS='|' read -r _ _ cni_file cni_sha cni_url < <(grep '^cni|' build/pins.conf)
say "installing the CNI plugins ${cni_file} on the nodes"
curl -fsSLo "${WORK}/${cni_file}" "${cni_url}"
echo "${cni_sha}  ${WORK}/${cni_file}" | sha256sum -c --quiet -
for node in $(kind get nodes --name "${NAME}"); do
  "${KIND_EXPERIMENTAL_PROVIDER:-docker}" exec -i "${node}" tar -C /opt/cni/bin -xzf - <"${WORK}/${cni_file}"
done

say "applying the ${CNI} manifest"
kubectl apply -f "${WORK}/cni.yaml" >/dev/null
kubectl -n "${ds_ns}" rollout status "ds/${ds}" --timeout=300s
if [ "${CNI}" = "cilium" ]; then
  kubectl -n kube-system rollout status deploy/cilium-operator --timeout=300s
fi

# Ready nodes are not the proof: the kubelet is satisfied by a conflist on
# disk. A pod that needs a sandbox, CoreDNS, Running and Ready is.
say "waiting for the nodes and CoreDNS"
kubectl wait --for=condition=Ready nodes --all --timeout=300s
kubectl -n kube-system wait --for=condition=Ready pod -l k8s-app=kube-dns --timeout=300s

say "a Service on the worker, a client on the control plane"
kubectl create deployment web --image="${AGNHOST}" -- /agnhost netexec --http-port=8080 >/dev/null
kubectl expose deployment web --port=80 --target-port=8080 >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: client
spec:
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  tolerations:
  - operator: Exists
  containers:
  - name: client
    image: ${AGNHOST}
    command: ["sleep", "3600"]
EOF
kubectl rollout status deployment/web --timeout=180s
kubectl wait --for=condition=Ready pod/client --timeout=180s

web_ip="$(kubectl get pod -l app=web -o jsonpath='{.items[0].status.podIP}')"
web_node="$(kubectl get pod -l app=web -o jsonpath='{.items[0].spec.nodeName}')"
client_node="$(kubectl get pod client -o jsonpath='{.spec.nodeName}')"
[ "${web_node}" != "${client_node}" ] || die "web and client landed on the same node (${web_node}); the test would not cross nodes"

# The datapath and DNS are programmed asynchronously once the pods come up, so a
# single connect can race them and fail while everything is healthy a moment
# later -- observed on CI: the first bring-up of a fresh cilium cluster failed
# the ClusterIP check, the immediate rerun passed. Retry until the budget runs
# out and only then fail, with the last error; the same shape as do_wait in
# test/cluster.sh, which refuses to call a cluster up on nodes alone.
connect_until() { # <budget-seconds> <client-pod> <cmd...>
  local budget="$1" client="$2"; shift 2
  local deadline=$(( $(date +%s) + budget )) out
  while :; do
    if out="$(kubectl exec "${client}" -- "$@" 2>&1)"; then
      printf '%s\n' "${out}"
      return 0
    fi
    if [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "  the connect never succeeded within ${budget}s; last error:" >&2
      sed 's/^/    /' <<<"${out}" >&2
      return 1
    fi
    sleep 3
  done
}

say "DNS and the ClusterIP Service, ${client_node} -> ${web_node}"
connect_until 120 client /agnhost connect --timeout=5s web.default.svc.cluster.local:80 \
  || die "the ClusterIP Service / DNS never answered across nodes"

say "pod to pod, ${client_node} -> ${web_ip} on ${web_node}"
connect_until 120 client /agnhost connect --timeout=5s "${web_ip}:8080" \
  || die "pod-to-pod across nodes failed"

if [ "${CNI}" = "cilium" ]; then
  # One operator, maxSurge 0: a rollout must replace it, not wait on a surge
  # pod the anti-affinity refuses.
  say "rolling the operator"
  kubectl -n kube-system rollout restart deploy/cilium-operator >/dev/null
  kubectl -n kube-system rollout status deploy/cilium-operator --timeout=300s
  kubectl -n kube-system exec "ds/cilium" -c cilium-agent -- cilium-dbg status --brief
fi

say "the ${CNI} pod network works"
