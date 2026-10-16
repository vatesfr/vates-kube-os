// Package cni holds the pod networks the node knows how to install itself.
//
// A pod network -- the cni.plugin field of vates-node.yaml -- is more than a
// manifest. The same choice also decides where that manifest is written, which
// runtime directories under /run its agent needs, and whether it implements
// Services itself. Each of those used to be a case in a switch somewhere in
// firstboot, with flannel as the implicit default behind a `default:` label.
// They are now a Plugin, resolved through Lookup: adding a pod network is a
// file here and one line in the registry, and nothing else in the system names
// a CNI.
package cni

import "github.com/vatesfr/vates-kube-os/vatescfg"

// Plugin is one pod network the node can install itself.
//
// The implementations are stateless, and the registry that holds them is shared
// and read-only after initialisation.
type Plugin interface {
	// Name is the cni.plugin value that selects it. It is the registry key,
	// restated -- what a caller has when it holds a Plugin but not the document
	// that chose it.
	Name() string

	// ManifestPath is where the manifest is written before being applied.
	//
	// Empty when the node installs nothing, which is cni.plugin: none: the
	// cluster's CNI is the operator's, and the node writes no file at all.
	ManifestPath() string

	// Manifest renders the manifest the bootstrapping control plane applies.
	//
	// It returns (nil, nil) for a plugin that installs nothing. Otherwise the
	// manifest is embedded and pinned (an artifact of the OS image, so a control
	// plane starts with no outbound access) and its image references pass
	// through the configured mirrors.
	Manifest(cfg *vatescfg.Config) ([]byte, error)

	// RunDirs are the runtime directories the plugin's agent needs under the
	// tmpfs /run. /run is wiped on every boot, so the node recreates them.
	//
	// Nil when the plugin needs none. A directory listed here is also labelled
	// for containers, so it must be the plugin's own state and not another's.
	RunDirs() []string

	// ReplacesKubeProxy reports whether the plugin implements Services itself.
	//
	// When true, kube-proxy must not be installed at all: deleting it afterwards
	// leaves its iptables rules behind. flannel and cilium, as installed here,
	// both run alongside kube-proxy and report false; none reports true, because
	// the operator then installs a CNI that does implement Services.
	ReplacesKubeProxy() bool
}

// registry is the one place a cni.plugin value is matched to its pod network.
//
// The keys are the values vatescfg accepts; its Validate refuses anything else,
// so Lookup resolves every document that came through it. The map is written
// only here, at initialisation, and read-only afterwards.
var registry = map[string]Plugin{
	vatescfg.CNIFlannel: &flannel{},
	vatescfg.CNICilium:  &cilium{},
	vatescfg.CNINone:    &none{},
}

// Lookup resolves a cni.plugin value to its pod network.
//
// ok is false for a name vatescfg does not accept. A caller turns that into an
// error rather than a silent no-op: a config that was never validated must not
// look like one that asked for nothing.
func Lookup(name string) (Plugin, bool) {
	p, ok := registry[name]
	return p, ok
}
