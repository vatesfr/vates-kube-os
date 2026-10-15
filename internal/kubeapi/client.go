package kubeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Node is the part of a Node object the dashboard reads.
type Node struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
		Name   string            `json:"name"`
	} `json:"metadata"`
	Status struct {
		NodeInfo struct {
			KubeletVersion          string `json:"kubeletVersion"`
			OSImage                 string `json:"osImage"`
			KernelVersion           string `json:"kernelVersion"`
			ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
		} `json:"nodeInfo"`
		Conditions []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			Reason             string `json:"reason"`
			Message            string `json:"message"`
			LastTransitionTime string `json:"lastTransitionTime"`
		} `json:"conditions"`
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
	} `json:"status"`
	Spec struct {
		Taints []struct {
			Key    string `json:"key"`
			Effect string `json:"effect"`
		} `json:"taints"`
	} `json:"spec"`
}

// Role reports the node's control-plane role, read from the label Kubernetes
// itself uses. The label's presence is what matters, not its value, which is
// the empty string.
func (n Node) Role() string {
	if _, ok := n.Metadata.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return "control-plane"
	}
	if _, ok := n.Metadata.Labels["node-role.kubernetes.io/master"]; ok {
		return "master"
	}
	return "worker"
}

// Ready reports the node's readiness and, when it is not ready, the reason the
// kubelet gave, which is the useful half of the answer.
func (n Node) Ready() (bool, string) {
	for _, c := range n.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		if c.Status == "True" {
			return true, ""
		}
		if c.Reason != "" {
			return false, c.Reason
		}
		return false, c.Message
	}
	return false, "no Ready condition"
}

// ReadySince returns when the node last became Ready, and whether it is Ready
// at all. It is the boundary between the events a node emits on its way up and
// the ones it emits once it is running: the dashboard keeps an expected boot
// reason calm only while it is still in that window, and draws it as a warning
// once the node has been up (see dashboard.BootEvent).
//
// The time comes from the Ready condition's lastTransitionTime, the moment the
// status last CHANGED -- not the node's creation time, which would be the same
// on every boot and would call a genuine warning on a long-lived node "old".
func (n Node) ReadySince() (time.Time, bool) {
	for _, c := range n.Status.Conditions {
		if c.Type != "Ready" || c.Status != "True" {
			continue
		}
		t, err := time.Parse(time.RFC3339, c.LastTransitionTime)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	}
	return time.Time{}, false
}

// Address returns the address of the given type, preferring the requested type
// and falling back to whatever exists, so the dashboard shows something rather
// than an empty block.
func (n Node) Address(kind string) string {
	for _, a := range n.Status.Addresses {
		if a.Type == kind {
			return a.Address
		}
	}
	if len(n.Status.Addresses) > 0 {
		return n.Status.Addresses[0].Address
	}
	return ""
}

// Taints returns the taints as "key" or "key=value:Effect" strings.
func (n Node) Taints() []string {
	var out []string
	for _, t := range n.Spec.Taints {
		out = append(out, t.Key+":"+t.Effect)
	}
	return out
}

// Pod is the part of a Pod object the dashboard reads.
type Pod struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		UID       string `json:"uid"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name         string `json:"name"`
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
		} `json:"containerStatuses"`
		InitContainerStatuses []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		} `json:"initContainerStatuses"`
	} `json:"status"`
}

// Ready reports how many containers of the pod are ready, and how many there
// are, and the total restart count. A pod can be Running while one of its
// containers is not ready, which is the difference the dashboard should show.
//
// Init containers and app containers are counted separately rather than
// appended together: they have different types (init containers have no
// restartCount) and merging them is what a first attempt at this did, and did
// not compile.
func (p Pod) Ready() (ready, total, restarts int) {
	for _, c := range p.Status.InitContainerStatuses {
		total++
		if c.Ready {
			ready++
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		total++
		if c.Ready {
			ready++
		}
		restarts += c.RestartCount
	}
	return ready, total, restarts
}

// Event is the part of an Event object the dashboard reads.
//
// The core v1 Event is used rather than events.k8s.io/v1: it is what `kubectl
// get events` reads, it is what the kubelet and the scheduler write, and it is
// the one whose field selectors are documented.
type Event struct {
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"involvedObject"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Type           string `json:"type"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	FirstTimestamp string `json:"firstTimestamp"`
	LastTimestamp  string `json:"lastTimestamp"`
	EventTime      string `json:"eventTime"`
	Count          int    `json:"count"`
}

// When returns the best timestamp an Event has.
//
// Both forms are present in a live cluster and neither is always set: the
// newer events carry eventTime, the older and the kubelet's carry
// lastTimestamp. Reading only one leaves half the events with no time at all.
func (e Event) When() time.Time {
	for _, s := range []string{e.EventTime, e.LastTimestamp, e.FirstTimestamp} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// get performs a GET and decodes the JSON into out.
func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(c.Server, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() // the body is fully read below; a failed Close adds nothing
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// The API's own message is the useful part -- "forbidden", "not found"
		// -- and dropping it would leave only the number.
		var status struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &status)
		if status.Message != "" {
			return fmt.Errorf("%s: %s", resp.Status, status.Message)
		}
		return fmt.Errorf("%s for %s", resp.Status, path)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

// cacheable marks a read as satisfied from the API server's watch cache.
//
// The console polls these reads every couple of seconds on EVERY node. A read
// with no resourceVersion is a QUORUM read: the API server forwards it to etcd
// and waits for a linearizable answer, so a fleet of consoles turns into a
// steady stream of etcd round-trips -- which is exactly what starves a control
// plane whose etcd sits on a slow disk. resourceVersion=0 asks for "any
// version" instead: the answer comes from the watch cache, no etcd round-trip.
//
// The console shows a snapshot, so a few hundred milliseconds of staleness is
// invisible and worth not hammering etcd for.
func cacheable(q url.Values) url.Values {
	q.Set("resourceVersion", "0")
	return q
}

// Node reads one Node by name.
func (c *Client) Node(name string) (Node, error) {
	var n Node
	err := c.get("/api/v1/nodes/"+url.PathEscape(name)+"?"+cacheable(url.Values{}).Encode(), &n)
	return n, err
}

// PodsOn returns the pods scheduled on the given node.
func (c *Client) PodsOn(node string) ([]Pod, error) {
	var list struct {
		Items []Pod `json:"items"`
	}
	q := url.Values{}
	q.Set("fieldSelector", "spec.nodeName="+node)
	if err := c.get("/api/v1/pods?"+cacheable(q).Encode(), &list); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		a, b := list.Items[i], list.Items[j]
		if a.Metadata.Namespace != b.Metadata.Namespace {
			return a.Metadata.Namespace < b.Metadata.Namespace
		}
		return a.Metadata.Name < b.Metadata.Name
	})
	return list.Items, nil
}

// Events returns recent events, newest last.
//
// All events are fetched, not only this node's. A node's own events -- the
// kubelet's heartbeats, image pulls, disk pressure -- are only part of what an
// operator wants to see; pod events carry the namespace and the pod name in
// involvedObject instead. Filtering server-side by involvedObject.name would
// therefore show the node's events and none of its pods'. The caller filters to
// what it knows belongs here.
func (c *Client) Events(limit int) ([]Event, error) {
	var list struct {
		Items []Event `json:"items"`
	}
	if err := c.get("/api/v1/events?"+cacheable(url.Values{}).Encode()+"&limit="+fmt.Sprint(limit), &list); err != nil {
		return nil, err
	}
	// The API returns events in no useful order for display; sort by time so the
	// caller can take the tail and have it be the most recent.
	sort.SliceStable(list.Items, func(i, j int) bool {
		return list.Items[i].When().Before(list.Items[j].When())
	})
	return list.Items, nil
}

// WatchEvents streams events as they arrive, calling fn for each new or changed
// one. It returns when the stream ends or ctx is cancelled.
//
// A Kubernetes watch is a STREAM, not a subscription with a promise: the
// connection ends -- the API's own timeoutSeconds, a proxy, a restart -- and
// what happened while it was down is never resent. This function therefore does
// not pretend to be complete: the caller is expected to keep listing as well,
// and it is the list that closes the gap. That is why the console's two-second
// poll is not redundant with this, and why neither one is allowed to be the only
// path.
//
// A watch is where the kubelet credential is most likely to be refused: the
// system:node role grants what a node needs to run its own pods, and watching
// cluster-wide events may not be part of it. A refusal is an ordinary error
// here, not a panic, and the caller falls back to the list.
func (c *Client) WatchEvents(ctx context.Context, fn func(Event)) error {
	q := url.Values{}
	q.Set("watch", "true")
	q.Set("allowWatchBookmarks", "true")
	// The server closes the stream itself after this, which is the healthy way
	// for it to end: a connection that lives for ever is one nobody can restart.
	q.Set("timeoutSeconds", "300")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.Server, "/")+"/api/v1/events?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	// A client of its own, WITHOUT the ten-second timeout the polling client
	// carries: a stream that is meant to stay open would otherwise be cut every
	// ten seconds and reconnect for ever. The transport is shared, so the
	// credentials and the certificate authority are the same.
	stream := &http.Client{Transport: c.http.Transport}
	resp, err := stream.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() // the body is fully read below; a failed Close adds nothing
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var status struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &status)
		if status.Message != "" {
			return fmt.Errorf("%s: %s", resp.Status, status.Message)
		}
		return fmt.Errorf("%s for the event watch", resp.Status)
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var frame struct {
			Type   string `json:"type"`
			Object Event  `json:"object"`
		}
		if err := dec.Decode(&frame); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		switch frame.Type {
		case "ADDED", "MODIFIED":
			fn(frame.Object)
		case "ERROR":
			return fmt.Errorf("the event watch reported an error: %s %s",
				frame.Object.Reason, frame.Object.Message)
		case "BOOKMARK":
			// A bookmark carries a resourceVersion and nothing else. This reader
			// keeps no resourceVersion -- the periodic list is what re-syncs --
			// so there is nothing to do with it.
		}
	}
}
