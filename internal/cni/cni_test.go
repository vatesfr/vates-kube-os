package cni

import (
	"slices"
	"testing"

	"github.com/vatesfr/vates-kube-os/vatescfg"
)

// Lookup is the registry's only door: every value vatescfg accepts must resolve,
// and every value it does not must come back not-ok rather than as some default
// pod network. The Name a plugin reports is the key it was found under.
func TestLookup(t *testing.T) {
	tests := []struct {
		name   string
		wantOK bool
	}{
		{name: vatescfg.CNIFlannel, wantOK: true},
		{name: vatescfg.CNICilium, wantOK: true},
		{name: vatescfg.CNINone, wantOK: true},
		{name: "weave", wantOK: false},
		{name: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := Lookup(tt.name)
			if ok != tt.wantOK {
				t.Fatalf("Lookup(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
			}
			if ok && p.Name() != tt.name {
				t.Errorf("Lookup(%q).Name() = %q, want %q", tt.name, p.Name(), tt.name)
			}
		})
	}
}

// The manifest path is the plugin's to declare, and only the plugin that writes
// nothing leaves it empty.
func TestManifestPath(t *testing.T) {
	tests := []struct {
		plugin string
		want   string
	}{
		{plugin: vatescfg.CNIFlannel, want: FlannelManifestPath},
		{plugin: vatescfg.CNICilium, want: CiliumManifestPath},
		{plugin: vatescfg.CNINone, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.plugin, func(t *testing.T) {
			p, ok := Lookup(tt.plugin)
			if !ok {
				t.Fatalf("Lookup(%q) failed", tt.plugin)
			}
			if got := p.ManifestPath(); got != tt.want {
				t.Errorf("ManifestPath(%q) = %q, want %q", tt.plugin, got, tt.want)
			}
		})
	}
}

// The runtime directories differ by plugin: /run is a tmpfs wiped on every boot,
// so each plugin's set is exactly what its agent writes there.
func TestRunDirs(t *testing.T) {
	tests := []struct {
		plugin string
		want   []string
	}{
		{plugin: vatescfg.CNIFlannel, want: []string{"/run/flannel"}},
		{plugin: vatescfg.CNICilium, want: []string{"/run/cilium", "/run/netns"}},
		{plugin: vatescfg.CNINone, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.plugin, func(t *testing.T) {
			p, ok := Lookup(tt.plugin)
			if !ok {
				t.Fatalf("Lookup(%q) failed", tt.plugin)
			}
			if got := p.RunDirs(); !slices.Equal(got, tt.want) {
				t.Errorf("RunDirs(%q) = %v, want %v", tt.plugin, got, tt.want)
			}
		})
	}
}

// none installs nothing: a nil manifest and no error, which is what the caller
// reads to skip both the write and the apply.
func TestNoneInstallsNothing(t *testing.T) {
	p, ok := Lookup(vatescfg.CNINone)
	if !ok {
		t.Fatalf("Lookup(%q) failed", vatescfg.CNINone)
	}
	if m, err := p.Manifest(nil); err != nil || m != nil {
		t.Errorf("none.Manifest() = (%v, %v), want (nil, nil)", m, err)
	}
}
