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

# download URL DEST -- one attempt, with the guard rails a build host needs.
# --connect-timeout: a dead host must fail, not hang the build.
# --speed-limit/--speed-time: a stalled transfer aborts, and fetch tries the
# next source (or the next run starts over).
download() {
	curl -fL --retry 3 --connect-timeout 20 \
		--speed-limit 1024 --speed-time 60 \
		-o "$2" "$1"
}

# fetch NAME VERSION SOURCE SHA URL [MIRRORS] -- stage SOURCE into ${DL}.
#
# URL is the upstream one; MIRRORS (space-separated, the optional 6th field of
# pins.conf) are tried, in order, only when the upstream fails or serves
# something the sha256 rejects. Some upstreams are single, flaky hosts --
# dri.freedesktop.org, measured -- and a build that depends on one of them fails
# for reasons that have nothing to do with this project. Every source must serve
# the SAME artifact: the sha256 is what makes a mirror trustworthy, not the
# mirror's name.
fetch() {
	local name="$1" version="$2" source="$3" sha="$4" url="$5" mirror="${6:-}" dest u
	dest="${DL}/${source}"
	if [ -f "${dest}" ] && echo "${sha}  ${dest}" | sha256sum -c --quiet -; then
		return 0
	fi
	rm -f "${dest}"
	for u in ${url} ${mirror}; do
		echo "  fetching ${source} from ${u}"
		if download "${u}" "${dest}.part" \
			&& mv "${dest}.part" "${dest}" \
			&& echo "${sha}  ${dest}" | sha256sum -c --quiet -; then
			return 0
		fi
		echo "  ${u} did not give a usable ${source}; trying the next source" >&2
		rm -f "${dest}" "${dest}.part"
	done
	echo "  no source served ${source} (url: ${url}, mirrors: ${mirror:-none})" >&2
	exit 1
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
while IFS='|' read -r name version source sha url mirror; do
	case "${name}" in \#*|"") continue ;; esac
	fetch "${name}" "${version}" "${source}" "${sha}" "${url}" "${mirror}"
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
echo "=== podman build --target ${TARGET} ==="
podman build \
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
	podman run "${run_opts[@]}" -v "${OUT}:/out" "${IMAGE}:${TARGET}" \
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
