package console

import (
	"encoding/json"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/dashboard"
	"github.com/vatesfr/vates-kube-os/internal/hostinfo"
)

// The fabricated node, for looking at the graphical dashboard's design without
// a cluster: `vates-dashboard --gui --mock`.
//
// It is written as the API writes it and unmarshalled, rather than built as Go
// literals: the Kubernetes objects use anonymous structs with json tags, and an
// anonymous struct built by hand is a different type from the one declared next
// to it. Going through the wire format also cannot drift from the shape the API
// actually sends.
//
// The numbers are deliberately round and obviously illustrative. A mock that
// imitates one machine well enough to be mistaken for it stops being a mock.
const mockJSON = `{
  "Ready": false,
  "Node": {
    "metadata": {"name": "vates-cp-1", "labels": {"node-role.kubernetes.io/control-plane": ""}},
    "status": {
      "conditions": [{"type": "Ready", "status": "False", "reason": "KubeletNotReady"}],
      "addresses": [{"type": "InternalIP", "address": "192.168.122.10"}],
      "nodeInfo": {
        "kubeletVersion": "v1.31.0",
        "containerRuntimeVersion": "containerd://2.2.7",
        "osImage": "Vates Kube OS 0.1.0"
      }
    }
  },
  "Pods": [
    {"metadata": {"name": "etcd-vates-cp-1", "namespace": "kube-system"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "etcd", "ready": true, "restartCount": 0}]}},
    {"metadata": {"name": "kube-apiserver-vates-cp-1", "namespace": "kube-system"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "kube-apiserver", "ready": true, "restartCount": 0}]}},
    {"metadata": {"name": "kube-controller-manager-vates-cp-1", "namespace": "kube-system"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "kube-controller-manager", "ready": true, "restartCount": 0}]}},
    {"metadata": {"name": "kube-scheduler-vates-cp-1", "namespace": "kube-system"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "kube-scheduler", "ready": true, "restartCount": 0}]}},
    {"metadata": {"name": "kube-vip-vates-cp-1", "namespace": "kube-system"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "kube-vip", "ready": true, "restartCount": 0}]}},
    {"metadata": {"name": "kube-proxy-abcde", "namespace": "kube-system"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "kube-proxy", "ready": true, "restartCount": 0}]}},
    {"metadata": {"name": "kube-flannel-ds-xyz12", "namespace": "kube-flannel"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "kube-flannel", "ready": true, "restartCount": 0}]}},
    {"metadata": {"name": "coredns-6d4b75cb6d-9x2vt", "namespace": "kube-system"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "coredns", "ready": true, "restartCount": 9}]}},
    {"metadata": {"name": "coredns-6d4b75cb6d-8k4mn", "namespace": "kube-system"},
     "status": {"phase": "Running", "containerStatuses": [{"name": "coredns", "ready": true, "restartCount": 0}]}}
  ],
  "Events": [
    {"type": "Normal", "reason": "Scheduled", "message": "Successfully assigned kube-system/coredns to vates-cp-1"},
    {"type": "Normal", "reason": "Pulling", "message": "Pulling image \"registry.k8s.io/kube-proxy:v1.31.0\""},
    {"type": "Normal", "reason": "Pulled", "message": "Successfully pulled image in 1.9s"},
    {"type": "Normal", "reason": "Created", "message": "Created container kube-proxy"},
    {"type": "Normal", "reason": "Started", "message": "Started container kube-proxy"},
    {"type": "Warning", "reason": "FailedCreatePodSandBox", "message": "Failed to create pod sandbox: rpc error: code = Unknown desc = failed to setup network for sandbox"},
    {"type": "Normal", "reason": "SandboxChanged", "message": "Pod sandbox changed, it will be killed and re-created."},
    {"type": "Normal", "reason": "Pulled", "message": "Container image \"docker.io/flannel/flannel:v0.26.1\" already present on machine"},
    {"type": "Normal", "reason": "Created", "message": "Created container kube-flannel"},
    {"type": "Normal", "reason": "Started", "message": "Started container kube-flannel"},
    {"type": "Warning", "reason": "FailedCreatePodSandBox", "message": "Failed to create pod sandbox: rpc error: code = Unknown desc = failed to setup network for sandbox \"8b1d2c3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c\": plugin type=\"flannel\" failed (add): failed to delegate add: failed to set bridge addr: \"cni0\" already has an IP address different from 10.244.0.1/24"}
  ]
}`

// mockSnapshot returns the fabricated node, with enough events to overflow the
// panel: how the feed behaves -- following the newest, and stopping the moment
// the reader scrolls up -- cannot be judged in a list that fits.
//
// The node is NOT ready, and its state is the long form -- "NotReady
// (KubeletNotReady)", which is what a control plane shows while its kubelet is
// still coming up. That is the longest value the tiles ever carry, and the one
// that made the grid wider than a narrow console: a mock whose values are all
// short cannot show that, and a mock that cannot show the fault cannot catch
// it.
// mockSeen counts the fabricated events handed out since the process started,
// and mockBase is the instant they are counted from.
//
// The base is fixed at the first call on purpose. This used to stamp every event
// from "now" on every refresh: the whole feed then had new keys twice a second,
// the display's anchoring had nothing to hold on to, and a reader who had
// scrolled up watched lines change under them -- a mock that LOOKS like the bug
// it is supposed to help find. With a fixed base, an event keeps its identity
// from one refresh to the next, exactly as a real one does: the window slides by
// one, and what was under the reader stays there.
var (
	mockSeen int
	mockBase time.Time
)

func mockSnapshot() dashboard.Snapshot {
	var s dashboard.Snapshot
	if err := json.Unmarshal([]byte(mockJSON), &s); err != nil {
		// mockJSON is a constant in this file: an error here is a programming
		// mistake, not a runtime condition to report and recover from.
		panic("vates-dashboard: the mock is not valid: " + err.Error())
	}
	if mockBase.IsZero() {
		mockBase = time.Now()
	}
	base := s.Events
	mockSeen++
	s.Events = nil
	for i := range 60 {
		// The window is the last sixty of an ever-growing sequence. Each event's
		// stamp is fixed once, from its index -- so a refresh adds one at the
		// bottom and drops one at the top, and the sixty in between do not move.
		seen := mockSeen - 60 + i
		e := base[((seen%len(base))+len(base))%len(base)]
		e.Count = seen
		e.LastTimestamp = mockBase.Add(time.Duration(seen) * 7 * time.Second).UTC().Format(time.RFC3339)
		s.Events = append(s.Events, e)
	}
	return s
}

// MockSnapshot and MockHost expose the fabricated node to the preview renderer
// (cmd/ggpreview), so that the picture it writes is the one --gui --mock shows
// and not a second fabrication free to drift from it.
func MockSnapshot() dashboard.Snapshot { return mockSnapshot() }

// MockHost fills a host reading with the fabricated machine's numbers.
func MockHost(info hostinfo.Info) hostinfo.Info { return mockHost(info) }

// mockHost returns readings for the fabricated machine: three gigabytes and two
// cores, so that the notes beside the percentages have something to say.
func mockHost(info hostinfo.Info) hostinfo.Info {
	info.Uptime = 32 * time.Minute
	info.CPUBusy = 0.021
	info.HasCPU = true
	info.CPUs = 2
	info.MemTotal = 3 << 30
	info.MemUsed = uint64(3<<30) * 41 / 100
	info.Load = [3]float64{0.03, 0.07, 0.08}
	info.OS = "AlmaLinux 10.0"
	info.Distribution = "kubeadm"
	return info
}
