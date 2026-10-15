package dashboard

import (
	"fmt"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/kubeapi"
)

// Snapshot is one observation of the node.
type Snapshot struct {
	// ReadyAt is when the node last became Ready, and is zero when it never
	// has (or the API did not say). It is the boundary the event feed uses to
	// tell a boot event from one on a node that is up -- see BootEvent.
	ReadyAt     time.Time
	ReadyReason string
	Node        kubeapi.Node
	Pods        []kubeapi.Pod
	Events      []kubeapi.Event

	// Ready and ReadyReason are pulled out because every screen shows them and
	// the condition list is awkward to read from a template.
	Ready bool
}

// MaxEvents bounds how many events are kept. The dashboard shows a tail; the
// API is asked for a bounded number so a busy cluster does not hand back a
// megabyte of history every two seconds.
const MaxEvents = 400

// Collect reads the node, its pods and its recent events.
//
// The node name comes from the caller, not from the API: it is known locally
// and stable, and asking "which node am I" of the API would be a second way to
// get the answer that can disagree with the first.
func Collect(c *kubeapi.Client, nodeName string) (Snapshot, error) {
	var s Snapshot

	node, err := c.Node(nodeName)
	if err != nil {
		return s, fmt.Errorf("reading node %s: %w", nodeName, err)
	}
	s.Node = node
	s.Ready, s.ReadyReason = node.Ready()
	s.ReadyAt, _ = node.ReadySince()

	// The pods and the events are supplementary: losing either should cost the
	// dashboard a panel, not the whole screen. The node itself is what must be
	// shown, so only its failure is returned.
	if pods, err := c.PodsOn(nodeName); err == nil {
		s.Pods = pods
	}
	s.Events = collectEvents(c, nodeName, s.Pods)

	return s, nil
}

// eventTypeError is the API's event type for a failure. Named because two
// places use it -- the synthetic event this package builds when the feed is
// unavailable, and the renderer that classifies it -- and they must agree.
const eventTypeError = "Error"

// collectEvents returns the events that concern this node: its own, and those
// of the pods running on it.
//
// Watched rather than filtered server-side. A node's events name the node in
// involvedObject; a pod's event names the pod, not the node it happens to be on.
// Asking the API for involvedObject.name=<node> would therefore return the
// kubelet's own events and silently none of the pod events that operators
// actually read.
func collectEvents(c *kubeapi.Client, nodeName string, pods []kubeapi.Pod) []kubeapi.Event {
	all, err := c.Events(MaxEvents)
	if err != nil {
		// Nearly always "events is forbidden", which is exactly the case the
		// console should report plainly instead of showing an empty feed that
		// looks like a quiet cluster.
		return []kubeapi.Event{{Type: eventTypeError, Reason: "EventsUnavailable", Message: err.Error()}}
	}
	return FilterEvents(all, nodeName, pods)
}

// FilterEvents keeps the events that concern this node: its own, and those of
// the pods running on it.
//
// Separate from the fetch so that the rule -- which is the part worth asserting
// -- can be tested without an API server, and so that a single streamed event
// can be judged by the same rule as a list of four hundred. A watch has to ask
// "is this one mine?" one event at a time; if it answered with its own copy of
// the rule, the feed would show one set of events on a watched machine and
// another on a machine that fell back to polling.
func FilterEvents(all []kubeapi.Event, nodeName string, pods []kubeapi.Pod) []kubeapi.Event {
	podKeys := make(map[string]bool, len(pods))
	for _, p := range pods {
		podKeys[p.Metadata.Namespace+"/"+p.Metadata.Name] = true
	}

	var kept []kubeapi.Event
	for _, e := range all {
		involved := e.InvolvedObject
		if involved.Kind == "Node" && involved.Name == nodeName {
			kept = append(kept, e)
			continue
		}
		if involved.Kind == "Pod" && podKeys[involved.Namespace+"/"+involved.Name] {
			kept = append(kept, e)
		}
	}
	// Keep the tail: the display is a feed, and the newest is at the bottom.
	if len(kept) > MaxEvents {
		kept = kept[len(kept)-MaxEvents:]
	}
	return kept
}

// Summary is the one-line form, used by --once and by the tests.
func (s Snapshot) Summary() string {
	state := "Ready"
	if !s.Ready {
		state = "NotReady"
		if s.ReadyReason != "" {
			state = "NotReady (" + s.ReadyReason + ")"
		}
	}
	role := "worker"
	if s.Node.Role() != "" {
		role = s.Node.Role()
	}
	return fmt.Sprintf("%s  %s  %s  %s  %d pods  %d events",
		s.Node.Metadata.Name, role, state, s.Node.Address("InternalIP"), len(s.Pods), len(s.Events))
}
