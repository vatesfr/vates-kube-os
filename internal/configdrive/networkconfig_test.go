package configdrive

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseNetworkConfig(t *testing.T) {
	tests := []struct {
		want    *StaticNetwork
		name    string
		doc     string
		wantErr bool
	}{
		{
			name: "reads the document Xen Orchestra writes for a static address",
			doc: `version: 2
ethernets:
  eth0:
    dhcp4: false
    addresses:
      - 10.30.1.42/24
    routes:
      - to: default
        via: 10.30.1.1
    nameservers:
      addresses: [10.30.1.1]
`,
			want: &StaticNetwork{
				Iface:       "eth0",
				Addresses:   []string{"10.30.1.42/24"},
				Gateway:     "10.30.1.1",
				Nameservers: []string{"10.30.1.1"},
			},
		},
		{
			name: "reads the older gateway4 spelling",
			doc: `version: 2
ethernets:
  eth0:
    addresses: [10.30.1.42/24]
    gateway4: 10.30.1.254
`,
			want: &StaticNetwork{
				Iface:     "eth0",
				Addresses: []string{"10.30.1.42/24"},
				Gateway:   "10.30.1.254",
			},
		},
		{
			name: "prefers routes over gateway4 when both are present",
			doc: `version: 2
ethernets:
  eth0:
    addresses: [10.30.1.42/24]
    gateway4: 10.30.1.254
    routes:
      - to: 10.96.0.0/12
        via: 10.30.1.9
      - to: default
        via: 10.30.1.1
`,
			want: &StaticNetwork{
				Iface:     "eth0",
				Addresses: []string{"10.30.1.42/24"},
				Gateway:   "10.30.1.1",
			},
		},
		{
			name: "asks for DHCP",
			doc: `version: 2
ethernets:
  eth0:
    dhcp4: true
`,
			want: nil,
		},
		{
			name: "has a stanza with nothing to apply",
			doc: `version: 2
ethernets:
  eth0:
    mtu: 1500
`,
			want: nil,
		},
		{
			name: "carries no ethernets at all",
			doc:  "version: 2\n",
			want: nil,
		},
		{
			name: "names several interfaces and takes the first",
			doc: `version: 2
ethernets:
  eth1:
    addresses: [10.30.9.9/24]
  eth0:
    addresses: [10.30.1.42/24]
`,
			want: &StaticNetwork{
				Iface:     "eth0",
				Addresses: []string{"10.30.1.42/24"},
			},
		},
		{
			name: "skips an interface that asks for DHCP to find a static one",
			doc: `version: 2
ethernets:
  eth0:
    dhcp4: true
  eth1:
    addresses: [10.30.1.42/24]
`,
			want: &StaticNetwork{
				Iface:     "eth1",
				Addresses: []string{"10.30.1.42/24"},
			},
		},
		{
			name:    "refuses a version this node does not read",
			doc:     "version: 1\nconfig: []\n",
			wantErr: true,
		},
		{
			name:    "refuses a document that is not YAML",
			doc:     "ethernets: [this is not a map\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNetworkConfig([]byte(tt.doc))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseNetworkConfig() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNetworkConfig() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseNetworkConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStaticNetworkFromDrive(t *testing.T) {
	// A drive is a directory: the ISO9660 spelling the OS has to cope with is
	// covered by Drive.File, and what matters here is which file is read.
	driveWith := func(t *testing.T, files map[string]string) *Drive {
		t.Helper()
		dir := t.TempDir()
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatalf("writing %s: %v", name, err)
			}
		}
		return Open(dir)
	}

	tests := []struct {
		files map[string]string
		want  *StaticNetwork
		name  string
	}{
		{
			name:  "a drive without a network-config asks for nothing",
			files: map[string]string{"meta-data": "instance-id: nocloud\n"},
			want:  nil,
		},
		{
			name: "a drive that asks for DHCP returns nothing to apply",
			files: map[string]string{
				NetworkConfigFile: "version: 2\nethernets:\n  eth0:\n    dhcp4: true\n",
			},
			want: nil,
		},
		{
			name: "a drive with a static address returns it",
			files: map[string]string{
				NetworkConfigFile: "version: 2\nethernets:\n  eth0:\n    addresses: [10.30.1.42/24]\n",
			},
			want: &StaticNetwork{Iface: "eth0", Addresses: []string{"10.30.1.42/24"}},
		},
		{
			name: "the upper-cased name some producers write is still found",
			files: map[string]string{
				"NETWORK-CONFIG": "version: 2\nethernets:\n  eth0:\n    addresses: [10.30.1.42/24]\n",
			},
			want: &StaticNetwork{Iface: "eth0", Addresses: []string{"10.30.1.42/24"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := driveWith(t, tt.files).StaticNetwork()
			if err != nil {
				t.Fatalf("StaticNetwork() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("StaticNetwork() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
