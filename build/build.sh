#!/bin/bash
# Vates Kube OS -- from-scratch build driver (podman first, docker compatible).
#
#   ./build.sh [target]
#
# `target` is a Containerfile stage: `socle` (default), `tools`, `console`,
# `kernel`, later `rootfs`/`disk`. Before building, every pinned source is
# fetched into build/dl/ from its upstream URL and verified against the
# sha256 in pins.conf, so the build itself needs no network and no other project.
#
# Every component comes from upstream, pinned by sha256; no other project's
# metadata is read.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${1:-socle}"

# A stable log, and a way to follow it from another terminal; the build tees
# everything there. `./build.sh follow` is a stable command even when the build
# runs elsewhere (an agent's shell, another terminal).
LOG="${VATES_SCRATCH_LOG:-${HERE}/build.log}"
if [ "${TARGET}" = follow ]; then
	mkdir -p "$(dirname "${LOG}")"
	touch "${LOG}"
	exec tail -n +1 -F "${LOG}"
fi
mkdir -p "$(dirname "${LOG}")"
exec > >(tee -a "${LOG}") 2>&1
echo "=== $(date -Is) build target: ${TARGET} (log: ${LOG}) ==="

# Fedora by default: RHEL-family, never Debian. The base is only the build host.
BASE="${VATES_SCRATCH_BASE:-registry.fedoraproject.org/fedora:44}"
DL="${HERE}/dl"
IMAGE="${VATES_SCRATCH_IMAGE:-localhost/vates-scratch}"

# Prefer Podman, but use Docker when it is the installed container engine.
# Check before fetching sources so a missing engine fails immediately.
if command -v podman >/dev/null 2>&1; then
	ENGINE=podman
elif command -v docker >/dev/null 2>&1; then
	ENGINE=docker
else
	echo "no container engine found; install podman or docker" >&2
	exit 1
fi

fetch() {
	local name="$1" version="$2" source="$3" sha="$4" url="$5" dest
	dest="${DL}/${source}"
	if [ ! -f "${dest}" ]; then
		echo "  fetching ${source}"
		# --connect-timeout: a dead host must fail, not hang the build.
		# --speed-limit/--speed-time: a stalled transfer aborts and the
		# retries above (or the next run) start over.
		curl -fL --retry 3 --connect-timeout 20 \
			--speed-limit 1024 --speed-time 60 \
			-o "${dest}.part" "${url}"
		mv "${dest}.part" "${dest}"
	fi
	if ! echo "${sha}  ${dest}" | sha256sum -c --quiet -; then
		echo "  sha256 mismatch for ${source}" >&2
		rm -f "${dest}"
		exit 1
	fi
}

# One build at a time. Two concurrent runs share ${DL} and ${LOG}: they would
# download into the same .part path and corrupt each other. Fail fast with the
# command that shows what the running build is doing.
exec 9> "${HERE}/.build.lock"
if ! flock -n 9; then
	echo "another build is already running (lock: ${HERE}/.build.lock)" >&2
	echo "follow it with: ./build.sh follow" >&2
	exit 1
fi

echo "=== staging and verifying sources into ${DL} ==="
mkdir -p "${DL}"
while IFS='|' read -r name version source sha url; do
	case "${name}" in \#*|"") continue ;; esac
	fetch "${name}" "${version}" "${source}" "${sha}" "${url}"
done < "${HERE}/pins.conf"

# The image's own content -- the vates binaries, the overlay, the node's /etc and
# the console assets -- is not built by any recipe. Stage it for the assembly
# stage, but only when the target actually assembles a root filesystem.
case "${TARGET}" in
rootfs | disk)
	echo
	echo "=== staging the image's own content (context/) ==="
	ROOT="$(cd "${HERE}/.." && pwd)"
	CTX="${HERE}/context"
	rm -rf "${CTX}"
	mkdir -p "${CTX}/src"
	# The Go sources the vates binaries are built from (recipes/vates, run in
	# the Containerfile's vates stage). The upstream components -- libxenstore
	# and the Rust Xen guest agent -- are pinned in pins.conf and fetched into
	# dl/ by this script instead.
	cp -a "${ROOT}/cmd" "${ROOT}/internal" "${ROOT}/vatescfg" "${ROOT}/proto" "${ROOT}/go.mod" "${ROOT}/go.sum" "${CTX}/src/"
	cp -a "${HERE}/overlay" "${CTX}/overlay"
	cp -a "${ROOT}/image/config" "${CTX}/config"
	cp -a "${ROOT}/image/assets" "${CTX}/assets"
	;;
esac

echo
echo "=== ${ENGINE} build --target ${TARGET} ==="
"${ENGINE}" build \
	--file "${HERE}/Containerfile" \
	--tag "${IMAGE}:${TARGET}" \
	--target "${TARGET}" \
	--build-arg "BASE=${BASE}" \
	"${HERE}"

# The disk is assembled HERE, from the :disk assembler image, and written
# straight into build/out. It is not a RUN in the Containerfile: a RUN would
# commit rootfs/ESP/vates.img -- several GiB -- into the image, which would make
# the image a result rather than the tool it is, and force a throwaway container
# and a copy to get the files back out. The image carries the tools and the
# inputs; this turns them into the disk.
#
# SELinux: the checkout is user_home_t, which a container cannot read without
# relabelling the whole tree. Disable labelling for this one container.
if [ "${TARGET}" = disk ]; then
	OUT="${HERE}/out"
	mkdir -p "${OUT}"
	run_opts=(--rm)
	if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" != "Disabled" ]; then
		run_opts+=(--security-opt label=disable)
	fi
	"${ENGINE}" run "${run_opts[@]}" -v "${OUT}:/out" "${IMAGE}:${TARGET}" \
		bash /build/lib/assemble.sh
	echo
	echo "=== converting ==="
	if command -v qemu-img >/dev/null; then
		qemu-img convert -f raw -O qcow2 "${OUT}/vates.img" "${OUT}/vates.qcow2"
		qemu-img convert -f raw -O vpc -o subformat=dynamic "${OUT}/vates.img" "${OUT}/vates.vhd"
		ls -lh "${OUT}/vates.qcow2" "${OUT}/vates.vhd"
	else
		echo "qemu-img not found; ${OUT}/vates.img is the raw disk."
	fi
fi
