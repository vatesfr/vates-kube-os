package configdrive

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// NetworkConfigFile is the NoCloud network configuration, beside user-data and
// meta-data.
const NetworkConfigFile = "network-config"

// StaticNetwork is the static addressing a config drive asks for: the subset of
// the cloud-init network configuration this OS applies.
type StaticNetwork struct {
	// Iface is the interface the addressing belongs to, e.g. "eth0".
	Iface string
	// Addresses are CIDR addresses, e.g. "10.30.1.42/24".
	Addresses []string
	// Gateway is the default route. Optional.
	Gateway string
	// Nameservers are the resolvers, written to /etc/resolv.conf.
	Nameservers []string
}

// StaticNetwork reads the drive's network-config and returns the addressing it
// asks for, or nil when it asks for DHCP -- or says nothing at all, which is the
// common case and leaves the node on the DHCP client.
//
// The document is cloud-init's, in the netplan v2 form: it is what Xen
// Orchestra writes from the infrastructure provider's network_config, and what a
// KVM seed ISO carries, so reading it keeps the node's addressing independent of
// the hypervisor. The node runs no cloud-init; this is the part of it that
// matters here -- one interface, addresses, a default route and resolvers.
//
// The node's address is decided by whoever provisions the machine, not by the
// DHCP server, and that is the point: a control plane's identity is tied to its
// address, so a lease that can move is a control plane that can be lost on the
// next reboot.
func (d *Drive) StaticNetwork() (*StaticNetwork, error) {
	if !d.Has(NetworkConfigFile) {
		return nil, nil
	}
	raw, err := d.File(NetworkConfigFile)
	if err != nil {
		return nil, err
	}
	return parseNetworkConfig(raw)
}

// parseNetworkConfig reads the netplan v2 subset above.
func parseNetworkConfig(raw []byte) (*StaticNetwork, error) {
	var doc struct {
		Ethernets map[string]struct {
			DHCP4     *bool    `yaml:"dhcp4"`
			Addresses []string `yaml:"addresses"`
			// Gateway4 is netplan's older spelling, still written by hand and
			// by some tooling. routes is the current one; both are read.
			Gateway4 string `yaml:"gateway4"`
			Routes   []struct {
				To  string `yaml:"to"`
				Via string `yaml:"via"`
			} `yaml:"routes"`
			Nameservers struct {
				Addresses []string `yaml:"addresses"`
			} `yaml:"nameservers"`
		} `yaml:"ethernets"`
		Version int `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("network-config: %w", err)
	}
	if doc.Version != 0 && doc.Version != 2 {
		return nil, fmt.Errorf(
			"network-config: version %d is not the netplan v2 document this node reads", doc.Version)
	}

	// The document names the interface, so the caller does not have to. A node
	// has one, and the sorted order makes the choice deterministic when a
	// document carries several.
	names := make([]string, 0, len(doc.Ethernets))
	for name := range doc.Ethernets {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		eth := doc.Ethernets[name]
		if len(eth.Addresses) == 0 {
			// dhcp4, or a stanza with nothing to apply: either way the node
			// keeps asking the DHCP server.
			continue
		}
		out := &StaticNetwork{
			Iface:       name,
			Addresses:   eth.Addresses,
			Gateway:     eth.Gateway4,
			Nameservers: eth.Nameservers.Addresses,
		}
		for _, route := range eth.Routes {
			if route.Via != "" && (route.To == "default" || route.To == "0.0.0.0/0") {
				out.Gateway = route.Via
				break
			}
		}
		return out, nil
	}
	return nil, nil
}
