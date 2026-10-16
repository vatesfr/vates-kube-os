package cni

import "github.com/vatesfr/vates-kube-os/vatescfg"

// none is the pod network the node does not install: the cluster's CNI is the
// operator's, applied from the cluster side. The node writes no manifest, keeps
// no runtime directory of its own, and says so out loud when it skips the apply.
//
// It is the one value for which kube-proxy can be left out, and it replaces it
// by delegation: vatescfg only allows cluster.proxy.disabled with cni.plugin:
// none (see its Validate), precisely because the operator then installs a CNI
// that does implement Services. The node cannot see that CNI, but it knows the
// contract, so it reports true and lets bootstrapPhases drop the kube-proxy
// addon -- which it could not rely on the operator to clean up afterwards.
type none struct{}

// none implements Plugin.
var _ Plugin = (*none)(nil)

func (*none) Name() string { return vatescfg.CNINone }

func (*none) ManifestPath() string { return "" }

func (*none) Manifest(*vatescfg.Config) ([]byte, error) { return nil, nil }

func (*none) RunDirs() []string { return nil }

func (*none) ReplacesKubeProxy() bool { return true }
