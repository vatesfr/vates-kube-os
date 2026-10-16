#!/bin/bash
# Install Cilium on a running Vates Kube OS test cluster whose nodes did not
# install a CNI (cni.plugin: none) or that ran flannel.
#
# This is a MANUAL retrofit tool: it removes flannel (and, when the
# replacement is on, kube-proxy) and applies the Cilium chart from the host,
# then waits for the agent to come up. A cluster that wants Cilium from the
# start selects it in the node document (cni.plugin: cilium) and the image
# installs it at bootstrap with no host involvement -- this script is for the
# other direction, adding Cilium to a cluster that came up without it.
#
#   make cilium                                # on vates-cp-1, replacing kube-proxy
#   test/cilium.sh vates-cp-2                  # a specific node to talk to
#   KUBE_PROXY_REPLACEMENT=false test/cilium.sh   # keep kube-proxy
#
# Cilium runs through its container image, so the node needs no rebuild for it:
# the kernel prerequisites are already in the image (docs/ARCHITECTURE.md).
set -euo pipefail

CONN="qemu:///system"
WORK="${WORK:-/var/tmp/vates-os/cluster}"
CLUSTER="${CLUSTER:-vates-test}"
KUBECONFIG_LOCAL="${WORK}/kubeconfig"
# The same Cilium the image embeds (internal/firstboot/cilium.go): a retrofit
# that installed a different release would leave two versions of the CNI in the
# cluster, and the operator and agent must agree.
CILIUM_VERSION="${CILIUM_VERSION:-1.20.1}"
# Cilium's kube-proxy replacement needs the API address before its own Service
# handling exists; the node's own address is always reachable and is an API
# endpoint, so it is used rather than the VIP (which kube-vip owns and which
# would add a second thing that has to be up for the install to work).
REPLACEMENT="${KUBE_PROXY_REPLACEMENT:-true}"

NODE="${1:-vates-cp-1}"
say() { printf '\n=== %s ===\n' "$*"; }
die() { echo "FATAL: $*" >&2; exit 1; }

vm_ip() {
	virsh -c "${CONN}" domifaddr "$1" --source lease 2>/dev/null \
		| awk '/ipv4/ {print $4}' | cut -d/ -f1 | head -1 || true
}

[ -f "${KUBECONFIG_LOCAL}" ] || die "no kubeconfig at ${KUBECONFIG_LOCAL}; run make cluster first"
IP="$(vm_ip "${NODE}")"
[ -n "${IP}" ] || die "no address for ${NODE}"
K8S_SERVICE_HOST="${K8S_SERVICE_HOST:-${IP}}"
K8S_SERVICE_PORT="${K8S_SERVICE_PORT:-6443}"

# The host cannot route the VIP the kubeconfig names, so the node's own address
# is used -- the same detour test/cluster.sh's host_kubectl takes.
k() { kubectl --kubeconfig "${KUBECONFIG_LOCAL}" --server="https://${IP}:${K8S_SERVICE_PORT}" "$@"; }

command -v helm >/dev/null || die "helm is not installed"
command -v kubectl >/dev/null || die "kubectl is not installed"

# The management API answering (what test/cluster.sh waits for) is not the
# Kubernetes API answering: the control plane's static pods come up a little
# later. Nothing here can be applied until it does.
say "waiting for the Kubernetes API at https://${IP}:${K8S_SERVICE_PORT}"
for _ in $(seq 1 60); do
	k get --raw /readyz >/dev/null 2>&1 && break
	sleep 3
done
k get --raw /readyz >/dev/null 2>&1 || die "the Kubernetes API never answered at https://${IP}:${K8S_SERVICE_PORT}"

say "removing flannel (Cilium takes over the pod network)"
k -n kube-flannel delete daemonset kube-flannel-ds --ignore-not-found
k -n kube-flannel delete configmap kube-flannel-cfg --ignore-not-found || true

if [ "${REPLACEMENT}" = "true" ]; then
	say "removing kube-proxy (Cilium replaces it in eBPF)"
	k -n kube-system delete daemonset kube-proxy --ignore-not-found
	k -n kube-system delete configmap kube-proxy --ignore-not-found || true
	k -n kube-system delete serviceaccount kube-proxy --ignore-not-found || true
	# kube-proxy's iptables rules are left behind by the delete; Cilium logs and
	# removes the ones that conflict as it takes over.
fi

say "rendering Cilium ${CILIUM_VERSION} (replacement: ${REPLACEMENT})"
helm repo add cilium https://helm.cilium.io >/dev/null 2>&1 || true
helm repo update cilium >/dev/null 2>&1 || true

helm_args=(
	--namespace kube-system
	--version "${CILIUM_VERSION}"
	--set ipam.mode=kubernetes
	# Reuse the cgroup v2 mount PID 1 already provides rather than let Cilium
	# make its own, and drop SYS_MODULE --
	# nothing here can load a kernel module, and asking for the capability only
	# makes the pod privileged for nothing.
	--set cgroup.autoMount.enabled=false
	--set cgroup.hostRoot=/sys/fs/cgroup
	--set securityContext.capabilities.ciliumAgent="{CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}"
	--set securityContext.capabilities.cleanCiliumState="{NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}"
)
if [ "${REPLACEMENT}" = "true" ]; then
	helm_args+=(
		--set kubeProxyReplacement=true
		--set k8sServiceHost="${K8S_SERVICE_HOST}"
		--set k8sServicePort="${K8S_SERVICE_PORT}"
	)
else
	helm_args+=(--set kubeProxyReplacement=false)
fi

helm template cilium cilium/cilium "${helm_args[@]}" | k apply -f -

say "waiting for the Cilium agent"
k -n kube-system rollout status daemonset/cilium --timeout=300s || true
k -n kube-system get pods -l k8s-app=cilium -o wide 2>/dev/null || true

say "nodes"
k get nodes -o wide
