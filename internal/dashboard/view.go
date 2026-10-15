// view.go holds what the console SHOWS, as opposed to what it collects.
//
// It is here, next to the collection, because there is more than one renderer:
// the text console and the drawing one. Each of them decides
// how big a thing is and where it goes -- that is rendering -- but none of them
// should decide what a tile SAYS, what colour a warning gets, or how an event is
// turned into a line. Two renderers with their own copy of that would drift, and
// the drift would be discovered on a machine nobody can look at.

package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/hostinfo"
	"github.com/vatesfr/vates-kube-os/internal/kubeapi"
)

// TileLabels is the tile list, in the order someone standing at the machine
// reads it: what this is, what it is doing, and where it is.
var TileLabels = []string{
	"ROLE", "SYSTEM", "ADDRESS", "KUBERNETES", "CONTAINERD",
	"PODS", "RESTARTS", "CPU", "RAM", "LOAD",
}

// Tones. A renderer maps these to its own colours; nothing else in the program
// should care what "warn" looks like.
const (
	ToneNone = ""
	ToneGood = "good"
	ToneWarn = "warn"
	ToneBad  = "bad"
	ToneDim  = "dim"
)

// Tile is one tile's content: a value, a note beside it when there is one, and
// the tone that colours it.
type Tile struct {
	Value string
	Note  string
	Tone  string
}

// Tiles is the whole tile column for a snapshot.
//
// When the node is not known yet -- the console starts before the kubeconfig
// exists, which is every boot -- every tile says "-" rather than zero. A zero is
// a reading, and at that moment there is no reading.
func (s Snapshot) Tiles(h hostinfo.Info) []Tile {
	if s.Node.Metadata.Name == "" {
		empty := make([]Tile, len(TileLabels))
		for i := range empty {
			empty[i] = Tile{Value: "-"}
		}
		return empty
	}
	node := s.Node

	pods, restarts := 0, 0
	for _, p := range s.Pods {
		pods++
		_, _, r := p.Ready()
		restarts += r
	}
	restartTone := ToneNone
	if restarts > 0 {
		restartTone = ToneWarn
	}

	role := node.Role()
	if role == "" {
		role = "worker"
	}

	// CPUPercent is -1 until a second sample has been taken, which is a quarter
	// of a second after the process starts. Saying "n/a" for that quarter second
	// is honest; saying 0 % is not. The count beside it because a percentage on
	// its own does not say what it is a percentage OF.
	cpu := "n/a"
	if v := h.CPUPercent(); v >= 0 {
		cpu = fmt.Sprintf("%.1f%%", v)
	}

	system, systemTone := s.SystemState()
	return []Tile{
		{Value: role},
		{Value: system, Tone: systemTone},
		{Value: node.Address("InternalIP")},
		{Value: node.Status.NodeInfo.KubeletVersion},
		{Value: strings.TrimPrefix(node.Status.NodeInfo.ContainerRuntimeVersion, "containerd://")},
		{Value: fmt.Sprint(pods)},
		{Value: fmt.Sprint(restarts), Tone: restartTone},
		{Value: cpu, Note: fmt.Sprintf("%d CPU", h.CPUs)},
		{Value: fmt.Sprintf("%.0f%%", h.MemPercent()), Note: HumanBytes(h.MemTotal)},
		{Value: fmt.Sprintf("%.2f %.2f %.2f", h.Load[0], h.Load[1], h.Load[2]), Tone: ToneDim},
	}
}

// SystemState is the state of the cluster's own components ON THIS NODE: OK
// when every system pod there is ready, Degraded when one is not.
//
// It is the component level, which the node's Ready condition (the header pill)
// knows nothing about: a node is Ready while Cilium, CoreDNS or the control
// plane are still coming up, and this is the difference. It is a word, not a
// count: the number ready over the number there says little without naming what
// is counted, and what matters is whether anything is wrong.
//
// The only "cluster" a node's console can see is this one -- the kubelet
// identity reads its own node and the pods bound to it, never cluster-wide.
func (s Snapshot) SystemState() (value, tone string) {
	ready, total := 0, 0
	for _, p := range s.Pods {
		if !systemNamespace(p.Metadata.Namespace) {
			continue
		}
		r, t, _ := p.Ready()
		total++
		if t > 0 && r == t {
			ready++
		}
	}
	if total == 0 {
		// No snapshot yet, or no component on this node. Not "OK": there is no
		// reading.
		return "-", ToneNone
	}
	if ready == total {
		return "OK", ToneGood
	}
	return "Degraded", ToneWarn
}

// systemNamespace reports whether a namespace holds the cluster's components
// rather than a workload. Kubernetes has no marker for this, so it is the rule
// the cluster follows: its own things live under kube-* -- kube-system (the
// control plane, CoreDNS, kube-proxy, and Cilium) and kube-flannel. A CNI that
// keeps its own namespace instead (Calico's calico-system) would not be counted;
// nothing general exists to count it by.
func systemNamespace(ns string) bool {
	return strings.HasPrefix(ns, "kube-")
}

// --- events ------------------------------------------------------------------

// EventKey identifies an event, for a feed that must tell "the same event seen
// again" from "the same event repeated later". The count is deliberately not
// part of it: nothing on screen shows it, so it cannot distinguish two lines.
func EventKey(e kubeapi.Event) string {
	return strings.Join([]string{
		e.When().Format(time.RFC3339),
		e.Type,
		e.Reason,
		e.Message,
	}, "|")
}

// BootEvent reports whether an event is one a node emits on its way up, and is
// therefore drawn calm rather than as a warning.
//
// The reason matters, but so does WHEN: a node that is not Ready is still in its
// boot window and every expected reason is calm; once it is Ready, an expected
// reason is calm only if it happened BEFORE the node became Ready. Without that
// boundary the boot's own events -- the kubelet's first disk stat, a probe
// retrying before the API answered -- kept their "not yet" reason but took the
// colour of a live warning the moment the node turned Ready, and the console
// looked broken seconds after it had finished starting.
//
// A node the console has not reached yet is not Ready either, so an expected
// reason seen before the first collection is a boot event too. That is the
// common case at the very start of a boot, and it is the one the previous
// "known and not Ready" test drew as a warning.
func (s Snapshot) BootEvent(e kubeapi.Event) bool {
	if !expectedWhileStarting[e.Reason] {
		return false
	}
	if !s.Ready {
		return true
	}
	if s.ReadyAt.IsZero() {
		return false
	}
	return !e.When().After(s.ReadyAt)
}

// expectedWhileStarting are the event reasons a node emits on its way up, and
// which mean "not yet" rather than "broken". They are drawn calm while the node
// is coming up and for the events that predate it becoming Ready; the same
// reason after that is a warning, because the image pull that is normal at boot
// is a problem on a node that has been up for hours (see Snapshot.BootEvent).
//
// Every reason here was seen on a real boot: `Unhealthy` is a probe retrying
// (kube-vip's, before the API server answers), `FailedCreatePodSandBox` and
// `FailedScheduling` are the CNI and the scheduler waiting for each other, and
// `InvalidDiskCapacity` is the kubelet's first filesystem stat, before cAdvisor
// has a reading.
var expectedWhileStarting = map[string]bool{
	"Unhealthy":              true,
	"FailedScheduling":       true,
	"FailedCreatePodSandBox": true,
	"InvalidDiskCapacity":    true,
	"BackOff":                true,
	"FailedMount":            true,
	"FailedAttachVolume":     true,
}

// EventTone is the tone an event is drawn with, from its type and whether it is
// an expected boot event (Snapshot.BootEvent). It is the one place that decides
// it, so the two renderers agree.
func EventTone(e kubeapi.Event, boot bool) string {
	switch e.Type {
	case eventTypeError:
		return ToneBad
	case "Warning":
		if boot && expectedWhileStarting[e.Reason] {
			return ToneDim
		}
		return ToneWarn
	default:
		return ToneNone
	}
}

// Timestamp is the time an event is stamped with, at the width a feed shows.
func Timestamp(e kubeapi.Event) string {
	return e.When().Format("15:04:05")
}

// EventMarkup is one event as markup: the time, the reason in a fixed
// column, and the message. The colour states the severity, and the message is
// what carries the prose -- it is the only part a feed lets wrap.
//
// There is no trailing newline: a feed that lays out one layout per event has no
// use for one, and the text view adds it.
func EventMarkup(e kubeapi.Event, boot bool) string {
	reasonColour := eventReasonColour[EventTone(e, boot)]
	if reasonColour == "" {
		reasonColour = eventReasonColour[ToneNone]
	}
	return Span("#8b93a7", Timestamp(e)) + "  " +
		Span(reasonColour, fmt.Sprintf("%-20s", e.Reason)) + " " +
		Span("#fffce4", e.Message)
}

// eventReasonColour maps a tone to the colour of the reason column. The dim one
// is the same grey as the timestamp: an expected boot warning should read as
// background, next to the line that says what the node is actually waiting for.
var eventReasonColour = map[string]string{
	ToneNone: "#a78bfa",
	ToneDim:  "#8b93a7",
	ToneWarn: "#f08019",
	ToneBad:  "#f87171",
}

// Span is a coloured run of text, escaped so that an event message containing an
// ampersand or an angle bracket stays data rather than becoming markup.
func Span(colour, s string) string {
	return `<span foreground="` + colour + `">` + MarkupEscape(s) + `</span>`
}

// MarkupEscape makes arbitrary text safe to put inside markup.
func MarkupEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// HumanBytes is a byte count the way a person reads it. Zero means the machine
// did not say, and is shown as nothing rather than as "0 GiB" -- a zero there
// reads as "this machine has no memory".
func HumanBytes(n uint64) string {
	switch {
	case n == 0:
		return ""
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	default:
		return fmt.Sprintf("%.0f MiB", float64(n)/float64(1<<20))
	}
}
