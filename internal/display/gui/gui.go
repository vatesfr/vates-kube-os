package gui

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/dashboard"
	"github.com/vatesfr/vates-kube-os/internal/display/gui/drm"
	"github.com/vatesfr/vates-kube-os/internal/display/gui/render"
	"github.com/vatesfr/vates-kube-os/internal/hostinfo"
	"github.com/vatesfr/vates-kube-os/internal/kubeapi"
)

// Collector gathers what the screen shows. It runs on its own goroutine: a slow
// API must not freeze the drawing.
type Collector func() (dashboard.Snapshot, hostinfo.Info, error)

// wheelMerge is how long a burst of wheel notches is collected before the screen
// is updated: one display frame. Long enough that a spun wheel becomes one
// update instead of one per notch, short enough that a single notch is not felt.
const wheelMerge = 16 * time.Millisecond

// Watch is the optional event stream: the caller supplies the API, this package
// supplies the goroutine and the back-off. A nil Watch means there is none --
// the mock, or a machine with no API to talk to -- and the console then shows
// exactly what the periodic collection gives it, which it always does anyway.
//
// It is a function rather than an interface so that the console never has to
// know about kubeapi: what arrives is already an Event, and what the function
// returns is already an error to be logged.
type Watch func(ctx context.Context, fn func(kubeapi.Event)) error

// Options is what the caller decides.
type Options struct {
	// Interval is how often the collection is refreshed.
	Interval time.Duration

	// Watch streams events as they happen. Optional.
	Watch Watch

	// Assets are the directories searched for the wordmark, in order: beside
	// the binary when run from a checkout, and /usr/share when installed.
	Assets []string

	// Device is the DRM card to take. Empty means the default.
	Device string
}

// frame is one collection on its way to the drawing.
type frame struct {
	snapshot dashboard.Snapshot
	info     hostinfo.Info
	err      error
}

// fonts is every face this screen uses, resolved once.
type fonts struct {
	hostname render.Font
	label    render.Font
	value    render.Font
	note     render.Font
	pill     render.Font
	section  render.Font
	event    render.Font
	footer   render.Font
}

// screen owns the display, the drawing state and the layout.
type screen struct {
	display *drm.Display
	surface render.Surface
	ctx     *render.Context
	w, h    int
	scale   float64

	logo  render.Surface
	fonts fonts

	snap dashboard.Snapshot
	info hostinfo.Info
	err  error

	// The feed: lines laid out in content coordinates, and the offset of the
	// panel's top within them. Which way it scrolls is the whole difference
	// between a memory copy and a full repaint.
	lines   []line
	offset  int
	follow  bool
	heights map[string]int
	lineH   int // the height of one line of the feed, in pixels

	// history is what the feed keeps: every event it has been shown, in order,
	// deduplicated by identity. It is NOT the snapshot's list. That list is
	// bounded -- the API returns the last N, the mock regenerates its own -- and
	// a feed built straight from it throws away its oldest events as new ones
	// arrive. At the bottom that is invisible; at the TOP, where a reader has
	// deliberately gone to read, it means the ground moves under them: the
	// oldest line is dropped and everything they are looking at slides up. What
	// the reader has seen is kept until the feed is long, and then dropped from
	// the far end only.
	history []kubeapi.Event
	seen    map[string]bool

	// The pointer. The wheel owns where it is; the cursor owns the pixels it
	// covers, so that it can be put back.
	wheel   *drm.Wheel
	cursors *cursor

	// The scrollbar grab. dragging is the left button held on the bar, and
	// dragGrab is how far down the thumb it was taken hold of: a thumb that
	// jumps to the pointer on the first move is a thumb that cannot be dragged.
	dragging bool
	dragGrab int

	// Layout, computed once from the screen's size.
	padding    int
	brandH     int
	headerTop  int
	headerH    int
	tilesTop   int
	tileH      int
	tileGap    int
	sectionTop int
	panelX     int
	panelY     int
	panelW     int
	panelH     int
	padH       int
	padV       int
	feedX      int
	feedY      int
	feedW      int
	feedH      int
	feedInnerW int
	feedInnerH int
	footerTop  int
}

// Run takes the screen and draws until the program is killed.
func Run(collect Collector, opts Options) error {
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.Device == "" {
		opts.Device = "/dev/dri/card0"
	}

	display, err := drm.Open(opts.Device)
	if err != nil {
		return err
	}
	defer display.Close()

	s := &screen{
		display: display,
		w:       display.Width(),
		h:       display.Height(),
		follow:  true,
		heights: make(map[string]int),
		seen:    make(map[string]bool),
	}
	// The one number the whole layout is built from: how much bigger or smaller
	// this screen is than the size the dashboard was drawn at. The machine does
	// not choose a resolution we can assume -- a QEMU/qxl console gives 1024x768,
	// Xen Orchestra's Cirrus VGA gives 800x600 or less -- so the screen measures
	// itself and everything scales from there.
	s.scale = scaleFor(s.w, s.h)
	s.surface = render.SurfaceOn(display.Memory(), display.Width(), display.Height(), display.Stride())
	if !s.surface.Valid() {
		return fmt.Errorf("console: could not lay a buffer over the screen")
	}
	defer s.surface.Destroy()
	s.ctx = render.New(s.surface)
	defer s.ctx.Free()

	if os.Getenv("VATES_WHEEL_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "console: screen %dx%d stride %d, input debug on\n",
			s.w, s.h, display.Stride())
	}

	s.loadFonts()
	s.loadLogo(opts.Assets)
	s.layout()
	s.watchSignals()

	// Fill before drawing anything: what is in the buffer before the first write
	// is the firmware's, or the previous program's.
	s.fillBackground()
	s.present()

	wheel := drm.OpenWheel(s.w, s.h)
	defer wheel.Close()
	s.wheel = wheel
	s.cursors = &cursor{bg: make([]byte, cursorW*cursorH*4)}

	// The event stream, if the caller has one. It arrives on its own channel and
	// is drawn on this goroutine, like everything else: the watch goroutine only
	// ever pushes, and never touches the screen, the lines or the offset.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watched := make(chan kubeapi.Event, 256)
	if opts.Watch != nil {
		go watchLoop(ctx, opts.Watch, watched)
	}

	frames := make(chan frame, 1)
	go collectLoop(collect, opts.Interval, frames)

	s.redraw()
	s.present()

	for {
		select {
		case f := <-frames:
			s.apply(f)
			s.redraw()
			s.present()
		case e := <-watched:
			if s.addWatched(e) {
				s.redraw()
				s.present()
			}
		case lines := <-wheel.Steps():
			// Coalesce the burst, then present at most once per display frame.
			//
			// A wheel spun fast delivers notches faster than a screen can be
			// refreshed, and every notch was one full screen update: measured on
			// this machine at ~64 notches a second, presenting each one kept qemu
			// on the host at 74 % of a core on average and 279 % at peak. The
			// window below merges whatever arrives within one frame, so a burst
			// becomes one update. It is short enough that a single notch is not
			// felt, and it is a CEILING, not a delay: with nothing else waiting,
			// the first notch is drawn as soon as the window closes.
			total := lines
			timer := time.NewTimer(wheelMerge)
		merge:
			for {
				select {
				case l := <-wheel.Steps():
					total += l
				case <-timer.C:
					break merge
				}
			}
			// The input says how many lines to move, positive towards the older
			// events; the offset grows towards the newer ones.
			s.scroll(-total * s.lineH)
		case <-wheel.Motion():
			s.moveCursor()
			// The button may be held: the bar follows the pointer, and the feed
			// follows the bar.
			if s.dragging {
				if _, y, ok := s.wheel.Position(); ok {
					s.dragTo(y - s.dragGrab)
				}
			}
		case down := <-wheel.Buttons():
			if down {
				if x, y, ok := s.wheel.Position(); ok {
					s.grabScrollbar(x, y)
				}
			} else {
				s.releaseScrollbar()
			}
		}
	}
}

// watchLoop keeps a watcher running. Every stream ends -- the API's timeout, a
// proxy, a restart -- so ending is normal and the loop reconnects; only the
// error is worth a line in the journal, and only when it is new or when a minute
// has passed, because a refused watch repeats for ever and a console that fills
// the journal is a console nobody can read.
//
// The gap a reconnect leaves is not this loop's problem to solve: the periodic
// collection keeps running beside it, and the list it reads is what puts back
// whatever the stream missed.
func watchLoop(ctx context.Context, watch Watch, out chan<- kubeapi.Event) {
	const backoff = 5 * time.Second
	var (
		lastErr  string
		lastSeen time.Time
	)
	for {
		err := watch(ctx, func(e kubeapi.Event) {
			select {
			case out <- e:
			default: // the drawing is behind: dropping an event beats blocking the stream
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if msg := err.Error(); msg != lastErr || time.Since(lastSeen) > time.Minute {
				fmt.Fprintf(os.Stderr, "console: event watch: %v\n", msg)
				lastErr, lastSeen = msg, time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// addWatched takes one streamed event into the feed. It reports whether the
// screen has to be redrawn: an event that arrives while the reader is at the
// bottom is shown at once, and one that arrives while they are reading further
// up is kept, below the fold, and does not move what they are looking at.
func (s *screen) addWatched(e kubeapi.Event) bool {
	node := s.snap.Node.Metadata.Name
	if node == "" {
		return false
	}
	if len(dashboard.FilterEvents([]kubeapi.Event{e}, node, s.snap.Pods)) == 0 {
		return false
	}
	s.absorb([]kubeapi.Event{e})
	s.refreshFeed()
	return s.follow
}

// collectLoop gathers in the background and hands frames over.
//
// The collection runs here and not in the drawing: a frame is milliseconds and a
// collection is a network call, and tying them together is what makes a screen
// freeze instead of merely lag.
func collectLoop(collect Collector, interval time.Duration, out chan frame) {
	for {
		snapshot, info, err := collect()
		f := frame{snapshot: snapshot, info: info, err: err}
		select {
		case out <- f:
		default:
			// The drawing is behind: drop what is waiting and keep the new.
			select {
			case <-out:
			default:
			}
			select {
			case out <- f:
			default:
			}
		}
		time.Sleep(interval)
	}
}

func (s *screen) loadFonts() {
	s.fonts = fonts{
		hostname: s.face(16, faceHostname),
		// The all-caps faces are drawn in the "Poppins Vates" logotype subset,
		// which carries A-Z and nothing else: it is the brand's display face and
		// it fits these strings (tile labels, EVENTS, READY/STARTING/NOT READY).
		// Anything with lowercase or digits must use Poppins proper, or the
		// subset would fall back mid-word -- hence the split.
		label:   s.face(9, faceLabel),
		value:   s.face(baseValue, faceValue),
		note:    s.face(10, faceNote),
		pill:    s.face(14, faceLabel),
		section: s.face(12, faceLabel),
		event:   s.face(11, faceEvent),
		footer:  s.face(11, faceNote),
	}
}

// The dashboard's faces, as FILES. The image installs the fonts at these paths
// (recipes/console/dejavu.sh, poppins.sh), and every entry falls back to a
// DejaVu face so that a checkout, or a machine that does not carry Poppins,
// still draws text.
var (
	faceHostname = []string{"poppins/Poppins-Bold.ttf", "truetype/poppins/Poppins-Bold.ttf", "dejavu/DejaVuSans-Bold.ttf", "truetype/dejavu/DejaVuSans-Bold.ttf"}
	faceLabel    = []string{"poppins-vates/poppins_vates-webfont.ttf", "truetype/poppins/poppins_vates-webfont.ttf", "poppins/Poppins-Regular.ttf", "truetype/poppins/Poppins-Regular.ttf", "dejavu/DejaVuSans.ttf", "truetype/dejavu/DejaVuSans.ttf"}
	faceValue    = []string{"poppins/Poppins-SemiBold.ttf", "truetype/poppins/Poppins-SemiBold.ttf", "dejavu/DejaVuSans.ttf", "truetype/dejavu/DejaVuSans.ttf"}
	faceNote     = []string{"poppins/Poppins-Regular.ttf", "truetype/poppins/Poppins-Regular.ttf", "dejavu/DejaVuSans.ttf", "truetype/dejavu/DejaVuSans.ttf"}
	faceEvent    = []string{"dejavu/DejaVuSansMono.ttf", "truetype/dejavu/DejaVuSansMono.ttf", "dejavu/DejaVuSans.ttf", "truetype/dejavu/DejaVuSans.ttf"}
)

// face builds a face whose size follows the screen. The sizes above are the ones
// the dashboard was drawn at; scale grows or shrinks them with the display. The
// floor of one keeps a tiny screen from asking for a size of zero.
func (s *screen) face(size float64, candidates []string) render.Font {
	pt := size * s.scale
	if pt < 1 {
		pt = 1
	}
	paths := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if p := render.FindFont(name); p != "" {
			paths = append(paths, p)
		}
	}
	return render.LoadFirstFont(pt, paths...)
}

// loadLogo reads the wordmark, if it is where the caller says it might be. A
// missing logo costs a picture, not the screen.
func (s *screen) loadLogo(dirs []string) {
	if path := findAsset(dirs, "vates-name-baseline-white.png"); path != "" {
		s.logo = render.PNG(path)
	}
}

// The dashboard is authored for a 1024x768 screen: every length in layout is a
// pixel value at that size. The machines it runs on do not agree on a
// resolution -- a QEMU/qxl console offers 1024x768, Xen Orchestra's Cirrus VGA
// offers 800x600 or less -- so the whole layout is scaled to whatever the
// display gives us. The resolution is READ, not guessed: drm.Open takes the
// connected connector's preferred mode, and the screen carries it as s.w/s.h.
const (
	refW = 1024
	refH = 768

	// The horizontal rhythm of a tile, at the reference resolution: the padding
	// around the grid, the gap between tiles, and the inset of a value inside
	// its tile. They are named because the room a value has -- and so whether it
	// is ellipsised -- is these against the size of its font (baseValue).
	refPadding   = 26
	refTileGap   = 10
	refTileInset = 14

	// baseValue is the value font's size at the reference resolution.
	baseValue = 13
)

// scaleFor is the uniform factor mapping the reference layout onto a screen.
// The SMALLER of the two ratios is taken on purpose: it keeps the fonts small
// enough for the tiles. Extra width is absorbed by the tiles and extra height by
// the feed, never the other way round, so a value can never outgrow its tile.
func scaleFor(w, h int) float64 {
	k := math.Min(float64(w)/refW, float64(h)/refH)
	if k <= 0 {
		return 1
	}
	return k
}

// u scales a length authored at the reference resolution to this screen.
func (s *screen) u(v float64) int { return int(math.Round(v * s.scale)) }

// layout computes every rectangle from the screen's own size, so that the
// console fits whatever mode the machine offers rather than a resolution it
// hopes for. Every fixed length is scaled by s.scale; the ratios between them
// are what the dashboard's design is, and they are preserved exactly.
func (s *screen) layout() {
	s.padding = s.u(refPadding)
	s.brandH = s.u(6)
	s.headerTop = s.brandH + s.u(12)
	s.headerH = s.u(40)
	s.tilesTop = s.headerTop + s.headerH + s.u(14)
	s.tileH = s.u(60)
	s.tileGap = s.u(refTileGap)
	rows := (len(dashboard.TileLabels) + cardColumns - 1) / cardColumns
	tilesBottom := s.tilesTop + rows*s.tileH + (rows-1)*s.tileGap

	s.sectionTop = tilesBottom + s.u(16)
	s.panelX = s.padding
	s.panelY = s.sectionTop + s.u(18)
	s.panelW = s.w - 2*s.padding

	s.padH = s.u(14)
	s.padV = s.u(10)
	s.footerTop = s.h - s.u(20)
	s.panelH = s.footerTop - s.u(12) - s.panelY

	s.feedX, s.feedY = s.panelX, s.panelY
	s.feedW, s.feedH = s.panelW, s.panelH
	s.feedInnerW = s.feedW - 2*s.padH
	s.feedInnerH = s.feedH - 2*s.padV

	// A wheel notch is three lines, an arrow one, a page ten -- the device layer
	// says how many lines, and this is the size of one.
	_, s.lineH = s.ctx.MeasureSize(s.fonts.event, "X", 0)
	if s.lineH <= 0 {
		s.lineH = s.u(18)
	}
}

// apply takes a frame into the model and keeps the reader where they were.
//
// Keeping them means knowing WHICH line they are on, not at what offset: a
// refresh can add events above the one being read, drop the oldest, or -- with
// the mock, whose every stamp is rebuilt -- replace the whole feed with one that
// shares no key at all. An offset is a number that means something only in the
// feed it was taken in. That is why a reader who scrolled to the middle was
// thrown back to an end on every refresh: the number was kept, and the content
// under it had moved.
func (s *screen) apply(f frame) {
	s.snap, s.info, s.err = f.snapshot, f.info, f.err
	s.absorb(f.snapshot.Events)
	s.refreshFeed()
}

// refreshFeed brings the laid-out lines in line with the history, and puts the
// reader back where they were. Two paths lead here -- a collected frame and a
// streamed event -- and both owe the reader the same thing: the line they were
// reading, by its identity, still on screen.
func (s *screen) refreshFeed() {
	anchorKey, anchorDelta := s.topAnchor()
	dropped, aligned := s.rebuildFeed()

	switch {
	case s.follow:
		s.offset = s.maxOffset()
	case anchorKey != "":
		if y, ok := s.offsetOf(anchorKey); ok {
			s.offset = y + anchorDelta
		} else if aligned {
			// The line being read has left the feed; what remains of the old
			// feed did shift, so the offset follows the shift.
			s.offset -= dropped
		}
		// Otherwise nothing is known about where the reader is: leave them.
	default:
		if aligned {
			s.offset -= dropped
		}
	}
	s.clampOffset()
}

// absorb adds the events the collector has not shown before, and keeps them.
//
// The snapshot is a WINDOW, not the story: it is bounded and it is replaced on
// every refresh. A feed built straight from it throws away its oldest events as
// fast as new ones arrive -- invisible at the bottom, but at the top, where a
// reader has deliberately gone, it means the ground moves under them. What has
// been seen is kept here, deduplicated by identity, and only the far end is let
// go when the feed is long.
func (s *screen) absorb(events []kubeapi.Event) {
	for _, e := range events {
		key := dashboard.EventKey(e)
		if s.seen[key] {
			continue
		}
		s.seen[key] = true
		s.history = append(s.history, e)
	}
	if excess := len(s.history) - feedMax; excess > 0 {
		for _, e := range s.history[:excess] {
			delete(s.seen, dashboard.EventKey(e))
		}
		n := copy(s.history, s.history[excess:])
		s.history = s.history[:n]
	}
}

// topAnchor is the line the viewport starts in, and how far into it.
func (s *screen) topAnchor() (string, int) {
	for i := range s.lines {
		l := &s.lines[i]
		if l.y+l.height > s.offset {
			return l.key, s.offset - l.y
		}
	}
	return "", 0
}

// offsetOf is where a line begins in the current feed, by its key.
func (s *screen) offsetOf(key string) (int, bool) {
	for i := range s.lines {
		if s.lines[i].key == key {
			return s.lines[i].y, true
		}
	}
	return 0, false
}

func (s *screen) clampOffset() {
	if s.offset < 0 {
		s.offset = 0
	}
	if m := s.maxOffset(); s.offset > m {
		s.offset = m
	}
}

// watchSignals installs the diagnostic probe, the same one the windowed console
// has, for the same reason: the machine it has to describe is the one nobody can
// reach. SIGUSR1 writes what is on the screen -- the raw framebuffer, as a PPM
// the host can read -- and what the feed believes it is showing.
//
// It is here rather than in a separate tool because the interesting screen is
// the one that is wrong, and a tool that has to be started alongside it would
// not be there when that happens.
func (s *screen) watchSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			s.dump()
		}
	}()
}

func (s *screen) dump() {
	const path = "/tmp/vates-console-screen.ppm"
	content := s.contentHeight()
	// The identity of the line at the top of the viewport is what says whether
	// the reading is still: an offset is a number in coordinates that move, a
	// key is the thing the reader is looking at.
	top := ""
	for i := range s.lines {
		if s.lines[i].y+s.lines[i].height > s.offset {
			top = s.lines[i].key
			break
		}
	}
	if len(top) > 42 {
		top = top[:42]
	}
	fmt.Fprintf(os.Stderr, "console: screen %s: offset %d of %d, %d lines, follow=%v, top=%q\n",
		path, s.offset, content-s.feedInnerH, len(s.lines), s.follow, top)

	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "console: dump: %v\n", err)
		return
	}
	defer func() { _ = f.Close() }() // a debug dump: the image is written or the write already reported its error
	if _, err := fmt.Fprintf(f, "P6\n%d %d\n255\n", s.w, s.h); err != nil {
		fmt.Fprintf(os.Stderr, "console: dump: %v\n", err)
		return
	}

	mem := s.display.Pixels()
	stride := s.display.Stride()
	row := make([]byte, s.w*3)
	for y := 0; y < s.h; y++ {
		off := y * stride
		if off+s.w*4 > len(mem) {
			break
		}
		// The screen is XRGB8888 little-endian: three bytes, blue first. The
		// PPM wants red first.
		for x := 0; x < s.w; x++ {
			row[x*3+0] = mem[off+x*4+2]
			row[x*3+1] = mem[off+x*4+1]
			row[x*3+2] = mem[off+x*4+0]
		}
		if _, err := f.Write(row); err != nil {
			fmt.Fprintf(os.Stderr, "console: dump: %v\n", err)
			return
		}
	}
}

// fillBackground paints the whole screen black.
func (s *screen) fillBackground() {
	s.ctx.RGB(0, 0, 0)
	s.ctx.Rect(0, 0, float64(s.w), float64(s.h))
	s.ctx.Fill()
}

// present writes the frame from our own image into the screen buffer and tells
// the kernel it changed. gg rasterizes into an image.RGBA, not into the mapped
// memory, so Flush is what actually puts pixels on the screen.
func (s *screen) present() {
	s.surface.Flush()
	s.display.Present()
}

// presentRect is present for a rectangle: only that part is copied and declared
// changed, which is what keeps a scroll from handing the whole framebuffer back
// to the host.
func (s *screen) presentRect(x, y, w, h int) {
	s.surface.FlushRect(x, y, w, h)
	s.display.PresentRect(x, y, w, h)
}

// findAsset looks for a file in the directories given, in order.
func findAsset(dirs []string, name string) string {
	for _, dir := range dirs {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}
