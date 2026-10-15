package console

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vatesfr/vates-kube-os/internal/dashboard"
	"github.com/vatesfr/vates-kube-os/internal/display/gui"
	"github.com/vatesfr/vates-kube-os/internal/display/tui"
	"github.com/vatesfr/vates-kube-os/internal/firstboot"
	"github.com/vatesfr/vates-kube-os/internal/hostinfo"
	"github.com/vatesfr/vates-kube-os/internal/kubeapi"
)

// Dashboard is the machine's console dashboard.
//
// It is what is shown on the physical console (tty1) in place of a login
// prompt: the node's name, role and state, its pods, and its events as they
// arrive. The machine has no Kubernetes binaries on the host, so this talks to
// the API server directly with the credentials the kubelet already has.
//
// --once prints a single snapshot as text and exits. That is how the collection
// is tested without a terminal -- over ssh, from a script -- and it is the same
// code path the interactive display uses.
//
// -g (or --gui) draws the same collection as a picture, straight onto the
// screen, instead of drawing it as text. It is the same binary and the same
// collector: the console is one program with two faces.
//
// The picture is drawn with fogleman/gg into an image of its own, blitted into
// the screen's buffer: no toolkit and no compositor, and what it draws lives in
// internal/display/gui and internal/dashboard.
const (
	// The kubeconfig of the kubelet, which exists on every node -- control
	// plane and worker alike -- and authenticates as system:node:<name>. Using
	// super-admin.conf would work on a control plane and not exist on a worker,
	// which is exactly the kind of difference that produces a dashboard that
	// works on half the cluster.
	defaultKubeconfig = "/etc/kubernetes/kubelet.conf"

	// dashboardVersion is printed by --version. The image build runs that as a
	// hard gate: a binary built for the wrong architecture, or one that cannot
	// start at all, would otherwise be discovered only on the machine's console.
	dashboardVersion = "0.1.0"
)

func Dashboard(argv []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	once := fs.Bool("once", false, "print one snapshot and exit")
	asJSON := fs.Bool("json", false, "with --once, print the snapshot as JSON")
	node := fs.String("node", "", "node name (default: this machine's hostname)")
	kubeconfig := fs.String("kubeconfig", defaultKubeconfig, "kubeconfig to authenticate with")
	showVer := fs.Bool("version", false, "print the version and exit")
	mock := fs.Bool("mock", false, "with -g, draw a fabricated node instead of collecting")
	interval := fs.Duration("interval", 0, "with -g, how often to redraw (default 2s)")
	// Two names for one switch: -g is what a person types on a console, --gui is
	// what a unit file or a script reads. The flag package accepts either as
	// -x or --x, so both spellings work on both names.
	var guiMode bool
	fs.BoolVar(&guiMode, "g", false, "draw the graphical dashboard instead of the text one")
	fs.BoolVar(&guiMode, "gui", false, "same as -g")
	drmDevice := fs.String("drm-device", "", "with -g, which card to take (default /dev/dri/card0)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	if *showVer {
		fmt.Printf("vates-dashboard %s\n", dashboardVersion)
		return nil
	}

	// The name is resolved on every refresh, not once here: the console starts
	// before vates-init has written the name anywhere, and a value captured at
	// that moment is the image's build hostname, not the node's.
	resolveName := func() string { return hostinfo.ResolveNodeName(*node, firstboot.KubeletDropIn) }

	// The graphical face. Same binary, same collector, a different renderer --
	// and no subprocess in between: the two cannot disagree about what they
	// show.
	if guiMode {
		reader := &hostinfo.Reader{}
		reader.Read() // a CPU percentage needs two samples, a quarter second apart

		// The client is built LATE -- the console starts with the machine, before
		// vates-init has written the kubeconfig, and a client that had to exist at
		// startup would leave the screen dead for the first seconds of every boot
		// -- and then kept.
		//
		// Building it on every refresh, which is what this did, gives a new
		// connection pool every two seconds and abandons the previous one. The
		// sockets are not closed with it: they are left to the server's idle
		// timeout, so how many there are is a race between how fast they are made
		// and how fast the server reaps them. Measured on a node: 65 sockets at
		// two seconds a turn, where the server keeps up, and 435 at 200 ms, where
		// it does not. The count is bounded either way -- and this is not what
		// freezes a screen -- but three sockets is the honest number for three
		// GETs per refresh, and one pool is the honest number of pools.
		//
		// Kept, not re-read: a credential rotated during the session would need a
		// restart, which a node's console gets at every boot anyway.
		//
		// Kept ONCE BUILT, but a FAILURE is not cached. The first attempt can
		// lose a race: on a node that JOINS, /etc/kubernetes/kubelet.conf is
		// written by the kubelet's own TLS bootstrap, after the console has
		// started, where on the bootstrapping node kubeadm writes it during
		// configure -- before the console. A cached "no such file" then leaves
		// the screen waiting for ever. Measured on vates-cp-2 and vates-cp-3,
		// Ready and serving, with consoles stuck on
		//   open /etc/kubernetes/kubelet.conf: no such file or directory
		// The mutex still gives one client, shared by the collection and the
		// event watch; it just does not make the first error permanent.
		var (
			clientMu sync.Mutex
			client   *kubeapi.Client
		)
		getClient := func() (*kubeapi.Client, error) {
			clientMu.Lock()
			defer clientMu.Unlock()
			if client != nil {
				return client, nil
			}
			c, err := kubeapi.NewFromKubeconfig(*kubeconfig)
			if err != nil {
				return nil, err
			}
			client = c
			return client, nil
		}

		collect := func() (dashboard.Snapshot, hostinfo.Info, error) {
			info := reader.Read()
			if *mock {
				return mockSnapshot(), mockHost(info), nil
			}
			// The error is returned, not fatal: the window says it is waiting, and
			// the next refresh tries again.
			c, err := getClient()
			if err != nil {
				return dashboard.Snapshot{}, info, err
			}
			snapshot, err := dashboard.Collect(c, resolveName())
			return snapshot, info, err
		}

		// The event stream. It is the fast path -- a line appears when it happens
		// rather than at the next two-second turn -- and the periodic collection
		// is the safety net: the list it reads is what puts back whatever a watch
		// that ended missed. Neither is allowed to be the only path.
		//
		// Nil under -mock, and not a function that returns nil: a watcher that
		// exists only to do nothing would be restarted every five seconds for
		// ever, which is a spin loop with a comment on it.
		var watch gui.Watch
		if !*mock {
			watch = func(ctx context.Context, fn func(kubeapi.Event)) error {
				c, err := getClient()
				if err != nil {
					return err
				}
				return c.WatchEvents(ctx, fn)
			}
		}

		ignoreQuitSignals()
		if err := gui.Run(collect, gui.Options{
			Interval: *interval,
			Watch:    watch,
			Device:   *drmDevice,
			Assets:   []string{"image/assets", "/usr/share/vates/assets"},
		}); err != nil {
			return err
		}
		return nil
	}
	if *once {
		client, err := kubeapi.NewFromKubeconfig(*kubeconfig)
		if err != nil {
			return fmt.Errorf("reading %s: %w", *kubeconfig, err)
		}
		snap, err := dashboard.Collect(client, resolveName())
		if err != nil {
			return err
		}
		if *asJSON {
			// A single reading of /proc/stat is a total since boot, not a rate,
			// so the CPU percentage is measured over a short interval. 250 ms is
			// long enough to be meaningful and short enough that a caller polling
			// every couple of seconds does not notice it.
			var reader hostinfo.Reader
			reader.Read()
			time.Sleep(250 * time.Millisecond)
			printJSON(snap, reader.Read())
			return nil
		}
		printOnce(snap)
		return nil
	}

	// The interactive display does NOT build the client here. The console starts
	// with the machine, before vates-init has written the kubeconfig, so a
	// client that must exist at startup would leave the console dead for the
	// first seconds of every boot -- or restarting, under Restart=always. The
	// model reads the file itself, reports it while it is missing, and picks it
	// up as soon as it appears.
	//
	// The console is the machine's face and cannot be left: there is no shell
	// behind it and no login prompt to return to, so Ctrl-C, q and Esc are all
	// inert here. The kernel console does not implement the alternate screen
	// buffer, so there is nothing to restore either; the model clears the screen
	// itself and fills every row, which is what actually removes the boot
	// messages from under the dashboard.
	ignoreQuitSignals()
	program := tea.NewProgram(
		tui.New(*kubeconfig, firstboot.KubeletDropIn, *node),
		// The default handler turns SIGINT (and SIGTERM) into a quit. It is
		// disabled, and the signals are ignored above, so that neither a
		// keystroke nor a stray kill can take the console down.
		tea.WithoutSignalHandler(),
	)
	if _, err := program.Run(); err != nil {
		return err
	}
	return nil
}

// ignoreQuitSignals makes the console impossible to leave by a keystroke or a
// conventional quit signal.
//
// The console is the machine's face: there is no shell behind it, no login
// prompt to return to, and PID 1 keeps it on tty1 for the life of the node.
// Ctrl-C is nevertheless easy to press by accident -- a terminal, or a
// hypervisor's keyboard, sends it -- and on a console it is not a request to
// stop a command but a way to lose the picture. SIGINT and SIGQUIT are ignored,
// and SIGTSTP too: Ctrl-Z would otherwise leave a stopped console on a screen
// nothing can restart.
//
// SIGTERM is deliberately NOT ignored: PID 1 stops the console with it on
// shutdown, and restarts the console if it ever exits for another reason.
func ignoreQuitSignals() {
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP)
}

// printJSON writes the snapshot as JSON.
//
// This exists so that the graphical dashboard does not reimplement any of this:
// it is the same collection, pointed at a different renderer. Two collectors
// would be two things to keep in agreement with the API, the kubeconfig and the
// drop-in, and they would drift.
//
// The shape is deliberately NOT the Kubernetes objects. Those are large, they
// carry fields nothing here uses, and their names would make this file a second
// copy of the API. What is written below is what a dashboard displays, and it
// changes only when the display does.
func printJSON(s dashboard.Snapshot, h hostinfo.Info) {
	type podCounts struct {
		Count    int `json:"count"`
		Restarts int `json:"restarts"`
	}
	type nodeView struct {
		Name    string   `json:"name"`
		Role    string   `json:"role"`
		Reason  string   `json:"reason,omitempty"`
		Address string   `json:"address"`
		Kubelet string   `json:"kubelet"`
		Runtime string   `json:"runtime"`
		OSImage string   `json:"os_image"`
		Taints  []string `json:"taints"`
		Ready   bool     `json:"ready"`
	}
	type hostView struct {
		CPUModel      string    `json:"cpu_model"`
		Load          []float64 `json:"load"`
		Gateways      []string  `json:"gateways"`
		DNS           []string  `json:"dns"`
		NTP           []string  `json:"ntp"`
		UptimeSeconds int64     `json:"uptime_seconds"`
		CPUs          int       `json:"cpus"`
		CPUPercent    float64   `json:"cpu_percent"`
		MemTotalBytes uint64    `json:"mem_total_bytes"`
		MemPercent    float64   `json:"mem_percent"`
		Procs         int       `json:"procs"`
	}
	type eventView struct {
		When    string `json:"when"`
		Type    string `json:"type"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}

	restarts := 0
	for _, p := range s.Pods {
		_, _, r := p.Ready()
		restarts += r
	}
	cpu := -1.0
	if v := h.CPUPercent(); v >= 0 {
		cpu = v
	}

	load := make([]float64, 0, 3)
	load = append(load, h.Load[0], h.Load[1], h.Load[2])

	events := make([]eventView, 0, len(s.Events))
	for _, e := range s.Events {
		when := ""
		if !e.When().IsZero() {
			when = e.When().Format(time.RFC3339)
		}
		events = append(events, eventView{When: when, Type: e.Type, Reason: e.Reason, Message: e.Message})
	}

	out := struct {
		Error  string      `json:"error,omitempty"`
		Host   hostView    `json:"host"`
		Node   nodeView    `json:"node"`
		Events []eventView `json:"events"`
		Pods   podCounts   `json:"pods"`
	}{
		Node: nodeView{
			Name:    s.Node.Metadata.Name,
			Role:    s.Node.Role(),
			Ready:   s.Ready,
			Reason:  s.ReadyReason,
			Address: s.Node.Address("InternalIP"),
			Kubelet: s.Node.Status.NodeInfo.KubeletVersion,
			Runtime: s.Node.Status.NodeInfo.ContainerRuntimeVersion,
			OSImage: s.Node.Status.NodeInfo.OSImage,
			Taints:  s.Node.Taints(),
		},
		Host: hostView{
			UptimeSeconds: int64(h.Uptime.Seconds()),
			CPUs:          h.CPUs,
			CPUModel:      h.CPUModel,
			CPUPercent:    cpu,
			MemTotalBytes: h.MemTotal,
			MemPercent:    h.MemPercent(),
			Procs:         h.Procs,
			Load:          load,
			Gateways:      h.Gateways,
			DNS:           h.DNS,
			NTP:           h.NTP,
		},
		Pods:   podCounts{Count: len(s.Pods), Restarts: restarts},
		Events: events,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out) // the encoder writes to stdout: a write failure is not actionable here
}

func printOnce(s dashboard.Snapshot) {
	ready := "Ready"
	if !s.Ready {
		ready = "NotReady"
		if s.ReadyReason != "" {
			ready = "NotReady (" + s.ReadyReason + ")"
		}
	}
	fmt.Printf("node      %s\n", s.Node.Metadata.Name)
	fmt.Printf("role      %s\n", s.Node.Role())
	fmt.Printf("state     %s\n", ready)
	fmt.Printf("address   %s\n", s.Node.Address("InternalIP"))
	fmt.Printf("kubelet   %s\n", s.Node.Status.NodeInfo.KubeletVersion)
	fmt.Printf("runtime   %s\n", s.Node.Status.NodeInfo.ContainerRuntimeVersion)
	fmt.Printf("image     %s\n", s.Node.Status.NodeInfo.OSImage)
	fmt.Printf("taints    %v\n", s.Node.Taints())

	fmt.Printf("\npods (%d)\n", len(s.Pods))
	for _, p := range s.Pods {
		r, total, restarts := p.Ready()
		line := fmt.Sprintf("  %-12s %-44s %d/%d %s", p.Metadata.Namespace, p.Metadata.Name, r, total, p.Status.Phase)
		if restarts > 0 {
			line += fmt.Sprintf("  restarts=%d", restarts)
		}
		fmt.Println(line)
	}

	// Newest last is the order of a feed; the printed view reverses it so the
	// most recent is the first thing read.
	events := append([]kubeapi.Event(nil), s.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].When().After(events[j].When()) })
	fmt.Printf("\nevents (%d)\n", len(events))
	for i, e := range events {
		if i >= 20 {
			fmt.Printf("  ... %d more\n", len(events)-i)
			break
		}
		fmt.Printf("  %s  %-8s %-22s %s\n", e.When().Format("15:04:05"), e.Type, e.Reason, truncate(e.Message, 70))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "\u2026"
}
