package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"

	"github.com/vatesfr/vates-kube-os/internal/dashboard"
	"github.com/vatesfr/vates-kube-os/internal/hostinfo"
	"github.com/vatesfr/vates-kube-os/internal/kubeapi"
)

// refreshInterval is how often the node is re-read. Two seconds is fast enough
// to feel live and slow enough that the API is not being asked to answer a
// dashboard all day.
const refreshInterval = 2 * time.Second

// notAvailable is what a value the cluster has not supplied reads. A zero or an
// empty string would be a reading; this says there is nothing to read yet.
const notAvailable = "n/a"

// Model is the bubbletea model.
type Model struct {
	// kubeconfig is re-read until it exists. The console starts with the
	// machine, and the file is written by vates-init, so for the first seconds
	// of a boot there is nothing to read yet -- which is a state to display, not
	// a reason to exit.
	kubeconfig string

	// dropIn is the systemd drop-in vates-init writes for the kubelet. It holds
	// the node's name and its address, so the grid has something to show before
	// the API answers.
	dropIn string

	// nodeOverride is the --node flag, for running the dashboard about one node
	// from another machine.
	nodeOverride string

	client *kubeapi.Client
	host   hostinfo.Reader

	snapshot dashboard.Snapshot
	hostInfo hostinfo.Info
	k8sLocal string
	nodeIP   string

	err           error
	width, height int

	// scroll is how many lines above the newest the feed is showing. 0 follows
	// the feed; more than 0 is a reader who scrolled up, and their place is kept
	// as new events arrive rather than being dragged back to the bottom.
	scroll int

	// unicode is whether the console's font carries the box-drawing glyphs, so
	// the frame can be drawn with them. PID 1 loads such a font and says so
	// through DASHBOARD_UNICODE; without it (a serial console, a font that did
	// not load) the frame falls back to ASCII.
	unicode bool
}

// New returns a model ready to run.
func New(kubeconfig, dropIn, nodeOverride string) Model {
	return Model{
		kubeconfig:   kubeconfig,
		dropIn:       dropIn,
		nodeOverride: nodeOverride,
		unicode:      os.Getenv("DASHBOARD_UNICODE") != "",
	}
}

// nodeName asks which node this machine is. Called on every refresh: the console
// starts before the name has been written anywhere, so a value captured once at
// startup is the image's build hostname, not the node's.
func (m Model) nodeName() string {
	return hostinfo.ResolveNodeName(m.nodeOverride, m.dropIn)
}

// Messages.
type (
	// tickMsg asks for another collection.
	tickMsg time.Time
	// snapshotMsg carries the result back to the model.
	//
	// hostReader travels with the reading because the CPU percentage is a
	// DIFFERENCE between two readings, and the Reader is what holds the previous
	// one. It is a value, not a pointer: the collection runs on a goroutine of
	// its own, and handing the state back through the message is what keeps that
	// goroutine from sharing mutable state with the render loop. Dropping it --
	// which is what this did until the CPU tile was noticed sitting at n/a for
	// the life of the process -- leaves every sample without a predecessor.
	snapshotMsg struct {
		snapshot   dashboard.Snapshot
		host       hostinfo.Info
		hostReader hostinfo.Reader
		client     *kubeapi.Client
		k8sLocal   string
		nodeIP     string
		err        error
	}
)

// Init starts the first collection immediately, rather than leaving the screen
// blank for one interval.
//
// It also clears the screen. The kernel console does NOT implement the
// alternate screen buffer, so the dashboard cannot rely on drawing "on top" of
// what came before: the boot messages stay where they were, below the few rows
// the dashboard draws, and look exactly like new kernel traces. Measured: a
// screenshot showed the dashboard at the top and two lines from t=11s still
// sitting under it minutes later. Clearing, and then filling the whole height,
// is what removes them.
func (m Model) Init() tea.Cmd {
	return tea.Batch(tea.ClearScreen, m.collect(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// sizeProbe re-reads the terminal size and hands it back as a WindowSizeMsg.
//
// A WindowSizeMsg is what tells bubbletea's renderer the geometry AND makes it
// repaint from scratch. Both matter here: the kernel console sends no resize
// signal, so the renderer never learns the console took its real size -- and on
// a console where the kernel and systemd also write, a full repaint is what
// repairs a line they overwrote.
func sizeProbe() tea.Msg {
	w, h, err := term.GetSize(os.Stdout.Fd())
	if err != nil || w <= 0 || h <= 0 {
		// A serial or Xen console reports no size at all, and a zero geometry
		// draws nothing: the screen sat on "starting..." for ever, because the
		// width the model needs never arrived. Fall back to the conventional
		// 80x24, a frame that fits everywhere; a client that does set a size is
		// picked up at the next tick, since the size is re-read on every refresh.
		return tea.WindowSizeMsg{Width: 80, Height: 24}
	}
	return tea.WindowSizeMsg{Width: w, Height: h}
}

// collect runs the blocking work off the render loop, so that a slow or
// unreachable API freezes the data and not the screen.
//
// A nil client means the kubeconfig has not been readable yet; it is retried
// here so that a console which started before its own configuration appeared
// begins working on its own, without a service restart.
func (m Model) collect() tea.Cmd {
	return func() tea.Msg {
		var msg snapshotMsg

		reader := m.host
		msg.host = reader.Read()
		// The reader now holds this reading's CPU counters as the predecessor of
		// the next one. It has to go back with the message: the copy lives on
		// this goroutine and Update is the only place the model is stored, so
		// leaving it here would throw the predecessor away and no sample would
		// ever have one.
		msg.hostReader = reader
		msg.k8sLocal = hostinfo.KubernetesVersionFromDropIn(m.dropIn)
		_, msg.nodeIP = hostinfo.NodeFromDropIn(m.dropIn)

		msg.client = m.client
		if msg.client == nil {
			c, err := kubeapi.NewFromKubeconfig(m.kubeconfig)
			if err != nil {
				msg.err = err
				return msg
			}
			msg.client = c
		}

		snap, err := dashboard.Collect(msg.client, m.nodeName())
		if err != nil {
			msg.err = err
			return msg
		}
		msg.snapshot = snap
		return msg
	}
}

// Update handles events.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		// No key quits. The console is the machine's face and there is nowhere
		// to go back to, so q, Ctrl-C and Esc move nothing; the keys that mean
		// something here are the ones that move the reader through the feed.
		switch msg.String() {
		case "up", "k":
			m.scroll++
		case "down", "j":
			if m.scroll > 0 {
				m.scroll--
			}
		case "pgup":
			m.scroll += m.pageLines()
		case "pgdown":
			m.scroll = max(0, m.scroll-m.pageLines())
		case "home", "g":
			m.scroll = len(m.snapshot.Events)
		case "end", "G":
			m.scroll = 0
		}
		m.scroll = min(m.scroll, len(m.snapshot.Events))
		return m, nil

	case tickMsg:
		// The size is handed back through the program as a WindowSizeMsg, not
		// assigned here.
		//
		// That message is what tells bubbletea's renderer the geometry AND
		// makes it repaint from scratch. Both are needed on a kernel console:
		// it sends no resize signal, so the renderer never learns the console
		// grew from its initial size -- measured, the new frame was drawn BELOW
		// the old one, leaving both on screen -- and a full repaint is also what
		// repairs a line the kernel or systemd wrote over.
		//
		// Sent every refresh, not only when the size changed: it costs one
		// redraw of a screen that is already in memory, and it means the display
		// cannot drift.
		return m, tea.Batch(sizeProbe, m.collect(), tick())

	case snapshotMsg:
		if msg.client != nil {
			m.client = msg.client
		}
		// The reader's state is stored before the error is looked at: the CPU
		// counters are collected whatever the API did, and a failed collection
		// must not cost the NEXT one its predecessor.
		m.host = msg.hostReader
		m.hostInfo = msg.host
		m.k8sLocal = msg.k8sLocal
		m.nodeIP = msg.nodeIP
		if msg.err != nil {
			// The last good snapshot is kept when a refresh fails: a dashboard
			// that blanks itself because one poll timed out is less useful than
			// one that shows the last known state and says it is stale.
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.snapshot = msg.snapshot
		return m, nil
	}
	return m, nil
}

// Styles. Colours stay inside the 16 the kernel console has; a colour from the
// 256-colour cube is silently dropped there.
var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	labelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	valueStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	strongStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	okStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	badStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	borderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// pair is one labelled value.
type pair struct {
	label string
	value string
	style lipgloss.Style
}

// field builds a labelled value, showing n/a when the cluster has not supplied
// one. A blank cell reads as a drawing fault; n/a reads as an answer -- the
// machine is up and the cluster has not replied yet.
func field(label, value string, style lipgloss.Style) pair {
	if strings.TrimSpace(value) == "" {
		return pair{label: label, value: notAvailable, style: dimStyle}
	}
	return pair{label: label, value: value, style: style}
}

// View draws the screen.
func (m Model) View() string {
	if m.width == 0 {
		return "starting..."
	}

	inner := m.innerWidth()
	info := m.headerLine() + "\n\n" + m.grid(inner)
	infoPanel := m.panel(titleStyle.Render(m.nodeName()), info, 0)

	// The feed takes exactly the rest of the screen, so the two panels together
	// cover every row. A panel that stopped short would leave the previous
	// contents -- boot messages, an earlier frame -- visible under it.
	//
	// Joined with "\n\n", and the -1 above is that blank row: joining with a
	// single "\n" makes the panels adjacent, one row short of the screen, and
	// the missing row keeps whatever was there before. Measured: the frame
	// occupied rows 1..47 of 48, with a stale title still in row 0.
	feedTotal := m.height - lipgloss.Height(infoPanel) - 1
	feedTotal = max(feedTotal, 4)
	title := "events"
	if m.scroll > 0 {
		title = fmt.Sprintf("events  \u2191%d  (End follows)", m.scroll)
	}
	out := infoPanel + "\n\n" +
		m.panel(dimStyle.Render(title), m.feed(feedTotal-3, inner), feedTotal)
	return out
}

// haveSnapshot reports whether the cluster has ever answered about this node.
func (m Model) haveSnapshot() bool {
	return m.snapshot.Node.Metadata.Name != ""
}

// configured reports whether vates-init has finished setting this machine up.
//
// The program's last act is to write the kubelet drop-in, so the file existing
// means the node is configured whether or not the cluster has answered. It is
// what separates "still configuring" from "configured, waiting for the API" --
// two states that look identical through the API alone, because in both the API
// has said nothing.
func (m Model) configured() bool {
	if m.dropIn == "" {
		return false
	}
	_, err := os.Stat(m.dropIn)
	return err == nil
}

// headerLine is the one-line summary of the machine, above the panel.
//
// The fields are ordered by how much they matter and added until the line is
// full, rather than all being emitted to wrap: on a narrow console a wrapped
// header costs a line of the feed, and the feed is the part that moves. The CPU
// model is the first thing dropped, since it never changes.
func (m Model) headerLine() string {
	h := m.hostInfo

	// Most useful first. The model is deliberately last: it is the only field
	// that is constant for the life of the machine.
	candidates := []string{strongStyle.Render(m.version())}
	// What the node is DOING leads, while it is coming up: it answers the only
	// question asked in front of a machine that is not Ready yet.
	if p, ok := dashboard.ReadPhase(); ok && !m.snapshot.Ready {
		candidates = append(candidates, valueStyle.Render("booting: "+p.Line(time.Now())))
	}
	candidates = append(candidates, "uptime "+h.Uptime.Round(time.Second).String())
	if h.CPUs > 0 {
		candidates = append(candidates, fmt.Sprintf("%d CPU", h.CPUs))
	}
	if h.MemTotal > 0 {
		candidates = append(candidates, gib(h.MemTotal)+" RAM")
	}
	if p := h.CPUPercent(); p >= 0 {
		candidates = append(candidates, fmt.Sprintf("CPU %.1f%%", p))
	}
	if h.MemTotal > 0 {
		candidates = append(candidates, fmt.Sprintf("RAM %.1f%%", h.MemPercent()))
	}
	if h.Procs > 0 {
		candidates = append(candidates, fmt.Sprintf("PROCS %d", h.Procs))
	}
	if h.CPUs > 0 && h.CPUModel != "" {
		candidates = append(candidates, fmt.Sprintf("%dx%s", h.CPUs, modelShort(h.CPUModel)))
	}

	const sep = "  ·  "
	width := m.innerWidth()
	used := 0
	var parts []string
	for _, c := range candidates {
		w := lipgloss.Width(c)
		if len(parts) > 0 {
			w += len(sep)
		}
		if used+w > width && len(parts) > 0 {
			break
		}
		parts = append(parts, c)
		used += w
	}
	return labelStyle.Render(strings.Join(parts, sep))
}

// version is the Kubernetes version to name in the header: the node's own
// kubelet version when the cluster has answered, and the version this node was
// told to run before that -- read from the same drop-in the node configured
// itself from, never from the image, which has no version.
func (m Model) version() string {
	if v := m.snapshot.Node.Status.NodeInfo.KubeletVersion; v != "" {
		return v
	}
	if m.k8sLocal != "" {
		return m.k8sLocal
	}
	return "kubernetes n/a"
}

// nodeStatus is the STAGE / READY / SYSTEM triple and the styles that colour it.
type nodeStatus struct {
	stage       string
	stageStyle  lipgloss.Style
	ready       string
	readyStyle  lipgloss.Style
	system      string
	systemStyle lipgloss.Style
}

// status computes the machine's displayed state.
//
// Three states, and the middle one is why this is its own function: the API
// having said nothing looks identical whether the machine is still being
// configured or is done and its endpoint simply does not answer. The drop-in
// vates-init writes last tells the two apart.
func (m Model) status() nodeStatus {
	s := nodeStatus{
		stage:       "Configuring",
		stageStyle:  warnStyle,
		ready:       notAvailable,
		readyStyle:  dimStyle,
		system:      notAvailable,
		systemStyle: dimStyle,
	}
	if !m.haveSnapshot() {
		if m.configured() {
			s.stage, s.stageStyle = "Configured", okStyle
			// The API has not answered, so the components' own state is
			// unknown. Saying THAT is honest; a word would be invented.
			s.system, s.systemStyle = "Waiting for API", dimStyle
		}
		return s
	}
	s.stage, s.stageStyle = "Running", okStyle
	if m.snapshot.Ready {
		s.ready, s.readyStyle = "True", okStyle
	} else {
		s.ready, s.readyStyle = "False", badStyle
		if m.snapshot.ReadyReason != "" {
			s.ready = "False (" + m.snapshot.ReadyReason + ")"
		}
	}
	// The node's own readiness is the READY field; this is the cluster's
	// components ON this node, which is a different and slower thing.
	s.system, s.systemStyle = toneStyle(m.snapshot.SystemState())
	return s
}

// toneStyle pairs a value with the style its tone asks for, so a value computed
// in the dashboard's terms (ToneGood, ToneWarn, ...) is coloured the way the
// rest of this console colours the same tone.
func toneStyle(value, tone string) (string, lipgloss.Style) {
	switch tone {
	case dashboard.ToneGood:
		return value, okStyle
	case dashboard.ToneWarn:
		return value, warnStyle
	case dashboard.ToneBad:
		return value, badStyle
	case dashboard.ToneDim:
		return value, dimStyle
	default:
		return value, valueStyle
	}
}

// nodeFacts are the values read from the node object, or n/a when the API has
// not answered.
func (m Model) nodeFacts() (role, taints, pods, restarts string) {
	role, taints, pods, restarts = notAvailable, notAvailable, notAvailable, notAvailable
	if !m.haveSnapshot() {
		return
	}
	n := m.snapshot.Node
	role = n.Role()
	taints = "-"
	if t := n.Taints(); len(t) > 0 {
		taints = strings.Join(t, " ")
	}
	pods = fmt.Sprint(len(m.snapshot.Pods))
	total := 0
	for _, p := range m.snapshot.Pods {
		_, _, r := p.Ready()
		total += r
	}
	restarts = fmt.Sprint(total)
	return
}

// grid builds the labelled values, in as many columns as the console fits.
//
// Nothing is left blank: a value the cluster has not supplied reads n/a, which
// is information -- it says the machine is up and the cluster has not answered
// yet.
func (m Model) grid(inner int) string {
	n := m.snapshot.Node
	st := m.status()
	role, taints, pods, restarts := m.nodeFacts()

	// The address comes from the API when it is there, and from the drop-in
	// before that -- the lease taken at first boot, which is the same address
	// unless the lease has changed without a reboot.
	address := m.nodeIP
	if m.haveSnapshot() {
		address = n.Address("InternalIP")
	}

	fields := []pair{
		field("NODE", m.nodeName(), strongStyle),
		field("ROLE", role, valueStyle),
		field("STAGE", st.stage, st.stageStyle),
		field("READY", st.ready, st.readyStyle),
		field("SYSTEM", st.system, st.systemStyle),
		field("TAINTS", taints, dimStyle),
		field("KUBERNETES", m.version(), valueStyle),
		field("KUBELET", n.Status.NodeInfo.KubeletVersion, valueStyle),
		field("CONTAINERD", n.Status.NodeInfo.ContainerRuntimeVersion, valueStyle),
		field("PODS", pods, strongStyle),
		field("RESTARTS", restarts, valueStyle),
		field("IP", address, valueStyle),
		field("GW", joinOrDash(m.hostInfo.Gateways), valueStyle),
		field("DNS", joinOrDash(m.hostInfo.DNS), valueStyle),
		field("NTP", joinOrDash(m.hostInfo.NTP), valueStyle),
		field("LOAD", loadString(m.hostInfo.Load), valueStyle),
	}

	cols := columnCount(m.width)
	perCol := (len(fields) + cols - 1) / cols
	columns := make([][]pair, 0, cols)
	for i := 0; i < len(fields); i += perCol {
		end := min(i+perCol, len(fields))
		columns = append(columns, fields[i:end])
	}
	return renderColumns(columns, inner)
}

// feed renders the event stream, newest last, bounded to height lines.
//
// When the cluster has not answered, the reason goes here: the feed is where
// activity belongs, and it is better than an empty panel that looks like a quiet
// cluster. A failure after a good snapshot is reported as a stale line at the
// bottom, so the last known state stays visible.
func (m Model) feed(height, inner int) string {
	events := m.snapshot.Events
	var lines []string
	switch {
	case len(events) == 0 && m.err != nil:
		// The console starts with the machine: before the API answers and
		// before the kubelet has registered the node, a read FAILS -- and that
		// failure is the node coming up, not a fault. It is drawn calm, and the
		// header's phase line says what is actually being waited for. Only once
		// the cluster has answered once is a failure a failure.
		if m.haveSnapshot() {
			lines = append(lines, warnStyle.Render(truncate(m.err.Error(), inner)))
		} else {
			lines = append(lines, dimStyle.Render(truncate("waiting for the API server", inner)))
		}
	case len(events) == 0:
		lines = append(lines, dimStyle.Render("none"))
	default:
		// A window that ends `scroll` lines above the newest. scroll is 0 when
		// following, so the window is the tail -- unless the reader scrolled up,
		// in which case their place is held while new events arrive below.
		off := min(m.scroll, max(0, len(events)-height))
		end := len(events) - off
		start := max(0, end-height)
		for _, e := range events[start:end] {
			lines = append(lines, formatEvent(e, inner, m.snapshot.BootEvent(e)))
		}
	}

	if m.err != nil && m.haveSnapshot() {
		note := warnStyle.Render(truncate("stale: "+m.err.Error(), inner))
		if len(lines) >= height {
			lines = lines[1:]
		}
		lines = append(lines, note)
	}
	return strings.Join(lines, "\n")
}

// formatEvent renders one event as a log line. boot is
// Snapshot.BootEvent(e): an expected boot reason that belongs to the node's
// way up is drawn calm, the same reason on a node that has been up is a warning.
func formatEvent(e kubeapi.Event, inner int, boot bool) string {
	style := valueStyle
	switch dashboard.EventTone(e, boot) {
	case dashboard.ToneDim:
		style = dimStyle
	case dashboard.ToneWarn:
		style = warnStyle
	case dashboard.ToneBad:
		style = badStyle
	}
	when := "  --:--:--"
	if !e.When().IsZero() {
		when = e.When().Format("  15:04:05")
	}
	return dimStyle.Render(when) + " " +
		style.Render(fmt.Sprintf("%-7s", e.Type)) + " " +
		truncate(e.Message, max(10, inner-22))
}

// columnCount picks how many columns the grid uses.
//
// The thresholds are the widths at which a column would otherwise be too narrow
// to hold a label and a value: below 62 columns one column, below 118 two.
func columnCount(width int) int {
	switch {
	case width >= 118:
		return 3
	case width >= 62:
		return 2
	default:
		return 1
	}
}

// innerWidth is the number of columns available inside a panel: the terminal
// width, less the border and padding on both sides.
func (m Model) innerWidth() int {
	return max(10, m.width-4)
}

// pageLines is how far PageUp and PageDown move: about a third of the screen, so
// a page keeps some of what was on it for context.
func (m Model) pageLines() int {
	return max(1, m.height/3)
}

// panel wraps content in a full-width frame.
//
// The width arithmetic is exact, and lipgloss's rule is the reason it looks
// fussy: the border is added OUTSIDE the width, while the padding is counted
// INSIDE it. So a frame of the terminal's width w needs Width(w-2) -- two
// columns, one per border -- and the text area inside it is w-2-2, four columns
// less than the terminal. Measured, because getting it wrong is invisible except
// on a console: using w-4 for the Width gave a frame 126 columns wide on a
// 128-column screen, a two-column gap down the right-hand side.
//
// The frame is drawn with box-drawing characters when the console's font carries
// them -- PID 1 loads eurlatgr and sets DASHBOARD_UNICODE -- and with ASCII
// otherwise. Measured on a node with the built-in VGA font: the vertical bar of a
// rounded border rendered and the horizontal dashes did NOT, so the panels
// appeared to have sides and no top or bottom -- that font does not carry every
// glyph of the box-drawing block. "+", "-" and "|" are in every font there is.
//
// totalHeight, when positive, pins the whole frame to that many rows by padding
// the content. That is how the panels cover the screen exactly and leave no row
// of whatever was displayed before.
func (m Model) panel(title, content string, totalHeight int) string {
	textWidth := m.innerWidth()
	lines := strings.Split(content, "\n")

	if totalHeight > 3 {
		want := totalHeight - 3 // the title row and the two border rows
		for len(lines) < want {
			lines = append(lines, "")
		}
		if len(lines) > want {
			lines = lines[:want]
		}
	}

	for i, line := range lines {
		lines[i] = pad(line, textWidth)
	}
	body := title + "\n" + strings.Join(lines, "\n")
	return borderStyle.Render(lipgloss.NewStyle().
		Border(frameBorder(m.unicode)).
		Padding(0, 1).
		Width(max(10, m.width-2)).
		Render(body))
}

// frameBorder is the panel frame: the Unicode box-drawing one when the console's
// font carries those glyphs, ASCII otherwise. The rounded border is not used: its
// corners (U+256D..U+2570) are in neither the built-in VGA font nor eurlatgr,
// while every glyph of the normal border is in eurlatgr.
func frameBorder(unicode bool) lipgloss.Border {
	if unicode {
		return lipgloss.NormalBorder()
	}
	return lipgloss.ASCIIBorder()
}

// renderColumns lays labelled pairs out in columns, each column as wide as its
// widest line, with the labels aligned across the whole grid.
func renderColumns(cols [][]pair, inner int) string {
	if len(cols) == 0 {
		return ""
	}
	labelWidth := 0
	for _, c := range cols {
		for _, p := range c {
			labelWidth = max(labelWidth, len(p.label))
		}
	}

	const gap = 3
	each := (inner - gap*(len(cols)-1)) / len(cols)
	valueWidth := max(6, each-labelWidth-2)

	rows := make([][]string, len(cols))
	widths := make([]int, len(cols))
	for i, c := range cols {
		for _, p := range c {
			line := labelStyle.Render(fmt.Sprintf("%-*s", labelWidth, p.label)) + "  " +
				p.style.Render(truncate(p.value, valueWidth))
			rows[i] = append(rows[i], line)
			widths[i] = max(widths[i], lipgloss.Width(line))
		}
	}

	height := 0
	for _, r := range rows {
		height = max(height, len(r))
	}
	out := make([]string, 0, height)
	for line := 0; line < height; line++ {
		var parts []string
		for i := range cols {
			text := ""
			if line < len(rows[i]) {
				text = rows[i][line]
			}
			parts = append(parts, pad(text, widths[i]))
		}
		out = append(out, strings.TrimRight(strings.Join(parts, strings.Repeat(" ", gap)), " "))
	}
	return strings.Join(out, "\n")
}

// pad extends a line (which may contain colour escapes) to n display columns.
func pad(s string, n int) string {
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

// truncate cuts a string to n runes, not bytes: a truncated image tag or name
// with a multi-byte character would otherwise end mid-character.
func truncate(s string, n int) string {
	if n <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "\u2026"
}

func joinOrDash(v []string) string {
	if len(v) == 0 {
		return notAvailable
	}
	return strings.Join(v, " ")
}

func loadString(l [3]float64) string {
	return fmt.Sprintf("%.2f %.2f %.2f", l[0], l[1], l[2])
}

// modelShort trims the CPU model to what fits a header without a wall of text.
//
// The vendor's full string ("AMD Ryzen 9 9955HX 16-Core Processor") is mostly
// the words "Core Processor", which say nothing the CPU count beside it does
// not. Cutting at the first of those leaves the part that identifies the chip.
func modelShort(s string) string {
	s = strings.TrimSpace(s)
	for _, cut := range []string{" @ ", " CPU ", " 16-Core", "-Core", " Core"} {
		if i := strings.Index(s, cut); i > 0 {
			s = s[:i]
			break
		}
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, " Processor"))
	if len([]rune(s)) > 20 {
		s = truncate(s, 20)
	}
	return s
}

// gib formats a byte count as GiB.
func gib(b uint64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}
