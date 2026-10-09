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
