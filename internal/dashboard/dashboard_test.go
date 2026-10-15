package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/kubeapi"
)

func event(kind, namespace, name, reason string) kubeapi.Event {
	var e kubeapi.Event
	e.InvolvedObject.Kind = kind
	e.InvolvedObject.Namespace = namespace
	e.InvolvedObject.Name = name
	e.Reason = reason
	return e
}

func pod(namespace, name string) kubeapi.Pod {
	var p kubeapi.Pod
	p.Metadata.Namespace = namespace
	p.Metadata.Name = name
	return p
}

func TestFilterEventsKeepsTheNodeAndItsPodsOnly(t *testing.T) {
	// The rule is what matters, and both halves of it are here: a node's own
	// events name the node, a pod's event names the pod and not the node it
	// happens to run on. Filtering server-side by involvedObject.name=<node>
	// would return the node's events and silently none of its pods' -- which is
	// the mistake this asserts against.
	all := []kubeapi.Event{
		event("Node", "", "vates-cp-1", "NodeReady"),
		event("Node", "", "vates-cp-2", "NodeReady"),
		event("Pod", "kube-system", "etcd-vates-cp-1", "Started"),
		event("Pod", "kube-system", "etcd-vates-cp-2", "Started"),
		event("ReplicaSet", "kube-system", "coredns-6f6b679f8f", "SuccessfulCreate"),
	}
	pods := []kubeapi.Pod{pod("kube-system", "etcd-vates-cp-1")}

	got := FilterEvents(all, "vates-cp-1", pods)

	if len(got) != 2 {
		t.Fatalf("filterEvents returned %d events, want 2: %v", len(got), reasons(got))
	}
	for _, e := range got {
		switch e.InvolvedObject.Kind {
		case "Node":
			if e.InvolvedObject.Name != "vates-cp-1" {
				t.Errorf("kept another node's event: %s", e.InvolvedObject.Name)
			}
		case "Pod":
			if e.InvolvedObject.Name != "etcd-vates-cp-1" {
				t.Errorf("kept a pod that is not on this node: %s", e.InvolvedObject.Name)
			}
		default:
			t.Errorf("kept an event of kind %s, which belongs to no node", e.InvolvedObject.Kind)
		}
	}
}

func TestFilterEventsMatchesPodsByNameAndNamespace(t *testing.T) {
	// Two pods of the same name in different namespaces are two pods. Matching
	// on the name alone would leak one namespace's events into another's.
	all := []kubeapi.Event{
		event("Pod", "team-a", "web-0", "Started"),
		event("Pod", "team-b", "web-0", "Started"),
	}
	pods := []kubeapi.Pod{pod("team-a", "web-0")}

	got := FilterEvents(all, "node", pods)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(got), reasons(got))
	}
	if got[0].InvolvedObject.Namespace != "team-a" {
		t.Errorf("kept namespace %q, want team-a", got[0].InvolvedObject.Namespace)
	}
}

func TestFilterEventsKeepsTheNewestWhenTrimming(t *testing.T) {
	// The feed shows the tail. Keeping the head instead would mean a busy
	// cluster displays only its oldest events.
	var all []kubeapi.Event
	for i := 0; i < MaxEvents+50; i++ {
		e := event("Node", "", "node-a", "")
		e.Message = string(rune('a' + i%26))
		all = append(all, e)
	}
	got := FilterEvents(all, "node-a", nil)
	if len(got) != MaxEvents {
		t.Fatalf("got %d events, want the cap %d", len(got), MaxEvents)
	}
	if got[len(got)-1].Message != all[len(all)-1].Message {
		t.Errorf("the last kept event is not the newest")
	}
}

func TestEventToneCalmsExpectedBootEvents(t *testing.T) {
	// The complaint this answers: an event that means "not yet" was drawn like a
	// failure, so the console looked crashed while the node was coming up. The
	// same event is a warning when it is NOT a boot event -- an image pull that
	// is normal at boot is a problem on a node that has been up for hours. Which
	// event is a boot event is Snapshot.BootEvent's call; this pins the colours.
	cases := []struct {
		name   string
		typ    string
		reason string
		want   string
		boot   bool
	}{
		{name: "probe while booting", typ: "Warning", reason: "Unhealthy", boot: true, want: ToneDim},
		{name: "probe once ready", typ: "Warning", reason: "Unhealthy", boot: false, want: ToneWarn},
		{name: "scheduling while booting", typ: "Warning", reason: "FailedScheduling", boot: true, want: ToneDim},
		{name: "sandbox while booting", typ: "Warning", reason: "FailedCreatePodSandBox", boot: true, want: ToneDim},
		{name: "disk capacity while booting", typ: "Warning", reason: "InvalidDiskCapacity", boot: true, want: ToneDim},
		{name: "a real warning while booting", typ: "Warning", reason: "CrashLoopBackOff", boot: true, want: ToneWarn},
		{name: "a real warning once ready", typ: "Warning", reason: "FailedScheduling", boot: false, want: ToneWarn},
		{name: "an error is always an error", typ: "Error", reason: "EventsUnavailable", boot: true, want: ToneBad},
		{name: "a normal event is normal", typ: "Normal", reason: "Started", boot: true, want: ToneNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e kubeapi.Event
			e.Type = tc.typ
			e.Reason = tc.reason
			if got := EventTone(e, tc.boot); got != tc.want {
				t.Errorf("EventTone(%s/%s, boot=%v) = %q, want %q",
					tc.typ, tc.reason, tc.boot, got, tc.want)
			}
		})
	}
}

func TestBootEventCalmsOnlyTheBootsOwnEvents(t *testing.T) {
	// The second half of the complaint: the boot's own events were drawn orange
	// the instant the node turned Ready, because "starting" became false. A boot
	// reason is a boot event only until the node becomes Ready; after that it is
	// a warning again, unless it happened before the transition.
	readyAt := time.Date(2026, 10, 15, 10, 31, 0, 0, time.UTC)
	before := kubeapi.Event{Type: "Warning", Reason: "InvalidDiskCapacity"}
	before.LastTimestamp = readyAt.Add(-30 * time.Second).Format(time.RFC3339)
	after := kubeapi.Event{Type: "Warning", Reason: "Unhealthy"}
	after.LastTimestamp = readyAt.Add(30 * time.Second).Format(time.RFC3339)
	real := kubeapi.Event{Type: "Warning", Reason: "CrashLoopBackOff"}
	real.LastTimestamp = readyAt.Add(-30 * time.Second).Format(time.RFC3339)

	cases := []struct {
		name string
		snap Snapshot
		e    kubeapi.Event
		want bool
	}{
		{"unknown node is still booting", Snapshot{}, before, true},
		{"not ready yet", Snapshot{Node: nodeNamed("vates-cp-1")}, before, true},
		{"boot reason before Ready", Snapshot{Ready: true, ReadyAt: readyAt}, before, true},
		{"boot reason after Ready", Snapshot{Ready: true, ReadyAt: readyAt}, after, false},
		{"a real reason is never a boot event", Snapshot{Ready: true, ReadyAt: readyAt}, real, false},
		{"Ready without a transition time", Snapshot{Ready: true}, before, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.BootEvent(tc.e); got != tc.want {
				t.Errorf("BootEvent(%s) = %v, want %v", tc.e.Reason, got, tc.want)
			}
		})
	}
}

func nodeNamed(name string) kubeapi.Node {
	var n kubeapi.Node
	n.Metadata.Name = name
	return n
}

func TestPhaseParsesAndRenders(t *testing.T) {
	// The file is one line written by PID 1: "<text>|<unix start>". The console
	// shows the text and how long it has been going, so an unresolved wait is
	// legible instead of a frozen screen.
	got, ok := parsePhase([]byte("waiting for the API server|1000\n"))
	if !ok {
		t.Fatal("a phase line was not parsed")
	}
	if got.Text != "waiting for the API server" {
		t.Errorf("text = %q", got.Text)
	}
	if got.Since.Unix() != 1000 {
		t.Errorf("since = %d, want 1000", got.Since.Unix())
	}
	if line := got.Line(time.Unix(1012, 0)); line != "waiting for the API server (12s)" {
		t.Errorf("line = %q", line)
	}
}

func TestPhaseHidesWhatIsNotAWait(t *testing.T) {
	// "ready" ends the wait, an empty file is a machine that is not publishing
	// one, and neither is an activity line.
	for _, b := range []string{"ready|1000\n", "\n", ""} {
		if _, ok := parsePhase([]byte(b)); ok {
			t.Errorf("parsePhase(%q) reported an activity", b)
		}
	}
	if line := (Phase{}).Line(time.Unix(0, 0)); line != "" {
		t.Errorf("an empty phase rendered %q", line)
	}
}

func reasons(events []kubeapi.Event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.InvolvedObject.Kind+"/"+e.InvolvedObject.Namespace+"/"+e.InvolvedObject.Name)
	}
	return strings.Join(out, ", ")
}

// podsFromJSON decodes pods from a document, so a test can write a container
// status -- which the anonymous struct inside kubeapi.Pod cannot be built from
// without repeating its shape.
func podsFromJSON(t *testing.T, doc string) []kubeapi.Pod {
	t.Helper()
	var list struct {
		Items []kubeapi.Pod `json:"items"`
	}
	if err := json.Unmarshal([]byte(doc), &list); err != nil {
		t.Fatalf("unmarshal pods: %v", err)
	}
	return list.Items
}

func TestSystemStateTellsTheComponentsFromTheNode(t *testing.T) {
	// The SYSTEM tile is the component level on this node -- the level the
	// node's own Ready condition (the header pill) knows nothing about. It is a
	// word, not a count, and a user's workload is not a component.
	if TileLabels[1] != "SYSTEM" {
		t.Errorf("tile 1 is %q, want SYSTEM (STATE repeated the node's Ready condition)", TileLabels[1])
	}

	var s Snapshot
	if v, _ := s.SystemState(); v != "-" {
		t.Errorf("with no pods, SYSTEM = %q, want -", v)
	}

	s.Pods = podsFromJSON(t, `{"items":[
	  {"metadata":{"namespace":"kube-system","name":"cilium"},
	   "status":{"containerStatuses":[{"name":"c","ready":true}]}},
	  {"metadata":{"namespace":"kube-system","name":"coredns"},
	   "status":{"containerStatuses":[{"name":"c","ready":false}]}},
	  {"metadata":{"namespace":"demo","name":"hello"},
	   "status":{"containerStatuses":[{"name":"c","ready":false}]}}
	]}`)
	if v, tone := s.SystemState(); v != "Degraded" || tone != ToneWarn {
		t.Errorf("SYSTEM = %q (%s), want Degraded/warn: a component is not ready, the demo pod is not a component", v, tone)
	}

	s.Pods[1].Status.ContainerStatuses[0].Ready = true
	if v, tone := s.SystemState(); v != "OK" || tone != ToneGood {
		t.Errorf("SYSTEM = %q (%s), want OK/good", v, tone)
	}
}
