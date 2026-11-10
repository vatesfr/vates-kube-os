package hostinfo

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Info is one observation of the machine.
type Info struct {
	Hostname string
	Uptime   time.Duration
	CPUModel string
	CPUs     int
	MemTotal uint64 // bytes
	MemUsed  uint64 // bytes
	Procs    int
	Load     [3]float64

	// CPUBusy is the fraction of CPU time spent not idle between the previous
	// observation and this one. It is only meaningful once a second sample has
	// been taken; until then it is zero and HasCPU says so.
	CPUBusy float64
	HasCPU  bool

	Gateways []string
	DNS      []string
	NTP      []string

	// OS is the Linux distribution, from /etc/os-release, and Distribution the
	// Kubernetes one. Both are local, so the screen names them before the node
	// has joined.
	OS           string
	Distribution string
}

type cpuTimes struct{ busy, total uint64 }

// Reader keeps the state that cannot be read from a file: the previous CPU
// counters, whose difference is the only way to show a percentage rather than a
// total since boot.
type Reader struct {
	prev *cpuTimes
}

// Read collects everything. It never returns an error: a console that refuses
// to draw because one line of /proc was unreadable is worse than one with a
// blank field.
func (r *Reader) Read() Info {
	var i Info
	i.Hostname, _ = os.Hostname()
	i.Uptime = uptime()
	i.CPUModel, i.CPUs = cpuInfo()
	i.MemTotal, i.MemUsed = memInfo()
	i.Procs = procCount()
	i.Load = loadAvg()
	i.CPUBusy, i.HasCPU, r.prev = cpuBusy(r.prev)
	i.Gateways = gateways()
	i.DNS = dnsServers()
	i.NTP = ntpServers()
	i.OS = osName()
	i.Distribution = distribution()
	return i
}

// osName is NAME and VERSION_ID from /etc/os-release: "AlmaLinux 10.0". Short
// on purpose, PRETTY_NAME's codename does not fit a tile.
func osName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	return parseOSRelease(string(b))
}

func parseOSRelease(s string) string {
	var name, version string
	for line := range strings.SplitSeq(s, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "NAME":
			name = v
		case "VERSION_ID":
			version = v
		}
	}
	return strings.TrimSpace(name + " " + version)
}

// distribution is the Kubernetes distribution, from DASHBOARD_K8S_DISTRIBUTION:
// the image that installs the console says what it built. Every node this
// console knows is bootstrapped by kubeadm, hence the default.
func distribution() string {
	if v := os.Getenv("DASHBOARD_K8S_DISTRIBUTION"); v != "" {
		return v
	}
	return "kubeadm"
}

// CPUPercent returns the busy percentage as a number, or -1 when unknown.
func (i Info) CPUPercent() float64 {
	if !i.HasCPU {
		return -1
	}
	return i.CPUBusy * 100
}

// MemPercent is the used fraction, 0 when the total is unknown.
func (i Info) MemPercent() float64 {
	if i.MemTotal == 0 {
		return 0
	}
	return float64(i.MemUsed) / float64(i.MemTotal) * 100
}

func uptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

func cpuInfo() (model string, cpus int) {
	cpus = runtime.NumCPU()
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", cpus
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.HasPrefix(line, "model name") {
			if _, v, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(v), cpus
			}
		}
	}
	return "", cpus
}

func memInfo() (total, used uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var avail uint64
	for line := range strings.SplitSeq(string(b), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		kb, err := strconv.ParseUint(strings.Fields(strings.TrimSpace(value))[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			avail = kb * 1024
		}
	}
	if total >= avail {
		used = total - avail
	}
	return total, used
}

// procCount counts the running processes by the numeric entries of /proc.
//
// /proc/stat's procs_running counts only the runnable ones, which on an idle
// machine reads 1 and looks like a fault.
func procCount() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
		}
	}
	return n
}

func loadAvg() [3]float64 {
	var out [3]float64
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out
	}
	for j, f := range strings.Fields(string(b)) {
		if j >= 3 {
			break
		}
		out[j], _ = strconv.ParseFloat(f, 64)
	}
	return out
}

// cpuBusy reads /proc/stat and compares with the previous reading.
//
// A cumulative counter since boot cannot give a percentage on its own: the
// first call stores it and returns nothing, and every call after that returns
// the busy fraction over the interval -- which is what a dashboard should show,
// and is why the sample is carried across calls by the Reader.
func cpuBusy(prev *cpuTimes) (busy float64, ok bool, now *cpuTimes) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false, nil
	}
	first, _, ok2 := strings.Cut(string(b), "\n")
	if !ok2 {
		return 0, false, nil
	}
	fields := strings.Fields(first)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, false, nil
	}
	var vals []uint64
	for _, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, false, nil
		}
		vals = append(vals, v)
	}
	var total uint64
	for _, v := range vals {
		total += v
	}
	// idle is the 4th field overall, iowait the 5th. iowait counts as idle for
	// this purpose: a machine waiting on a disk is not burning CPU.
	idle := vals[3]
	if len(vals) > 4 {
		idle += vals[4]
	}
	now = &cpuTimes{busy: total - idle, total: total}
	if prev == nil {
		return 0, false, now
	}
	dTotal := now.total - prev.total
	dBusy := now.busy - prev.busy
	if dTotal == 0 {
		return 0, false, now
	}
	return float64(dBusy) / float64(dTotal), true, now
}

func gateways() []string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	var out []string
	// /proc/net/route starts with a header line; drop it at the first newline.
	_, body, _ := strings.Cut(string(b), "\n")
	for line := range strings.SplitSeq(body, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		if ip := ipv4FromHex(f[2]); ip != "" {
			out = append(out, ip)
		}
	}
	return out
}

// ipv4FromHex converts the gateway field of /proc/net/route to dotted form.
//
// The kernel writes it as a 32-bit little-endian hexadecimal number, so the
// bytes come out in reverse: C0A80101 is 192.168.1.1 only if the octets are
// taken from the least significant end. Reading it as written yields 1.1.168.192.
func ipv4FromHex(h string) string {
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return ""
	}
	return strings.Join([]string{
		strconv.FormatUint(v&0xff, 10),
		strconv.FormatUint((v>>8)&0xff, 10),
		strconv.FormatUint((v>>16)&0xff, 10),
		strconv.FormatUint((v>>24)&0xff, 10),
	}, ".")
}

func dnsServers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			out = append(out, fields[1])
		}
	}
	return out
}

func ntpServers() []string {
	b, err := os.ReadFile("/etc/ntp.conf")
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "server" {
			out = append(out, fields[1])
			if len(out) >= 2 {
				break
			}
		}
	}
	return out
}

// NodeFromDropIn reads the node name and address vates-init wrote for the
// kubelet unit.
//
// The name is the machine's Kubernetes node name: it came from the config
// drive's meta-data, which is the same source Kubernetes registers the node
// under. The address is the DHCP lease taken at first boot, which is why it is
// only a fallback for the API's answer -- a lease can change without a reboot.
//
// Both are absent until the machine has configured itself, and empty values mean
// exactly that.
func NodeFromDropIn(path string) (name, ip string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Environment=NODE_NAME="); ok {
			name = strings.TrimSpace(rest)
		}
		if rest, ok := strings.CutPrefix(line, "Environment=NODE_IP="); ok {
			ip = strings.TrimSpace(rest)
		}
	}
	return name, ip
}

// KubernetesVersionFromDropIn reads the Kubernetes version vates-init wrote for
// the kubelet unit.
//
// This is the version THIS NODE runs, and it is the honest answer for a console:
// the image carries no version at all -- one image serves several -- so a file
// saying what the build was made for would describe something that no longer
// exists. Read from the same drop-in as the node's name, for the same reason.
func KubernetesVersionFromDropIn(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Environment=KUBERNETES_VERSION="); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// NodeNameFromDropIn is NodeFromDropIn when only the name is wanted.
func NodeNameFromDropIn(path string) string {
	name, _ := NodeFromDropIn(path)
	return name
}

// ResolveNodeName decides which node this machine is.
//
// The order is deliberate, and getting it wrong is not cosmetic: an overridden
// name wins; then the name vates-init wrote, which is what the cluster knows the
// node as; and only last the hostname, which is set by cloud init from the same
// meta-data but by a different program and may not have been set yet.
//
// Measured, because this was wrong: the console was starting before vates-init,
// reading the hostname -- which was still the name of the machine the image had
// been BUILT on -- and then asking the API about a node that did not exist. The
// screen read "waiting for the API server" for the life of the machine while the
// cluster was healthy. The caller must therefore ask again on every refresh,
// not once at startup.
func ResolveNodeName(override, dropInPath string) string {
	if override != "" {
		return override
	}
	if name := NodeNameFromDropIn(dropInPath); name != "" {
		return name
	}
	host, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return host
}
