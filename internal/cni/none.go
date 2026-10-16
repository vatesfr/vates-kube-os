package cni

import "github.com/vatesfr/vates-kube-os/vatescfg"

// none is the pod network the node does not install: the cluster's CNI is the
// operator's, applied from the cluster side. The node writes no manifest, keeps
// no runtime directory of its own, and says so out loud when it skips the apply.
type none struct{}

// none implements Plugin.
var _ Plugin = (*none)(nil)

func (*none) Name() string { return vatescfg.CNINone }

func (*none) ManifestPath() string { return "" }

func (*none) Manifest(*vatescfg.Config) ([]byte, error) { return nil, nil }

func (*none) RunDirs() []string { return nil }
