// Command render prints the CNI manifest a bootstrapping control plane would
// apply for a node document, so test/kind.sh applies exactly what the node
// does -- the embedded template, the pins and the mirrors -- rather than a copy.
//
//	go run ./test/kind/render image/config/vates-node.yaml cilium
//
// The plugin argument, when given, overrides cni.plugin in the document.
package main

import (
	"fmt"
	"os"

	"github.com/vatesfr/vates-kube-os/internal/firstboot"
	"github.com/vatesfr/vates-kube-os/vatescfg"
)

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: render <vates-node.yaml> [flannel|cilium]")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
}

func run(path string, plugin []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cfg, err := vatescfg.Load(data)
	if err != nil {
		return err
	}
	if len(plugin) == 1 {
		cfg.CNI.Plugin = plugin[0]
	}
	var manifest []byte
	switch cfg.CNI.Plugin {
	case vatescfg.CNIFlannel:
		manifest, err = firstboot.FlannelManifest(cfg)
	case vatescfg.CNICilium:
		manifest, err = firstboot.CiliumManifest(cfg)
	default:
		return fmt.Errorf("cni.plugin %q installs no manifest", cfg.CNI.Plugin)
	}
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(manifest)
	return err
}
