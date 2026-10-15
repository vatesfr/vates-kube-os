# Vates Kube OS — from-scratch builder

The node's userspace is built from pinned **upstream** sources with a
`Containerfile`, driven by podman. Podman is the first-class target; the file is
plain Dockerfile syntax, so `docker build` works too. Nothing is built on the
host.

> For the developer guide -- how the distribution is built end to end, and how to
> change it -- see [`../docs/BUILD.md`](../docs/BUILD.md). This file is the
> builder's own reference.

Every component comes from its own upstream project, pinned by sha256. Where a
component would not build against our environment, we change our own recipe
(e.g. busybox's `tc` applet is disabled because a recent kernel UAPI dropped the
CBQ structures it uses) rather than borrowing a downstream patch.

## The idea

"From scratch" is four independent layers:

| Layer | Who does it here |
|---|---|
| 1. C library | **glibc built from GNU source** (pass -1) — our version, our build |
| 2. Kernel | `linux` built from source + `board/kernel.fragment` |
| 3. Userspace | one Containerfile stage per pass, one recipe per component |
| 4. Assembly + image | overlay + genimage (pass 6) |

The compiler is the seed gcc of the builder image (Fedora), used only to build
things — it never ships. What ships is the glibc we compiled.

> Why not a prebuilt cross-toolchain (Bootlin, crosstool-NG)? Because those are
> built for embedded, by a meta build system we are deliberately leaving behind.
> Consuming one inherits exactly the provenance we want to avoid. Owning the libc
> — the part that actually ships — is the answer; the compiler is a build tool.

### The dependency graph, as passes

The userspace is ~33 recipes, not thousands. Grouped by what can build in
parallel:

- **pass -1 — libc**: kernel UAPI headers (from the pinned `linux` source) +
  **glibc 2.44** from `ftp.gnu.org`, into `/sysroot`. The seed gcc's own runtime
  (C++ headers, `libgcc_s`, `libstdc++`) is copied in after: that is compiler
  runtime, not the libc.
- **pass 2 — socle**: `libzlib, libdrm, libseccomp, libmnl, popt`
- **pass 3 — tools**: `util-linux, libnftnl, iptables, kmod, e2fsprogs, gptfdisk,
  iproute2, kbd, busybox`
- **pass 4 — console**: the fonts (`dejavu`, `poppins`). The drawing is pure Go
  (`internal/display/gui/render`, fogleman/gg); this pass ships the TTF faces
  the console loads.
- **pass 1 — kernel**: `linux` + the fragment (independent; builds alongside)
- **pass 5 — runtime**: `containerd, runc` and the CNI plugins, from upstream
  release artifacts
- **pass 6 — assemble**: systemd-boot and genimage, plus the
  inputs `assemble.sh` needs; `build.sh` runs it into `build/out`

Each component is its own container layer, so changing one recipe rebuilds only
it and what depends on it.

### The staging sysroot

`/sysroot` is the tree we own: the libc pass seeds it, every recipe installs into
it with `DESTDIR=/sysroot`, and every compile points at it with
`--sysroot=/sysroot`. It carries our glibc, the kernel headers and the components
— and nothing from the base distribution: one directory we own, carried forward
with `COPY --from=<pass> /sysroot /sysroot`.

## Running it

From the repository root:

```bash
make image            # the whole disk (build/out/vates.qcow2)
```

or, from `build/`, a single stage:

```bash
./build.sh socle      # one pass (default)
./build.sh console    # the console stack
./build.sh kernel     # the kernel
./build.sh follow     # tail the build log
```

`build.sh` fetches every pinned source from its upstream URL into `dl/` and
verifies its sha256 before the build, so the container needs no network.

The base image is Fedora (RHEL-family), never Debian; override with
`VATES_SCRATCH_BASE`. For Docker instead of Podman, the Containerfile is
unchanged:

```bash
docker build -f Containerfile --target console \
  --build-arg BASE=fedora:44 -t vates-scratch:console .
```

## The pins

`pins.conf` lists each component's version, source tarball, sha256 and **upstream
URL**, plus an optional space-separated list of **mirrors** tried only when the
upstream fails or serves a file the sha256 rejects. It is maintained by hand —
there is no generator reading another project's metadata. To add or bump a
component, edit the file; the sha256 comes from the upstream release (its
checksum or signature file) or from `sha256sum` of a tarball verified some other
way. A mirror may name the file differently; the sha256 is what makes it the
same artifact, and it is checked either way.

## Layout

```text
build.sh              podman/docker driver; fetches+verifies sources, builds a stage
Containerfile         the passes, as stages
pins.conf             maintained: version|source|sha256|url[|mirrors] per component
board/                our kernel and busybox fragments (self-contained copy)
overlay/              what goes into the rootfs (our config)
lib/common.sh         native env + fetch/verify/unpack + autotools/meson drivers
lib/build-libc.sh     pass -1: kernel headers + glibc + compiler runtime seed
lib/cross.ini         meson cross file
lib/run-recipe.sh     sources common.sh + the recipe, runs build()
recipes/<pass>/<c>.sh one recipe per component
recipes/host/*.sh     host tools built for the builder (genimage, systemd-boot)
dl/                   fetched source tarballs (gitignored)
```

Inside the Containerfile, beyond the passes: a `go-base` stage (Fedora's Go) with
`vates` (the vates binaries) built from it; the `guest` pass (the Rust Xen guest
agent and libxenstore) on `socle`; and systemd-boot built in the `disk` stage.

## Status

- [x] graph extracted, passes designed
- [x] pins from upstream, all URLs verified
- [x] pass -1 (glibc) + pass 2 (socle) — glibc links and runs, verified in-pass
- [x] pass 3 (tools) + pass 4 (console) + pass 5 (containerd/runc/CNI)
- [x] pass 1 (kernel) + pass 6 (assemble) — a bootable A/B disk (`out/vates.qcow2`)
- [x] the disk boots under UEFI: systemd-boot → kernel → `vates-sysinit` → console
- [x] nothing is built on the host: the vates binaries, the Rust Xen agent and
      systemd-boot are all built by the Containerfile (recipes,
      with the Go and Rust toolchains borrowed from the base)
- [x] the disk boots and `make cluster CP=1 WORKERS=0` brings up a real
      single-node control plane: etcd, apiserver, controller-manager, scheduler,
      kube-vip, kube-proxy, flannel and coredns all Running, node Ready
- [ ] dm-verity, the updater (`vateskctl ab update`), the CAPI provider
