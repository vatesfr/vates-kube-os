package ab

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// slotNames are the two roots.
var slotNames = []string{"a", "b"}

// trials is how many attempts a freshly switched-to entry gets before
// systemd-boot gives up on it. Three survives a transient failure and rolls a
// genuinely bad update back in a few minutes.
const trials = 3

// Status is what an operator wants to know about the two slots.
type Status struct {
	// Entries maps each slot to the base name of its boot entry on the ESP.
	Entries map[string]string
	// Running is the slot the kernel was started from, "a" or "b", or "" when
	// the command line does not name one.
	Running string
	// Default is the entry name loader.conf points at, boot counter included.
	Default string
}

// GetStatus reports the running slot, the default entry and both entry names.
func GetStatus() (Status, error) {
	e, err := mountESP()
	if err != nil {
		return Status{}, err
	}
	defer e.unmount()

	st := Status{Entries: map[string]string{}, Default: e.defaultEntry()}
	st.Running, _ = e.currentSlot()
	for _, s := range slotNames {
		if n, err := e.entryFileName(s); err == nil {
			st.Entries[s] = n
		}
	}
	return st, nil
}

// Switch points systemd-boot at a slot and marks its entry a trial, so a slot
// that does not come up is rolled back from automatically.
func Switch(slot string) error {
	e, err := mountESP()
	if err != nil {
		return err
	}
	defer e.unmount()
	return e.switchTo(slot)
}

// Bless makes the RUNNING slot's boot entry permanent, dropping its boot
// counter. PID 1 calls it once the OS is up.
func Bless() error {
	e, err := mountESP()
	if err != nil {
		return err
	}
	defer e.unmount()
	s, err := e.currentSlot()
	if err != nil {
		return err
	}
	return e.bless(s)
}

// esp is the mounted EFI System Partition, where the boot entries live.
type esp struct {
	path string
}

// mountESP mounts the ESP read-write at a temporary directory. It is one of the
// few things a node writes outside /var, and it is where systemd-boot reads the
// entries and keeps the boot counters.
func mountESP() (*esp, error) {
	dev, err := espDevice()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "vates-esp-")
	if err != nil {
		return nil, err
	}
	if err := syscall.Mount(dev, dir, "vfat", 0, ""); err != nil {
		_ = os.Remove(dir)
		return nil, fmt.Errorf("mount %s: %w", dev, err)
	}
	return &esp{path: dir}, nil
}

func (e *esp) unmount() {
	_ = syscall.Unmount(e.path, 0)
	_ = os.Remove(e.path)
}

func (e *esp) entriesDir() string { return filepath.Join(e.path, "loader", "entries") }
func (e *esp) loaderConf() string { return filepath.Join(e.path, "loader", "loader.conf") }

// entryOptions returns the `options ...` line of a slot's entry, counter or not.
func (e *esp) entryOptions(slot string) (string, error) {
	name, err := e.entryFileName(slot)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(e.entriesDir(), name+".conf"))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "options "); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("entry %s has no options line", name)
}

// entryFileName finds the slot's entry file, whose name carries a boot counter
// (`vates-a+3`) while it is a trial and none once it is blessed (`vates-a`).
func (e *esp) entryFileName(slot string) (string, error) {
	base := "vates-" + slot
	matches, _ := filepath.Glob(filepath.Join(e.entriesDir(), base+"*.conf"))
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".conf")
		if name == base || strings.HasPrefix(name, base+"+") {
			return name, nil
		}
	}
	return "", fmt.Errorf("no boot entry for slot %s in %s", slot, e.entriesDir())
}

// currentSlot maps the root=PARTUUID on the kernel command line to a slot.
func (e *esp) currentSlot() (string, error) {
	uuid, err := cmdlineRootPartUUID()
	if err != nil {
		return "", err
	}
	for _, s := range slotNames {
		opts, err := e.entryOptions(s)
		if err != nil {
			continue
		}
		if strings.Contains(opts, uuid) {
			return s, nil
		}
	}
	return "", fmt.Errorf("no slot matches root PARTUUID %s", uuid)
}

func (e *esp) defaultEntry() string {
	b, err := os.ReadFile(e.loaderConf())
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "default "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// switchTo makes a slot the one systemd-boot boots next: its entry sorts first,
// and it becomes a TRIAL. A slot that does not come up has its boot counter run
// down by systemd-boot, which then sorts it after the other slot -- the rollback.
// There is no `default` to write: the sort IS the selection.
func (e *esp) switchTo(slot string) error {
	if slot != "a" && slot != "b" {
		return fmt.Errorf("slot %q is not a or b", slot)
	}
	other := "b"
	if slot == "b" {
		other = "a"
	}
	if err := e.makeEntry(other, 0, "1"); err != nil {
		return err
	}
	return e.makeEntry(slot, trials, "0")
}

// makeEntry gives a slot's entry the wanted boot counter (0 = none) and
// sort-key, renaming the file when the counter changes.
func (e *esp) makeEntry(slot string, tries int, sortKey string) error {
	base := "vates-" + slot
	name, err := e.entryFileName(slot)
	if err != nil {
		return err
	}
	want := base
	if tries > 0 {
		want = fmt.Sprintf("%s+%d", base, tries)
	}
	if name != want {
		if err := os.Rename(filepath.Join(e.entriesDir(), name+".conf"),
			filepath.Join(e.entriesDir(), want+".conf")); err != nil {
			return err
		}
	}
	return e.setSortKey(want, sortKey)
}

// bless makes the slot's entry permanent by dropping its boot counter.
func (e *esp) bless(slot string) error {
	base := "vates-" + slot
	name, err := e.entryFileName(slot)
	if err != nil {
		return err
	}
	if name == base {
		return nil // already blessed
	}
	return os.Rename(filepath.Join(e.entriesDir(), name+".conf"),
		filepath.Join(e.entriesDir(), base+".conf"))
}

// setSortKey sets (or inserts) the `sort-key` line of an entry. systemd-boot
// sorts by it, ascending, with "bad" entries pushed last regardless -- so the
// active slot carries "0" and the other "1".
func (e *esp) setSortKey(name, key string) error {
	path := filepath.Join(e.entriesDir(), name+".conf")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "sort-key ") {
			lines[i] = "sort-key " + key
			return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		}
	}
	// No sort-key line yet: put it just after the title (never before it).
	at := 0
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "title ") {
		at = 1
	}
	lines = append(lines[:at], append([]string{"sort-key " + key}, lines[at:]...)...)
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// espDevice finds the EFI System Partition: partition 1 of the disk the root is
// on. This image always puts it there (genimage.cfg), and the two naming schemes
// cover virtio/xen and nvme/mmc.
func espDevice() (string, error) {
	part, err := rootPartition()
	if err != nil {
		return "", err
	}
	disk, err := blockParent(part)
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{disk + "1", disk + "p1"} {
		dev := filepath.Join("/dev", candidate)
		if _, err := os.Stat(dev); err == nil {
			return dev, nil
		}
	}
	return "", fmt.Errorf("no ESP (partition 1) on %s", disk)
}

// rootPartition returns the block device name the root filesystem is mounted
// from: "vda2", "nvme0n1p4", ...
func rootPartition() (string, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[4] != "/" {
			continue
		}
		src := fields[len(fields)-2]
		if dev, ok := strings.CutPrefix(src, "/dev/"); ok {
			return dev, nil
		}
	}
	return "", fmt.Errorf("cannot tell which device the root is mounted from")
}

// blockParent returns the disk a partition belongs to, from the block device's
// name: xvda4 -> xvda, nvme0n1p4 -> nvme0n1. Read from sysfs rather than parsed
// from the name, so the naming scheme is the kernel's problem.
func blockParent(part string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", part))
	if err != nil {
		return "", err
	}
	return filepath.Base(filepath.Dir(resolved)), nil
}

// RootPartUUID is the root=PARTUUID the kernel was started with. PID 1 records
// it so an offline read of the disk says which slot booted.
func RootPartUUID() (string, error) { return cmdlineRootPartUUID() }

// cmdlineRootPartUUID extracts the PARTUUID from the kernel command line.
func cmdlineRootPartUUID() (string, error) {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", err
	}
	for _, tok := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(tok, "root=PARTUUID="); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("no root=PARTUUID= on the kernel command line")
}
