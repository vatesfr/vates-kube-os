package sysinit

import (
	"os"
	"testing"
)

// TestVarRuntimeDirsRecreateTheSkeletonAndTheSymlinkTargets pins the /var
// directory list mountVar builds on the mounted partition. The partition is
// formatted EMPTY, so whatever is missing from this list is missing on the
// node: /var/tmp was measured absent, and a pod with a hostPath on it stayed in
// ContainerCreating with "does not exist".
//
// The /var/tmp case also guards a trap: the sticky bit is os.ModeSticky, not the
// literal 0o1777. In os.FileMode, bit 9 is not a permission bit, so
// syscallMode drops it and a directory created with 0o1777 is NOT sticky.
func TestVarRuntimeDirsRecreateTheSkeletonAndTheSymlinkTargets(t *testing.T) {
	got := make(map[string]os.FileMode, len(varRuntimeDirs))
	for _, d := range varRuntimeDirs {
		got[d.path] = d.mode
	}

	tests := []struct {
		name string
		path string
		want os.FileMode
	}{
		{"tmp is world-writable and sticky", "/var/tmp", 0o777 | os.ModeSticky},
		{"log", "/var/log", 0o755},
		{"cache", "/var/cache", 0o755},
		{"lib", "/var/lib", 0o755},
		{"the kubernetes config target", "/var/lib/vates/etc/kubernetes", 0o755},
		{"the kubelet config target", "/var/lib/vates/etc/kubelet", 0o755},
		{"the vates config target", "/var/lib/vates/etc/vates", 0o755},
		{"the systemd drop-in target", "/var/lib/vates/etc/systemd", 0o755},
		{"the flexvolume hostPath target", "/var/lib/vates/usr/libexec/kubernetes/kubelet-plugins/volume/exec", 0o755},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, ok := got[tt.path]
			if !ok {
				t.Fatalf("%s is missing from varRuntimeDirs", tt.path)
			}
			if mode != tt.want {
				t.Errorf("%s mode = %v, want %v", tt.path, mode, tt.want)
			}
		})
	}
}
