# Vates Kube OS
#
# Most targets are .PHONY on purpose: several share a name with a directory in
# the repository (test/, build/), and make would otherwise consider them up to
# date because the directory exists. The CLI binary is the exception: it is a
# real file target, so make rebuilds it only when a source is newer.
#
# `make` with no argument prints the list of targets.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# Where the image build tees its output (build/build.sh). Fixed, not per-run, so
# `make follow` is a stable command from any terminal -- useful when the build is
# driven by an agent rather than in front of you.
BUILD_LOG ?= $(CURDIR)/build/build.log

.PHONY: help image cli install follow check fmt vet lint go-test configdrives cluster cilium cluster-wait cluster-status cluster-down vip-failover demo-app template clean distclean

# The Kubernetes version the TEST CLUSTER runs. It is not a property of the OS
# image: the image carries a launcher and serves any supported version. This
# value reaches the config drives test/cluster.sh writes.
K8S_VERSION ?= v1.31.0

# Where `make install` puts the CLI. The images are deployed, not installed;
# only the CLI has an install story.
PREFIX ?= $(HOME)/.local

# `make template` imports the built disk and turns it into a Xen Orchestra VM
# template (the UUID the CAPI providers clone). The values it needs (XO_URL,
# XO_TOKEN, XO_POOL, XO_SR, NAME, VHD) come from a gitignored .env beside this
# Makefile -- see scripts/xo-template.sh for the list -- and an environment
# variable overrides the file, so `NAME=... make template` wins.

help: ## Show this help
	@printf '\nVates Kube OS\n\n'
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@printf '\nVariables: K8S_VERSION=%s (the test cluster runs it), PREFIX=%s (make install)\n\n' '$(K8S_VERSION)' '$(PREFIX)'

# --- building ---------------------------------------------------------------

# The one image: built from pinned upstream sources by build/ -- a Containerfile
# driven by podman, NOT a distribution. The userspace, glibc,
# the kernel, containerd, the console, systemd-boot and the kubelet image are all
# built inside it; see build/README.md.
#
# The disk lands in build/out/ as vates.qcow2 and vates.vhd. The first build is
# long (it compiles glibc, the kernel and the console stack); later ones are
# incremental, and adding a component no longer rebuilds the toolchain.
image: ## Build the disk (podman: from-scratch userspace + genimage)
	@echo "=== building the Vates Kube OS disk (from-scratch builder) ==="
	./build/build.sh disk
	@for f in vates.qcow2 vates.vhd; do \
		img="$$(ls -1 build/out/$$f 2>/dev/null)"; \
		printf '\n  image: %s\n' "$${img:-$$f not found}"; \
	done

# Turn the built disk into a Xen Orchestra VM template, replacing the previous
# one of the same name. The CAPI providers clone a template, and building one by
# hand (upload the VHD, create a VM, attach the disk, convert) is exactly what
# this automates. It is a host-side deploy step, not part of the image. Values
# come from a gitignored .env; `NAME=... make template` overrides them.
template: ## Create/replace the XO VM template (values from .env)
	@./scripts/xo-template.sh

# The operator CLI is a host artifact, shipped alongside the images: it runs off
# the node and is not part of the disk. It is a FILE target, not .PHONY, so make
# rebuilds it only when a Go source (or go.mod/go.sum, or the build script) is
# newer; `make cluster` builds it through the same script.
GO_SOURCES := $(shell find cmd internal vatescfg proto -name '*.go') go.mod go.sum scripts/build-vateskctl.sh

build/bin/vateskctl: $(GO_SOURCES)
	./scripts/build-vateskctl.sh

cli: build/bin/vateskctl ## Build the operator CLI (build/bin/vateskctl)

# --- installing -------------------------------------------------------------

install: build/bin/vateskctl ## Install the CLI under $(PREFIX)/bin
	install -Dm0755 build/bin/vateskctl "$(DESTDIR)$(PREFIX)/bin/vateskctl"

# --- following --------------------------------------------------------------

follow: ## Follow the image build log (when the build runs elsewhere)
	@cd build && ./build.sh follow

# --- checking ---------------------------------------------------------------

fmt: ## Fail if any Go file needs gofmt
	@out=$$(gofmt -l cmd internal vatescfg); \
	if [ -n "$$out" ]; then \
		echo "these files need gofmt:" >&2; echo "$$out" >&2; \
		echo "run: gofmt -w cmd internal vatescfg" >&2; exit 1; \
	fi
	@echo "gofmt: ok"

vet: ## Run go vet
	go vet ./... && echo "vet: ok"

lint: ## Run errcheck, with the exclusions this project chose (.errcheck-exclude)
	@command -v errcheck >/dev/null || { \
		echo "errcheck is not installed: go install github.com/kisielk/errcheck@latest" >&2; \
		exit 1; }
	errcheck -exclude .errcheck-exclude ./cmd/... ./internal/... ./vatescfg/... && echo "errcheck: ok"

go-test: ## Run the Go tests
	go test ./...

check: fmt vet lint go-test ## Everything to run before committing

# --- the cluster ------------------------------------------------------------

# configdrives regenerates the NoCloud ISOs from the directories beside them.
# They are derived files (the .iso is in .gitignore); the cluster writes its own.
configdrives: ## Regenerate the NoCloud config drives from their directories
	./scripts/mkconfigdrive.sh test/fixtures/cfgdrive
	./scripts/mkconfigdrive.sh test/fixtures/cfgdrive-capi
	./scripts/mkconfigdrive.sh test/fixtures/cfgdrive-cp
	./scripts/mkconfigdrive.sh test/fixtures/seed

# The host user must be in the qemu and libvirt groups (or root): the working
# tree is group-owned by qemu so the qemu user can walk to every config drive,
# and libvirt's polkit rule waives the password for qemu:///system for its own
# group only. test/cluster.sh checks both up front and prints the fix.
cluster: ## Bring up a cluster under libvirt (CP= WORKERS= DASHBOARD= CNI= IMAGE= EFIVARS= START_STAGGER= CONFIG_FORM=)
	K8S_VERSION=$(K8S_VERSION) CP=$(CP) WORKERS=$(WORKERS) DASHBOARD=$(DASHBOARD) CNI=$(CNI) IMAGE=$(IMAGE) EFIVARS=$(EFIVARS) START_STAGGER=$(START_STAGGER) CONFIG_FORM=$(CONFIG_FORM) ./test/cluster.sh up

cilium: ## Retrofit Cilium onto a running flannel test cluster (manual; CNI=cilium installs it from the image)
	./test/cilium.sh

cluster-wait: ## Wait until every machine is Ready (polls the condition)
	CP=$(CP) WORKERS=$(WORKERS) ./test/cluster.sh wait

cluster-status: ## Show the cluster's nodes, pods and virtual IP
	CP=$(CP) WORKERS=$(WORKERS) ./test/cluster.sh status

cluster-down: ## Remove every vates-cp-N / vates-worker-N machine
	./test/cluster.sh down

vip-failover: ## Kill the control plane holding the VIP and measure the takeover
	./test/vip-failover.sh

demo-app: ## Deploy a namespace and a small app, and check it serves traffic
	./test/demo-app.sh

# --- housekeeping -----------------------------------------------------------

clean: ## Remove build artifacts and the test VM's scratch state
	./scripts/clean.sh

distclean: ## clean, plus the expensive host-built prerequisites
	./scripts/clean.sh distclean
