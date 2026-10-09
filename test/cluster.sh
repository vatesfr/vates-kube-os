#!/bin/bash
# Bring up a Vates Kube OS cluster under libvirt. The size is CP control
# planes and WORKERS workers, both variables; the default is three and three.
#
#   test/cluster.sh [up|down|status]
#
# The topology is the one the project is aimed at:
#
#   vates-cp-1        bootstraps the cluster
#   vates-cp-2, -3    join it as control planes
#   vates-worker-1..3 join as workers
#
#   control plane VIP 192.168.122.200, owned by kube-vip
#
# Everything is derived from one Linux image built by `make image`; the only
# thing that differs between machines is the config drive.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

CONN="qemu:///system"
# Beside the VM disks, under /var/tmp/vates-os, and not under /tmp: libvirt runs
# qemu as its own user, which cannot walk into the repository's home directory nor
# into a private scratch directory. A config drive left somewhere qemu cannot
# read defines perfectly and then fails at start with "Could not open ...:
# Permission denied".
WORK="${WORK:-/var/tmp/vates-os/cluster}"
VIP="${VIP:-192.168.122.200}"
ENDPOINT="${VIP}:6443"
# The management API's port. The node reads the same value from vates-node.yaml
# (api.port) and vateskctl from the operator material (gen --api-port).
API_PORT="${API_PORT:-50000}"
K8S_VERSION="${K8S_VERSION:-v1.31.0}"

# How the node configuration travels on each config drive. This is the one knob
# that lets `make cluster` prove BOTH readers, because the node tries user-data
# first and falls back to the file -- writing both would test only one path:
#
#   user-data   the CAPI path: the vates-node.yaml document in the user-data
#               slot, which is what a bootstrap provider hands to the hypervisor
#   file        the direct-drive path: a vates-node.yaml file
#
#   make cluster CONFIG_FORM=file
CONFIG_FORM="${CONFIG_FORM:-user-data}"

# The image every machine boots, and the EFI variables they start from. The image
# is the disk `make image` produces; the EFI variables default to the
# stock ones, because the disk carries its own ESP.
#
#   IMAGE=.../vates.qcow2 make cluster
IMAGE="${IMAGE:-${ROOT}/build/out/vates.qcow2}"
EFIVARS="${EFIVARS:-}"
export IMAGE EFIVARS

# How many machines of each kind. Three and three by default, which is the
# smallest cluster that shows the things worth showing: a real etcd quorum, and
# workers that are not on the control planes.
#
#   CP=1 WORKERS=0 make cluster     the single node a provider creates first
#   CP=3 WORKERS=5 make cluster     more workers than control planes
#
# An even number of control planes is allowed and is a bad idea -- etcd elects a
# quorum from a majority, so two are worse than one and four are no better than
# three. The default is odd; the script does not forbid the others, because a
# test may want to see what happens.
CP="${CP:-3}"
WORKERS="${WORKERS:-3}"

# What the console shows, per machine. tui by default -- it works everywhere,
# including on a serial console where nothing else can run. gui draws the same
# thing as a picture, under cage, and needs a screen:
#
#   DASHBOARD=gui make cluster
#
# Refused here rather than on the machine: the schema in vatescfg would
# refuse it too, and a bad value discovered at boot is discovered late.
DASHBOARD="${DASHBOARD:-gui}"
case "${DASHBOARD}" in
  tui|gui) ;;
  *) echo "FATAL: DASHBOARD=${DASHBOARD} is not one of tui, gui" >&2; exit 1 ;;
esac

# The cluster's CNI, chosen in the node document: flannel is the default, and
# cilium is installed by the node itself the same way (its manifest, pinned, is
# embedded in the image and applied at bootstrap). Neither replaces kube-proxy.
#
#   CNI=cilium make cluster
#
# Bringing up Cilium on a cluster whose nodes do not install it is a manual
# retrofit, covered by test/cilium.sh (make cilium).
CNI="${CNI:-flannel}"
case "${CNI}" in
  flannel|cilium) ;;
  *) echo "FATAL: CNI=${CNI} is not one of flannel, cilium" >&2; exit 1 ;;
esac

# Memory per machine, in MiB. Six machines on a laptop want less than three.
NODE_MEM="${NODE_MEM:-3072}"

# Seconds between starting one joining machine and the next. They still overlap
# -- the first are in their configure phase while the last one starts -- but a
# short gap stops the whole boot storm, and the qemu setup of five machines,
# from landing on the host at the same instant, which made it stutter. Set
# START_STAGGER=0 to start them all at once.
START_STAGGER="${START_STAGGER:-3}"

CP_NODES=()
for i in $(seq 1 "${CP}"); do CP_NODES+=("vates-cp-${i}"); done
WORKER_NODES=()
for i in $(seq 1 "${WORKERS}"); do WORKER_NODES+=("vates-worker-${i}"); done
[ "${#CP_NODES[@]}" -gt 0 ] || { echo "FATAL: CP=0 -- a cluster needs a control plane" >&2; exit 1; }
# The node has no SSH and no podman: the test drives it through the management
# API. Operator material -- the CA that signs the node's server certificate, and
# the CLI's own certificate -- lives under the vates data directory, in the
# scratch tree, never in the repository.
export XDG_DATA_HOME="${WORK}/xdg"
CLUSTER="${CLUSTER:-vates-test}"
VATESKCTL_BIN="${ROOT}/build/bin/vateskctl"
KUBECONFIG_LOCAL="${WORK}/kubeconfig"

operator_dir() { echo "${XDG_DATA_HOME}/vates/kube-clusters/${CLUSTER}"; }

say() { printf '\n=== %s ===\n' "$*"; }
# Timestamps, so the time a cluster takes is visible rather than felt. The OS
# itself is quick -- from power-on to a registered node is about half a minute --
# and knowing which step costs what is the difference between that fact and an
# impression.
START_TS=$(date +%s)
ts() { printf '  [+%3ds] %s\n' "$(( $(date +%s) - START_TS ))" "$*"; }
die() { echo "FATAL: $*" >&2; exit 1; }

# dump_node_logs prints a node's own logs (PID 1 and its services) over its
# management API, best effort. The node carries no shell, so this is how a
# bring-up failure says WHY it failed instead of only that the API never
# answered. It is called from the failure paths below.
dump_node_logs() { # <address> (host, without port)
  local addr="$1"
  [ -n "${addr}" ] || return 0
  echo "--- ${addr}: node logs (best effort) ---" >&2
  "${VATESKCTL_BIN}" logs --cluster "${CLUSTER}" --node "${addr}:${API_PORT}" 2>&1 \
    | sed 's/^/  /' >&2 || true
}

# --- helpers ----------------------------------------------------------------

vm_ip() {
  # virsh exits 1 when a domain has no lease yet, which is the ordinary state of
  # a machine for its first few seconds -- and every caller polls for exactly
  # that. Under `set -e` the "no address yet" would be read as a failure and
  # would stop the run before the machine had a chance to get one, silently,
  # because the caller has printed nothing at that point.
  virsh -c "${CONN}" domifaddr "$1" --source lease 2>/dev/null \
    | awk '/ipv4/ {print $4}' | cut -d/ -f1 | head -1 || true
}

wait_api() { # <name> -> its address, once the management API answers
  local name="$1" ip="" i
  for i in $(seq 1 60); do
    ip=$(vm_ip "${name}")
    [ -n "${ip}" ] && timeout 3 bash -c "cat < /dev/null > /dev/tcp/${ip}/${API_PORT}" 2>/dev/null && {
      echo "${ip}"; return 0; }
    sleep 3
  done
  return 1
}

# fetch_kubeconfig asks the node's API for the cluster kubeconfig, authenticated
# by the operator CA, and writes it where kubectl will read it. It echoes the
# node's address. This is the whole point: nothing is read off the node's disk,
# and there is no shell on the node to reach.
fetch_kubeconfig() { # <name>
  local name="$1" ip
  ip=$(wait_api "${name}") || return 1
  "${VATESKCTL_BIN}" kubeconfig --node "${ip}:${API_PORT}" --cluster "${CLUSTER}" \
    -o "${KUBECONFIG_LOCAL}" >/dev/null || return 1
  echo "${ip}"
}

# host_kubectl runs kubectl ON THE HOST, with the kubeconfig the API handed over.
# That kubeconfig names the control-plane VIP, which this libvirt network does
# not route from the host, so the node's own address is used instead.
host_kubectl() { # <name> <kubectl args...>
  local name="$1"; shift
  local ip; ip=$(vm_ip "${name}")
  kubectl --kubeconfig "${KUBECONFIG_LOCAL}" --server="https://${ip}:6443" "$@"
}

# --- joining machines -------------------------------------------------------
#
# A node that joins draws everything from the cluster, not from a shell on any
# machine. The credentials live only on the bootstrap control plane -- they are
# signed or encrypted with the cluster CA -- so they are asked for over the
# management API, which is exactly what that API exists for outside CAPI.

# join_material fetches fresh join credentials and sources them into TOKEN,
# CERT_KEY and CA_HASH. They expire (24 hours and two), so they are obtained when
# the joining machines are about to be created, never stored.
join_material() {
  "${VATESKCTL_BIN}" join-material --cluster "${CLUSTER}" > "${WORK}/join.env" || return 1
  # A silent empty answer must not become "TOKEN: unbound variable" three steps
  # later, which names the symptom and not the cause.
  [ -s "${WORK}/join.env" ] || return 1
  # shellcheck disable=SC1090,SC1091
  . "${WORK}/join.env"
  [ -n "${TOKEN:-}" ] && [ -n "${CERT_KEY:-}" ] || return 1
}

# cluster_ca writes the cluster's CA certificate, taken from the kubeconfig the
# node handed over, into a file.
cluster_ca() { # <path>
  kubectl --kubeconfig "${KUBECONFIG_LOCAL}" config view --raw --minify \
    -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$1"
}

# drive_ca writes the cluster's CA certificate into a machine's config drive.
#
# It is all a joining machine needs to verify the API server, and all its kubelet
# needs to bootstrap its own certificate from the token in vates-node.yaml. The CA
# certificate is public; the private key never leaves the control planes.
drive_ca() { # <dir>
  install -d -m 2775 -g qemu "$1/pki"
  cluster_ca "$1/pki/ca.crt"
}

# --- config drives ----------------------------------------------------------

# write_drive <name> <role> [extra cluster yaml on stdin]
#
# One document per machine, which is the whole contract: everything that differs
# between the machines is in here.
write_drive() {
  local name="$1" role="$2" extra="${3:-}"
  local dir="${WORK}/cfgdrive/${name}"
  rm -rf "${dir}"; install -d -m 2775 -g qemu "${dir}"

  printf 'instance-id: %s\nlocal-hostname: %s\n' "${name}" "${name}" > "${dir}/meta-data"

  # The VIP is a control plane concern, and the schema rejects it on a worker:
  # accepting it would let a worker claim the cluster's endpoint for itself.
  local vip_block=""
  if [ "${role}" = "master" ]; then
    vip_block="  vip:
    address: \"${VIP}\"
"
  fi

  # The CNI the node installs itself, stated in its document. Both are
  # applied by the node at bootstrap -- see internal/firstboot -- and both
  # run alongside kube-proxy, so nothing else in the document changes.
  local cni_plugin="${CNI}"

  local doc
  doc="$(cat <<EOF
role: ${role}
kubernetes:
  version: ${K8S_VERSION}
cluster:
  controlPlaneEndpoint: "${ENDPOINT}"
${extra}${vip_block}network:
  iface: eth0
  mode: dhcp
cni:
  plugin: ${cni_plugin}
  cidr: "10.244.0.0/16"
dashboard:
  mode: ${DASHBOARD}
api:
  port: ${API_PORT}
EOF
)"

  # The configuration travels the way CONFIG_FORM asks, and ONLY one way: the
  # node reads user-data first, so writing both would silently test only the
  # user-data path and never the fallback.
  case "${CONFIG_FORM}" in
    user-data)
      printf '%s\n' "${doc}" > "${dir}/user-data"
      ;;
    file)
      printf '#cloud-config\n' > "${dir}/user-data"
      printf '%s\n' "${doc}" > "${dir}/vates-node.yaml"
      ;;
    *)
      printf 'FATAL: CONFIG_FORM must be user-data or file (got %q)\n' "${CONFIG_FORM}" >&2
      exit 1
      ;;
  esac

  # The operator CA. The node mints its management-API server certificate from it
  # and trusts the certificates it signs; without it the API would fall back to
  # the cluster CA, which an operator outside CAPI does not hold.
  cp "$(operator_dir)/api-ca.crt" "${dir}/api-ca.crt"
  cp "$(operator_dir)/api-ca.key" "${dir}/api-ca.key"

  # Anything else the drive carries -- a worker's PKI and kubeconfig -- is copied
  # in by the caller before this runs.
  "${ROOT}/scripts/mkconfigdrive.sh" "${dir}" "${dir}.iso" >/dev/null
  # xorriso creates the ISO through the umask too.
  chmod 0644 "${dir}.iso"
}

# write_join_drive <name> <role> <extra cluster yaml>
#
# The drive a JOINING machine boots, which is write_drive's output plus the
# cluster CA it needs to verify the API server. The ISO is rebuilt after the CA
# is installed because write_drive has already built one without it.
write_join_drive() {
  local name="$1" role="$2" extra="$3"
  local dir="${WORK}/cfgdrive/${name}"
  write_drive "${name}" "${role}" "${extra}"
  drive_ca "${dir}"
  "${ROOT}/scripts/mkconfigdrive.sh" "${dir}" "${dir}.iso" >/dev/null
  chmod 0644 "${dir}.iso"
}

start_vm() { # <name> <drive iso>
  local name="$1" iso="$2"

  # qemu runs as its own user and has to walk the whole path to the drive. Every
  # component is made group-readable here rather than trusted to whatever created
  # it: the failure it prevents -- "Could not open '<iso>': Permission denied" at
  # domain start -- names the drive, not the directory that blocks it.
  local d
  for d in "$(dirname "${iso}")" "$(dirname "$(dirname "${iso}")")"; do
    chgrp qemu "${d}" 2>/dev/null || true
    chmod 2775 "${d}" 2>/dev/null || true
  done
  chmod 0644 "${iso}" 2>/dev/null || true
  echo "  drive: $(stat -c '%A %U:%G' "${iso}" 2>/dev/null) $(stat -c '%A %U:%G' "$(dirname "${iso}")" 2>/dev/null) $(stat -c '%A %U:%G' "$(dirname "$(dirname "${iso}")")" 2>/dev/null)"

  virsh -c "${CONN}" destroy "${name}" >/dev/null 2>&1 || true
  virsh -c "${CONN}" undefine "${name}" --nvram >/dev/null 2>&1 || true
  rm -f "/var/tmp/vates-os/${name}"*
  "${ROOT}/test/virsh-vm.sh" "${name}" "${iso}" --mem "${NODE_MEM:-3072}" >/dev/null
}

# --- down -------------------------------------------------------------------

do_down() {
  say "removing the cluster"
  # By name, and not by the CP/WORKERS variables: those say what to CREATE, and
  # a `down` that only knew the default would leave behind a cluster brought up
  # with other counts. Whatever is called vates-cp-N or vates-worker-N goes.
  local n
  while read -r n; do
    [ -n "${n}" ] || continue
    virsh -c "${CONN}" destroy "${n}" >/dev/null 2>&1 || true
    virsh -c "${CONN}" undefine "${n}" --nvram >/dev/null 2>&1 || true
    rm -f "/var/tmp/vates-os/${n}"*
    rm -f "${WORK}/cfgdrive/${n}.iso"
  done < <(virsh -c "${CONN}" list --all --name 2>/dev/null | grep -E '^vates-(cp|worker)-[0-9]+$')
  echo "  done"
}

# --- up ---------------------------------------------------------------------

# require_host_groups verifies the two group memberships the whole up flow
# assumes, before anything is deleted, and says the fix rather than the symptom.
# A fresh host has neither, and each omission fails somewhere that names
# something else entirely:
#
#   qemu    the working tree is group-owned by qemu, which runs as its own user
#           and must walk the path to every config drive. A non-root user can
#           only chgrp to a group they belong to, so without it the
#           `install -d -g qemu` below fails with "Operation not permitted".
#
#   libvirt every virsh call below manages qemu:///system. The libvirt package
#           ships the polkit rule 50-libvirt.rules, which waives the password
#           for this group and no other; without it EVERY virsh call raises an
#           authentication dialog, and `make cluster` becomes a stream of
#           password prompts rather than a cluster.
#
# Groups are read at login, so both fixes need a new session.
require_host_groups() {
  [ "$(id -u)" -eq 0 ] && return 0
  local groups=" $(id -nG) " missing=() g
  case "${groups}" in *" qemu "*) ;; *) missing+=(qemu) ;; esac
  case "${groups}" in *" libvirt "*) ;; *) missing+=(libvirt) ;; esac
  [ "${#missing[@]}" -eq 0 ] && return 0

  echo "FATAL: you are missing the group(s) the cluster needs: ${missing[*]}." >&2
  echo "  qemu    lets the qemu user walk to every config drive." >&2
  echo "  libvirt lets virsh manage qemu:///system without a password prompt on every call." >&2
  echo "  Add yourself, then start a new session (groups are read at login):" >&2
  for g in "${missing[@]}"; do
    echo "    sudo usermod -aG ${g} $(whoami)" >&2
  done
  echo "  Or run the cluster as root." >&2
  exit 1
}

# build_vateskctl compiles the operator CLI into ${VATESKCTL_BIN}. The build
# lives in scripts/build-vateskctl.sh, which `make cli` runs too: vateskctl is a
# host artifact of its own, not a detail of the test cluster.
#
# XDG_DATA_HOME points at the scratch tree (below), for vateskctl's operator
# material. Rootless podman reads it too, though, and would bury its whole image
# store there -- subuid-owned files that do_up's `rm -rf "${WORK}"` then cannot
# remove, and a private store that re-pulls the image on every run. Unset it for
# this one call, so podman uses the user's normal storage.
build_vateskctl() {
  env -u XDG_DATA_HOME VATESKCTL_BIN="${VATESKCTL_BIN}" "${ROOT}/scripts/build-vateskctl.sh"
}

do_up() {
  [ -f "${IMAGE}" ] || die "no image at ${IMAGE}: run make image, or set IMAGE="
  require_host_groups
  rm -rf "${WORK}"
  # install -d sets the mode explicitly, where mkdir is filtered through the
  # umask. That matters: qemu runs as its own user and has to be able to walk the
  # whole path to a config drive, and a directory created 0700 -- which is what a
  # restrictive umask produces -- makes the domain fail to start with "Could not
  # open ...: Permission denied", long after the drive was written.
  install -d -m 2775 -g qemu "${WORK}" "${WORK}/cfgdrive"

  # The operator's own CA, and the CLI that uses it, generated before anything
  # boots: this is the credential, and the node never hands one out. The CLI is a
  # host tool -- it runs off the node -- built by build_vateskctl.
  build_vateskctl
  # --endpoint is the address the operator knows the cluster by: the VIP. It is
  # recorded so vateskctl defaults to it, and it is what vates-init must put in
  # the API's certificate -- the node does not have the VIP when that certificate
  # is minted.
  "${VATESKCTL_BIN}" gen --cluster "${CLUSTER}" --endpoint "${ENDPOINT}" --api-port "${API_PORT}" >/dev/null

  # --- the bootstrap control plane ----------------------------------------
  START_TS=$(date +%s)
  say "starting ${CP_NODES[0]} (bootstraps the cluster)"
  write_drive "${CP_NODES[0]}" master
  start_vm "${CP_NODES[0]}" "${WORK}/cfgdrive/${CP_NODES[0]}.iso"

  # Wait for the management API, then take the cluster kubeconfig from it.
  local cp1_ip
  cp1_ip=$(fetch_kubeconfig "${CP_NODES[0]}") || {
    dump_node_logs "$(vm_ip "${CP_NODES[0]}")"
    die "${CP_NODES[0]}'s management API never answered"
  }
  ts "address ${cp1_ip}; kubeconfig fetched over the API"

  # A single-node control plane comes up on its own; wait for the API server
  # before asking it for anything.
  local i ready=0
  for i in $(seq 1 30); do
    sleep 15
    if host_kubectl "${CP_NODES[0]}" get nodes --no-headers 2>/dev/null | grep -q " Ready "; then
      ready=1; break
    fi
  done
  [ "${ready}" = "1" ] || {
    dump_node_logs "${cp1_ip}"
    die "${CP_NODES[0]} never reached Ready"
  }
  ts "${CP_NODES[0]} Ready"

  # --- the joining machines ------------------------------------------------
  #
  # A second control plane or a worker joins through the management API, which is
  # where join credentials live outside CAPI: they are signed or encrypted with
  # the cluster CA, and the bootstrap control plane is the only holder. Nothing
  # here needs a shell on any node.
  local joining=$(( ${#CP_NODES[@]} - 1 + ${#WORKER_NODES[@]} ))
  if [ "${joining}" -gt 0 ]; then
    say "collecting join credentials from ${CP_NODES[0]}"
    join_material || die "cannot obtain join credentials from ${CP_NODES[0]}"
    ts "join material collected (token, certificate key, CA hash)"

    # Every joining machine's drive is written first, and the machines are then
    # started TOGETHER rather than one after another. The first seconds after
    # power-on are the node's own configure work, and overlapping them is the
    # point: a machine no longer waits for the previous one to be defined.
    #
    # The joins do contend -- several control planes can race on etcd's learner
    # promotion -- but kubeadm's join is already retried on the node
    # (firstboot.JoinAttempts), so the race costs a retry and not a failure.
    local n k
    local -a join_names=() join_roles=() join_extras=()

    # --- the joining control planes ---
    # They fetch the shared certificates from the cluster with the certificate
    # key, so no CA private key is shipped. Their kubelet obtains its OWN
    # certificate too: the drive carries the cluster CA and the token, and the
    # kubelet bootstraps from them.
    for n in "${CP_NODES[@]:1}"; do
      join_names+=("${n}"); join_roles+=("master")
      join_extras+=("  token: \"${TOKEN}\"
  certificateKey: \"${CERT_KEY}\"
  caCertHash: \"${CA_HASH}\"
")
    done

    # --- the workers ---
    # A worker does not run kubeadm: its kubelet gets the cluster CA and the
    # token, and asks the API server for its own certificate. Nothing is signed
    # here, and the CA key is never needed.
    for n in "${WORKER_NODES[@]}"; do
      join_names+=("${n}"); join_roles+=("worker")
      join_extras+=("  token: \"${TOKEN}\"
")
    done

    for k in "${!join_names[@]}"; do
      say "preparing ${join_names[k]} (joins as a ${join_roles[k]})"
      write_join_drive "${join_names[k]}" "${join_roles[k]}" "${join_extras[k]}"
    done

    # Start them all at once, a breath apart. Each start is backgrounded and
    # writes its own log, because five virsh invocations interleaved on one
    # terminal name no machine. The logs are replayed in order once every machine
    # has been told to start. The base image is staged by the bootstrap control
    # plane, which is already up, so these parallel starts do not race on it.
    say "starting ${#join_names[@]} joining machines together (${START_STAGGER}s apart)"
    local -a join_pids=() rc=0
    for k in "${!join_names[@]}"; do
      start_vm "${join_names[k]}" "${WORK}/cfgdrive/${join_names[k]}.iso" \
        >"${WORK}/start-${join_names[k]}.log" 2>&1 &
      join_pids+=("$!")
      # No pause after the last one: nothing is left to stagger.
      if [ "${k}" -lt "$(( ${#join_names[@]} - 1 ))" ]; then
        sleep "${START_STAGGER}"
      fi
    done
    for k in "${!join_names[@]}"; do
      wait "${join_pids[k]}" || rc=1
    done
    for k in "${!join_names[@]}"; do
      cat "${WORK}/start-${join_names[k]}.log"
    done
    [ "${rc}" = "0" ] || die "at least one joining machine failed to start"
  fi

  do_wait
  do_status
}

# --- wait -------------------------------------------------------------------

# The number of machines that should end up Ready.
want_nodes() { echo $(( ${#CP_NODES[@]} + ${#WORKER_NODES[@]} )); }

# do_wait polls until every machine is Ready, and returns the moment they are.
#
# A fixed sleep would be either too short, and read a half-built cluster as a
# failure, or too long, and waste the difference on every run. The condition is
# the thing to wait for, and it is cheap to ask.
do_wait() {
  local want="${1:-$(want_nodes)}" n first=""
  for n in "${CP_NODES[@]}" "${WORKER_NODES[@]}"; do
    [ -n "$(vm_ip "${n}")" ] && { first="${n}"; break; }
  done
  [ -n "${first}" ] || die "no machine is reachable"

  START_TS=$(date +%s)
  local deadline=$(( $(date +%s) + 900 )) ready
  while :; do
    # The "|| true" is not decoration. grep -c exits 1 when it counts nothing,
    # and the five machines that have just been started are not Ready yet, so
    # that is the very first answer this loop gets. Under `set -e` an assignment
    # takes the exit status of what it is assigned, so without it the whole
    # script stops on the first poll -- silently, because nothing has been
    # printed yet. Asking how many nodes are Ready and treating "none" as an
    # answer, rather than as a failure, is the whole point of the loop.
    ready=$(host_kubectl "${first}" get nodes --no-headers 2>/dev/null | grep -c " Ready " || true)
    ts "nodes Ready: ${ready}/${want}"
    [ "${ready}" -ge "${want}" ] && break
    if [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "  the missing machines are worth looking at individually:" >&2
      echo "    ./test/cluster.sh status" >&2
      echo "    virsh -c qemu:///system console <node>   # then read the journal" >&2
      die "${ready}/${want} nodes Ready after 15 minutes"
    fi
    sleep 10
  done

  # Every node Ready is not the cluster up: the kubelet reports Ready as soon as
  # a CNI conflist exists, and a CNI whose agent socket the plugins cannot reach
  # (a Cilium that lost its /var/run -> /run link) leaves every pod -- CoreDNS
  # included -- in ContainerCreating while the nodes all say Ready. So the wait
  # ends only when a pod that needs the pod network is actually Running.
  wait_pod_network "${first}"
}

# wait_pod_network waits until at least one CoreDNS pod is Running, which is the
# proof that the pod network programs sandboxes. `Ready` nodes are not the
# proof: the kubelet is satisfied with a conflist on disk, and a CNI whose agent
# socket the plugin cannot dial keeps every pod in ContainerCreating -- CoreDNS
# among them -- with the nodes still Ready.
#
# CoreDNS is the pod to ask: the addons always deploy it on the control planes,
# and it cannot start until its sandbox is programmed, so its being Running is
# the CNI working. Polls cheaply, and on failure names the cause -- the stuck
# pod and the CNI error in its describe -- rather than "nodes Ready".
wait_pod_network() { # <name>
  local name="$1" deadline=$(( $(date +%s) + 300 )) running
  while :; do
    running=$(host_kubectl "${name}" -n kube-system get pods -l k8s-app=kube-dns --no-headers 2>/dev/null \
      | grep -c " Running " || true)
    ts "CoreDNS Running: ${running}"
    [ "${running}" -ge 1 ] && { ts "the pod network programs sandboxes (CoreDNS is Running)"; return 0; }
    if [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "  the nodes are Ready but no pod can start; the CNI is the thing to look at:" >&2
      host_kubectl "${name}" -n kube-system get pods -l k8s-app=kube-dns -o wide 2>&1 | sed 's/^/    /' >&2
      local pod
      pod=$(host_kubectl "${name}" -n kube-system get pods -l k8s-app=kube-dns \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
      if [ -n "${pod}" ]; then
        echo "  and the CNI error from the pod's describe:" >&2
        host_kubectl "${name}" -n kube-system describe pod "${pod}" 2>&1 \
          | sed -n '/^Events:/,$p' | tail -14 | sed 's/^/    /' >&2
      fi
      die "the nodes are Ready but CoreDNS never ran; the pod network is not working"
    fi
    sleep 5
  done
}

# --- status -----------------------------------------------------------------

do_status() {
  say "nodes"
  local n first=""
  for n in "${CP_NODES[@]}" "${WORKER_NODES[@]}"; do
    [ -n "$(vm_ip "${n}")" ] && { first="${n}"; break; }
  done
  [ -n "${first}" ] || die "no machine is reachable"
  host_kubectl "${first}" get nodes -o wide 2>&1 | sed 's/^/  /'

  say "pods"
  host_kubectl "${first}" get pods -A 2>&1 | sed 's/^/  /'

  say "the management API, at the cluster endpoint (${ENDPOINT%:*}:${API_PORT})"
  # No --node: vateskctl defaults to the endpoint recorded at gen time -- the
  # VIP, the address an operator actually knows. This only works because
  # vates-init put the VIP in the API's certificate, which the node did not have
  # when that certificate was minted.
  "${VATESKCTL_BIN}" status --cluster "${CLUSTER}" 2>&1 | sed 's/^/  /' || true
}

case "${1:-up}" in
  up)     do_up ;;
  down)   do_down ;;
  status) do_status ;;
  wait)   do_wait ;;
  *) echo "usage: $(basename "$0") [up|down|status|wait]" >&2; exit 2 ;;
esac
