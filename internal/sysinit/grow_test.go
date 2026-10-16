package sysinit

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureSymlink pins the behaviour of the /var/run -> /run link: created
// when absent, left alone when already right, replaced when a directory or a
// wrong-target symlink shadows the tmpfs, and refused over a regular file.
//
// The directory case is the load-bearing one: a real /var/run resolves the CNI
// plugins' compiled paths to a place the agent never listens, which is the
// exact failure ("dial unix /var/run/cilium/cilium.sock: no such file") the
// link exists to prevent -- so the link must not merely be absent, it must win.
func TestEnsureSymlink(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup puts what the link path holds before ensureSymlink runs.
		setup func(t *testing.T, link string)
		// wantErr marks the one existing thing this boot must not delete.
		wantErr bool
	}{
		{
			name:  "absent: created",
			setup: func(t *testing.T, link string) {},
		},
		{
			name: "already the right link: left alone",
			setup: func(t *testing.T, link string) {
				if err := os.Symlink("/run", link); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a symlink to the wrong target: replaced",
			setup: func(t *testing.T, link string) {
				if err := os.Symlink("/elsewhere", link); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a directory shadowing the tmpfs: replaced",
			setup: func(t *testing.T, link string) {
				if err := os.MkdirAll(filepath.Join(link, "stale"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:    "a regular file: refused and left alone",
			wantErr: true,
			setup: func(t *testing.T, link string) {
				if err := os.WriteFile(link, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := filepath.Join(t.TempDir(), "var", "run")
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, link)

			err := ensureSymlink(link, "/run")
			if tc.wantErr {
				if err == nil {
					t.Fatal("ensureSymlink() succeeded over a regular file; it must refuse")
				}
				if _, statErr := os.Stat(link); statErr != nil {
					t.Error("ensureSymlink() deleted the regular file it refused")
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureSymlink() failed: %v", err)
			}
			le, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			if le.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("ensureSymlink() left %s as a %s, want a symlink", link, le.Mode())
			}
			if dst, _ := os.Readlink(link); dst != "/run" {
				t.Errorf("the link points at %q, want %q", dst, "/run")
			}
			// Idempotent: a second run does nothing and does not fail, because
			// the link is recreated on every boot.
			if err := ensureSymlink(link, "/run"); err != nil {
				t.Errorf("a second ensureSymlink() failed: %v", err)
			}
		})
	}
}

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
