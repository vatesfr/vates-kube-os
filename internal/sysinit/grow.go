package sysinit

// The /var partition: mounting it, and growing it to fill a disk the
// hypervisor has made bigger.
//
// /var is a separate partition for two reasons that are one reason. It is the
// part of the machine that is the machine -- the container store, the kubelet's
// state, and one day a CSI's volumes -- so it must survive an OS change; and the
// hypervisor grows a machine by growing its disk, which only the LAST partition
// can absorb. So /var is last, always, and this is where the grown space lands.
//
// Everything here is best-effort and idempotent. A machine that cannot resolve
// or mount /var keeps running with /var on the root filesystem, which is what
// every node did before this partition existed; a machine whose disk did not
// grow does nothing at all.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// The two lines of `sgdisk -i` that give a partition's extent, in 512-byte
// sectors.
var (
	firstSectorRe = regexp.MustCompile(`(?m)^First sector:\s*([0-9]+)`)
	lastSectorRe  = regexp.MustCompile(`(?m)^Last sector:\s*([0-9]+)`)
)

// varLabel is the filesystem label of the /var partition. Resolved by label and
// not by PARTUUID because there is no udev to create /dev/disk/by-*, and the
// label travels in the filesystem, which neither the partition resize nor
// resize2fs touches.
const varLabel = "vates-var"

// mountVar mounts the /var partition, growing it first when the disk is bigger
// than the partition table says.
//
// It runs before anything that writes to /var -- containerd, the kubelet, the
// logs this process itself redirects -- so that they all write to the partition
// rather than to the root filesystem underneath it, which the mount would then
// hide.
func mountVar() {
	dev, err := byLabel(varLabel)
	if err != nil {
		kmsg("var: no partition labelled %s (%v); /var stays on the root filesystem", varLabel, err)
		return
	}
	growVarPartition(dev)

	if err := os.MkdirAll("/var", 0o755); err != nil {
		kmsg("var: mkdir /var: %v", err)
		return
	}
	if isMountpoint("/var") {
		return
	}
	if err := syscall.Mount(dev, "/var", "ext4", 0, ""); err != nil {
		kmsg("var: mount %s on /var: %v", dev, err)
		return
	}
	kmsg("var: mounted %s on /var", dev)

	// The paths that are written at runtime are symlinks into /var (see
	// board/vates/overlay/etc and overlay/usr/libexec): the root is read-only,
	// so what a node writes about itself -- its certificates, its kubeconfigs,
	// its hostname -- and what a hostPath points at must land on the partition
	// that persists. The symlink TARGETS have to exist before anything writes
	// THROUGH the symlink: a dangling symlink is not a directory, so
	// `MkdirAll /etc/kubernetes/pki` fails at its first component.
	for _, d := range varRuntimeDirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			kmsg("var: mkdir %s: %v", d, err)
		}
	}

	// /var/run -> /run: the standard link, recreated on the mounted /var at
	// every boot. It is a link and not a directory because the CNI plugins are
	// compiled with /var/run paths -- Cilium's cilium-cni dials
	// /var/run/cilium/cilium.sock -- while the agent's cilium-run hostPath is
	// /run/cilium, the tmpfs. Through the link the plugin's compiled path
	// reaches the socket the agent listens on. Without it, the path is a
	// directory that does not exist, and every pod's sandbox fails with
	// "dial unix /var/run/cilium/cilium.sock: no such file or directory"
	// while the node stays Ready (the kubelet is happy as soon as a conflist
	// exists; only the plugin's first dial proves the socket is reachable).
	if err := ensureSymlink("/var/run", "/run"); err != nil {
		kmsg("var: /var/run -> /run: %v", err)
	}
}

// ensureSymlink makes linkPath a symlink to target, idempotently, replacing
// what is there only when it is a symlink (with a different target) or a
// directory.
//
// A directory is replaced and not kept: a real /var/run would shadow the tmpfs
// /run and resolve the CNI plugins' compiled paths to a place the agent never
// listens, which is the exact failure the link exists to prevent. The replaced
// directory is runtime state (sockets, pid files) that the processes that own
// it recreate, so removing it is safe. A regular file is an error and is left
// alone: it is not something this boot is allowed to delete.
func ensureSymlink(linkPath, target string) error {
	le, err := os.Lstat(linkPath)
	switch {
	case err == nil && le.Mode()&os.ModeSymlink != 0:
		if dst, e := os.Readlink(linkPath); e == nil && dst == target {
			return nil // already the right link
		}
		if err := os.Remove(linkPath); err != nil {
			return err
		}
	case err == nil && le.IsDir():
		if err := os.RemoveAll(linkPath); err != nil {
			return err
		}
	case err == nil:
		return fmt.Errorf("%s exists and is neither a symlink nor a directory", linkPath)
	case !os.IsNotExist(err):
		return err
	}
	return os.Symlink(target, linkPath)
}

// varRuntimeDirs are the /var directories the image's runtime-written symlinks
// resolve into. The symlinks live in the image; these are their targets, and
// they must exist on the mounted /var before anything runs. The /etc ones carry
// the node's own state; the last is the target of /usr/libexec/kubernetes.
//
// The last one is load-bearing for the controller manager: kubeadm's static pod
// declares /usr/libexec/kubernetes/kubelet-plugins/volume/exec as a
// DirectoryOrCreate hostPath, so containerd mkdirs it on the HOST before the
// container starts. On the read-only root that mkdir fails with EROFS and
// kube-controller-manager never leaves CreateContainerError.
var varRuntimeDirs = []string{
	"/var/lib/vates/etc",
	"/var/lib/vates/etc/kubernetes",
	"/var/lib/vates/etc/kubelet",
	"/var/lib/vates/etc/vates",
	"/var/lib/vates/etc/systemd",
	"/var/lib/vates/usr/libexec/kubernetes/kubelet-plugins/volume/exec",
}

// byLabel resolves a filesystem label to its device without udev. blkid is in
// the image; the caller treats any error as "not there".
func byLabel(label string) (string, error) {
	out, err := exec.Command("blkid", "-L", label).Output()
	if err != nil {
		return "", err
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return "", fmt.Errorf("no device labelled %s", label)
	}
	return dev, nil
}

// growVarPartition extends the /var partition and its filesystem to fill the
// disk, when the disk has grown under it.
//
// The sequence is the one every growpart does, spelled out so each step can say
// what it is for:
//
//  1. move the secondary GPT header to the new end of the disk: on a grown disk
//     it still points at the old end, and the table is inconsistent until it
//     does not;
//  2. recreate the last partition with the same start and no end, so it now
//     reaches the new end of the disk (the type defaults to Linux filesystem and
//     the filesystem inside is untouched);
//  3. tell the kernel the partition grew -- BLKPG, because BLKRRPART re-reads
//     the whole table and fails with EBUSY while the root is mounted;
//  4. grow the filesystem into the new space with resize2fs.
//
// Only the last partition is touched. The root is never moved, which is the
// whole reason /var is the last one.
func growVarPartition(dev string) {
	name := filepath.Base(dev)
	disk, err := blockParent(name)
	if err != nil {
		kmsg("var: parent of %s: %v", dev, err)
		return
	}

	diskSectors, ok := readSysUint(filepath.Join("/sys/class/block", disk, "size"))
	if !ok {
		return
	}
	partStart, ok := readSysUint(filepath.Join("/sys/class/block", name, "start"))
	if !ok {
		return
	}
	partSectors, ok := readSysUint(filepath.Join("/sys/class/block", name, "size"))
	if !ok {
		return
	}
	pno, ok := readSysUint(filepath.Join("/sys/class/block", name, "partition"))
	if !ok || pno == 0 {
		return
	}
	if partStart+partSectors >= diskSectors {
		// The partition already fills the disk -- but the FILESYSTEM may not.
		// A previous boot can have grown the partition and NOT the filesystem
		// (a BLKPG the kernel refused, a resize2fs that errored), and this boot
		// would otherwise return here and never repair it: measured, /var's
		// partition at 3967 MiB with its ext4 still at 231 MiB, and the kubelet
		// dying with "no space left on device" for ever. resize2fs is a no-op
		// when they already match, so it is safe on every boot.
		resizeVarFilesystem(dev)
		return
	}

	diskDev := filepath.Join("/dev", disk)
	pnoStr := strconv.FormatUint(pno, 10)
	kmsg("var: disk grew; extending %s (partition %s) to fill %d sectors", dev, pnoStr, diskSectors)

	if out, err := exec.Command("sgdisk", "-e", diskDev).CombinedOutput(); err != nil {
		kmsg("var: sgdisk -e %s: %v: %s", diskDev, err, strings.TrimSpace(string(out)))
		return
	}
	recreate := exec.Command("sgdisk", "-d", pnoStr,
		"-n", fmt.Sprintf("%s:%d:0", pnoStr, partStart), diskDev)
	if out, err := recreate.CombinedOutput(); err != nil {
		kmsg("var: sgdisk resize %s: %v: %s", diskDev, err, strings.TrimSpace(string(out)))
		return
	}

	// Read back where the partition now ends, rather than assuming it ends at
	// the last sector of the disk. It does not: a GPT reserves the last 33
	// sectors for its backup header and entries, so the partition ends at the
	// last USABLE sector. Telling the kernel a larger partition makes resize2fs
	// grow the filesystem PAST its partition -- an inconsistency e2fsck refuses
	// ("filesystem size ... is larger than the physical size of the device").
	start, end, err := partitionExtent(diskDev, pnoStr)
	if err != nil {
		// The partition is grown in the GPT but the kernel is not told, and the
		// filesystem is left alone rather than risked. The next boot retries.
		kmsg("var: %s: cannot read the new partition extent, leaving the filesystem alone: %v", diskDev, err)
		return
	}
	// BLKPG's start and length are in bytes, not sectors (the kernel shifts by
	// 9); sector counts from the GPT are scaled up.
	if err := blkpgResize(diskDev, int(pno), int64(start)*sectorSize, int64(end-start+1)*sectorSize); err != nil {
		// Not fatal on its own: resize2fs below reports the truth. If it fails
		// too, the next boot tries again.
		kmsg("var: BLKPG resize partition %s: %v", pnoStr, err)
	}
	resizeVarFilesystem(dev)
}

// resizeVarFilesystem grows the /var filesystem into its partition. It is
// idempotent -- resize2fs does nothing when the filesystem already fills the
// partition -- so it runs on EVERY boot, not only on the boot that grew the
// partition. That is what repairs a filesystem an earlier boot left behind,
// rather than leaving /var too small for ever.
func resizeVarFilesystem(dev string) {
	if out, err := exec.Command("resize2fs", dev).CombinedOutput(); err != nil {
		kmsg("var: resize2fs %s: %v: %s", dev, err, strings.TrimSpace(string(out)))
		return
	}
	kmsg("var: filesystem on %s fills its partition", dev)
}

// partitionExtent reads a partition's first and last sector, in 512-byte units,
// from the GPT that sgdisk has just rewritten.
//
// The kernel is not asked: it still holds the old size until the BLKPG below,
// and it is the GPT that decides where the partition really ends.
func partitionExtent(disk, pno string) (start, end uint64, err error) {
	out, err := exec.Command("sgdisk", "-i", pno, disk).Output()
	if err != nil {
		return 0, 0, err
	}
	first := firstSectorRe.FindSubmatch(out)
	last := lastSectorRe.FindSubmatch(out)
	if first == nil || last == nil {
		return 0, 0, fmt.Errorf("sgdisk -i %s reported no sector range", disk)
	}
	start, _ = strconv.ParseUint(string(first[1]), 10, 64)
	end, _ = strconv.ParseUint(string(last[1]), 10, 64)
	return start, end, nil
}

// blockParent returns the disk a partition belongs to, from the block device's
// name: xvda4 -> xvda, nvme0n1p4 -> nvme0n1. Read from sysfs rather than parsed
// from the name, so the naming scheme is the kernel's problem, not this one's.
func blockParent(part string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", part))
	if err != nil {
		return "", err
	}
	return filepath.Base(filepath.Dir(resolved)), nil
}

// readSysUint reads a decimal value from a sysfs attribute. A missing file or a
// value that does not parse is not an error worth reporting: the caller simply
// does not grow.
func readSysUint(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// The BLKPG ioctl, from <linux/blkpg.h>. unsafe is used because the ioctl takes
// a blkpg_ioctl_arg that points at a blkpg_partition, a pointer inside a struct
// that cannot be expressed without it. This is the one place in the binary where
// unsafe is warranted, and it is confined to the declarations below.
//
// Two things this struct gets wrong if written from memory, both caught by a
// boot that grew the partition but not the filesystem:
//
//   - BLKPG_RESIZE_PARTITION is 3. 2 is BLKPG_DEL_PARTITION, which deletes the
//     partition in the kernel's view instead of resizing it;
//   - `start` and `length` are in BYTES, unlike almost every other block
//     interface: the kernel shifts them right by 9. A sector count from sysfs
//     has to be scaled up, or the resize is silently 512 times too small.
const (
	blkpgIoctl           = 0x1269 // _IO(0x12, 105)
	blkpgResizePartition = 3
	// BLKPG_DEVNAMELTH (64) + BLKPG_VOLNAMELTH (64), the two name fields that
	// follow pno. Unused by a resize, but the struct must match the kernel's.
	blkpgNameLen = 128
	// The sector size the kernel divides blkpg_partition's byte fields by.
	sectorSize = 512
)

type blkpgPartition struct {
	Start   int64
	Length  int64
	Pno     int32
	Devname [blkpgNameLen]byte
}

type blkpgIoctlArg struct {
	Op      int32
	Flags   int32
	Datalen int32
	Data    unsafe.Pointer
}

// blkpgResize tells the kernel that one partition's size changed, without
// re-reading the whole table.
//
// BLKRRPART -- the obvious "reread the partition table" -- fails with EBUSY
// while any partition of that disk is mounted, and the root always is. BLKPG
// with BLKPG_RESIZE_PARTITION resizes a single partition in place. It is what
// `partx -u` does under the hood, and the reason partx does not have to be in
// this image.
func blkpgResize(disk string, pno int, start, length int64) error {
	f, err := os.OpenFile(disk, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	part := blkpgPartition{Start: start, Length: length, Pno: int32(pno)}
	arg := blkpgIoctlArg{
		Op:      blkpgResizePartition,
		Datalen: int32(unsafe.Sizeof(part)),
		Data:    unsafe.Pointer(&part),
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkpgIoctl, uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return errno
	}
	return nil
}
