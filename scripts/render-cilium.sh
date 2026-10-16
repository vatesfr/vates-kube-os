#!/bin/bash
# Regenerate the embedded Cilium manifest, internal/firstboot/templates/cilium.yaml.tmpl,
# from the official chart.
#
# The manifest is the chart's output with the settings a node needs; every one
# of them is a chart value below, so a Cilium bump is this script with a new
# CILIUM_VERSION, not a hand edit of 1500 lines. The header of the template (the
# comment block before the first `---`) is hand-written and kept as is; only
# the body is replaced.
#
# Two things are not chart values and are done after the render:
#   - the two images become {{.AgentImage}} and {{.OperatorImage}}, so the node
#     can pass them through the registry mirrors (internal/firstboot/cilium.go);
#   - the netns hostPath moves from /var/run/netns to /run/netns, beside the
#     agent's runPath.
# The helm labels, the `# Source:` lines and trailing blanks are dropped, so the
# file reads as an artifact of the OS rather than as a helm release.
#
# The image references the chart pins are printed at the end: they go, tag AND
# digest, into internal/firstboot/cilium.go.
#
#   scripts/render-cilium.sh                        # the version below
#   CILIUM_VERSION=1.20.2 scripts/render-cilium.sh  # a bump
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMPL="${ROOT}/internal/firstboot/templates/cilium.yaml.tmpl"
# Keep in step with the images in internal/firstboot/cilium.go and the
# retrofit in test/cilium.sh.
CILIUM_VERSION="${CILIUM_VERSION:-1.20.1}"

command -v helm >/dev/null 2>&1 || {
  echo "FATAL: helm is not on PATH (https://helm.sh/docs/intro/install/)" >&2
  exit 1
}

helm_args=(
  --namespace kube-system
  --repo https://helm.cilium.io
  --version "${CILIUM_VERSION}"
  # The pod network comes from the node's podCIDR annotation, which kubeadm
  # writes from cni.cidr; kube-proxy stays and implements Services.
  --set ipam.mode=kubernetes
  --set kubeProxyReplacement=false
  --set hubble.enabled=false
  --set envoy.enabled=false
  --set gatewayAPI.enabled=false
  --set bpf.enableTCX=false
  # Reuse the cgroup v2 mount PID 1 already provides.
  --set cgroup.autoMount.enabled=false
  --set cgroup.hostRoot=/sys/fs/cgroup
  # The chart's lists minus SYS_MODULE and SYSLOG: nothing here loads a kernel
  # module or reads the kernel log.
  --set securityContext.capabilities.ciliumAgent="{CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}"
  --set securityContext.capabilities.cleanCiliumState="{NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}"
  # The agent's state on the tmpfs /run (see the template's header).
  --set daemon.runPath=/run/cilium
  # One operator, and a rollout that does not need a surge pod the chart's
  # anti-affinity would refuse on a single node.
  --set operator.replicas=1
  --set operator.updateStrategy.rollingUpdate.maxSurge=0
)

rendered="$(helm template cilium cilium "${helm_args[@]}")"

images="$(grep -oE 'image: "quay\.io/cilium/[^"]+"' <<<"${rendered}" | sort -u)"

body="$(sed -E \
    -e 's#image: "quay\.io/cilium/cilium:[^"]*"#image: "{{.AgentImage}}"#' \
    -e 's#image: "quay\.io/cilium/operator-generic:[^"]*"#image: "{{.OperatorImage}}"#' \
    -e 's#path: /var/run/netns$#path: /run/netns#' \
    -e '/^# Source: /d' \
    -e '/helm\.sh\/chart: /d' \
    -e 's/[[:space:]]+$//' <<<"${rendered}" \
  | awk '
      # An `annotations:` left empty once the helm label is gone: drop it.
      { lines[NR] = $0 }
      END {
        for (i = 1; i <= NR; i++) {
          if (lines[i] ~ /^ *annotations:$/) {
            match(lines[i], /^ */); ind = RLENGTH
            match(lines[i+1], /^ */)
            if (i == NR || RLENGTH <= ind) continue
          }
          print lines[i]
        }
      }')"

if grep -qE 'quay\.io/cilium/' <<<"${body}"; then
  echo "FATAL: an image the template does not name was left in the render:" >&2
  grep -nE 'quay\.io/cilium/' <<<"${body}" >&2
  exit 1
fi

{
  sed '/^---$/,$d' "${TMPL}"
  printf '%s\n' "${body}"
} >"${TMPL}.new"
mv "${TMPL}.new" "${TMPL}"

echo "rendered Cilium ${CILIUM_VERSION} into ${TMPL#"${ROOT}/"}"
echo "the chart pins (internal/firstboot/cilium.go):"
sed 's/^/  /' <<<"${images}"
