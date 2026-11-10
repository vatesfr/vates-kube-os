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

func TestEventToneCalmsWhatIsExpectedWhileStarting(t *testing.T) {
	// The complaint this answers: an event that means "not yet" was drawn like a
	// failure, so the console looked crashed while the node was coming up. The
	// same event is a warning once the node is Ready -- an image pull that is
	// normal at boot is a problem on a node that has been up for hours.
	cases := []struct {
		name     string
		typ      string
		reason   string
		starting bool
		want     string
	}{
		{"probe while starting", "Warning", "Unhealthy", true, ToneDim},
		{"probe once ready", "Warning", "Unhealthy", false, ToneWarn},
		{"scheduling while starting", "Warning", "FailedScheduling", true, ToneDim},
		{"sandbox while starting", "Warning", "FailedCreatePodSandBox", true, ToneDim},
		{"disk capacity while starting", "Warning", "InvalidDiskCapacity", true, ToneDim},
		{"a real warning while starting", "Warning", "CrashLoopBackOff", true, ToneWarn},
		{"a real warning once ready", "Warning", "FailedScheduling", false, ToneWarn},
		{"an error is always an error", "Error", "EventsUnavailable", true, ToneBad},
		{"a normal event is normal", "Normal", "Started", true, ToneNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e kubeapi.Event
			e.Type = tc.typ
			e.Reason = tc.reason
			if got := EventTone(e, tc.starting); got != tc.want {
				t.Errorf("EventTone(%s/%s, starting=%v) = %q, want %q",
					tc.typ, tc.reason, tc.starting, got, tc.want)
			}
		})
	}
}

func TestStartingNeedsAKnownNode(t *testing.T) {
	// Before the kubeconfig exists the console has no node at all; that is
	// "unknown", not "starting", and it must not be what calms a warning.
	var s Snapshot
	if s.Starting() {
		t.Error("an empty snapshot claims to be starting")
	}
	s.Node.Metadata.Name = "vates-cp-1"
	if !s.Starting() {
		t.Error("a known, not-Ready node is starting")
	}
	s.Ready = true
	if s.Starting() {
		t.Error("a Ready node is not starting")
	}
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
	systemSeenOK.Store(false)
	t.Cleanup(func() { systemSeenOK.Store(false) })
	if v, tone := s.SystemState(); v != "Starting" || tone != ToneDim {
		t.Errorf("SYSTEM = %q (%s), want Starting/dim: a component is not up yet, the demo pod is not a component", v, tone)
	}

	// Before the first OK, a component that has already failed is Degraded.
	coredns := &s.Pods[1].Status.ContainerStatuses[0]
	coredns.RestartCount = 1
	if v, tone := s.SystemState(); v != "Degraded" || tone != ToneWarn {
		t.Errorf("restarted component: SYSTEM = %q (%s), want Degraded/warn", v, tone)
	}
	coredns.RestartCount = 0
	s.Pods = append(s.Pods, podsFromJSON(t, `{"items":[
	  {"metadata":{"namespace":"kube-flannel","name":"flannel"},
	   "status":{"containerStatuses":[{"name":"c","ready":false,
	     "state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}
	]}`)...)
	if v, _ := s.SystemState(); v != "Degraded" {
		t.Errorf("crash-looping component: SYSTEM = %q, want Degraded", v)
	}
	s.Pods = s.Pods[:3]

	coredns.Ready = true
	if v, tone := s.SystemState(); v != "OK" || tone != ToneGood {
		t.Errorf("SYSTEM = %q (%s), want OK/good", v, tone)
	}

	// Once OK has been seen, an unready component is a regression even with no
	// failure sign: a readiness probe can fail without a restart.
	coredns.Ready = false
	if v, tone := s.SystemState(); v != "Degraded" || tone != ToneWarn {
		t.Errorf("after OK: SYSTEM = %q (%s), want Degraded/warn", v, tone)
	}
}
