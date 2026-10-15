package gui

import (
	"fmt"
	"os"

	"github.com/vatesfr/vates-kube-os/internal/dashboard"
)

// clickDebug traces the scrollbar grab, under the same switch the input layer
// uses: one environment variable to ask either side what it saw.
var clickDebug = os.Getenv("VATES_WHEEL_DEBUG") != ""

// feedMax bounds how many events the feed keeps. It is the console's own
// memory, not the API's window: the window is replaced on every refresh, and a
// feed built from it loses its oldest events as fast as they arrive. Two
// thousand events is roughly an hour of a busy node, which is longer than anyone
// reads; past that the oldest are dropped, from the far end.
const feedMax = 2000

// eventSpacing is the gap between two events, so the feed breathes. It is the
// value at the reference resolution; heightOf scales it with the screen.
const eventSpacing = 4

// line is one event laid out: its identity, its markup, its height, and where it
// begins in the feed's content coordinates.
type line struct {
	key    string
	markup string
	height int
	y      int
}

// emptyFeed is the single line shown when there is nothing to show, and why.
//
// A node that has never been seen is drawn calm even when a read failed: the
// console starts with the machine, before the API answers and before the
// kubelet has registered the node, so a failure here is the boot, not a fault.
// The raw error is still in the footer, where it is a fact rather than an alarm.
func (s *screen) emptyFeed() (string, uint32) {
	switch {
	case !s.hadNode && s.snap.Node.Metadata.Name == "":
		return "waiting for the API server", colourWhen
	case s.err != nil:
		return s.err.Error(), colourOrange
	default:
		return "none", colourWhen
	}
}

// buildLines lays out the window of events the feed shows.
//
// Heights are measured and remembered: an event's height only changes when the
// feed's width does, and measuring four hundred paragraphs on every refresh
// would be work done to learn nothing.
func (s *screen) buildLines() []line {
	tail := s.history
	if len(tail) > feedMax {
		tail = tail[len(tail)-feedMax:]
	}

	if len(tail) == 0 {
		text, colour := s.emptyFeed()
		key := "\x00" + text
		markup := dashboard.Span("#"+hex(colour), text)
		return []line{{key: key, markup: markup, height: s.heightOf(key, markup, s.feedInnerW)}}
	}

	lines := make([]line, 0, len(tail))
	y := 0
	for _, e := range tail {
		key := dashboard.EventKey(e)
		markup := dashboard.EventMarkup(e, s.snap.BootEvent(e))
		h := s.heightOf(key, markup, s.feedInnerW)
		lines = append(lines, line{key: key, markup: markup, height: h, y: y})
		y += h
	}
	return lines
}

// heightOf measures one line, with a memory.
//
// The memory is bounded: an event that has left the feed will never be asked
// about again, and a console left running for weeks must not remember every
// message it has ever shown. When it grows past a few feeds' worth it is simply
// dropped -- measuring a paragraph is cheap, and forgetting is cheaper than
// leaking.
func (s *screen) heightOf(key, markup string, width int) int {
	if h, ok := s.heights[key]; ok {
		return h
	}
	if len(s.heights) > 4*feedMax {
		s.heights = make(map[string]int)
	}
	h := s.ctx.Measure(s.fonts.event, markup, width) + s.u(eventSpacing)
	s.heights[key] = h
	return h
}

// rebuildFeed brings the lines in line with what the feed should show, and
// reports how many pixels left the top -- the reader's offset has to move by
// them, or the message they are reading would slide out from under them.
//
// Events only ever arrive at the end and leave from the start, so the change is
// a shift and the middle is untouched. The fallback is a full rebuild, for the
// one case that is not a shift.
func (s *screen) rebuildFeed() (dropped int, aligned bool) {
	fresh := s.buildLines()
	drop, ok := alignKeys(s.lines, fresh)
	if !ok {
		s.lines = fresh
		return 0, false
	}
	for i := range drop {
		dropped += s.lines[i].height
	}
	s.lines = fresh
	return dropped, true
}

// alignKeys finds how many lines left the top, and says whether what remains of
// the old lines is the head of the new ones.
//
// It is the only place in the feed that has to know the shape of a change, so it
// is also the only place that can be wrong: when it says "not a shift", the feed
// is rebuilt and the reader loses nothing but their anchor.
func alignKeys(old, fresh []line) (int, bool) {
	if len(fresh) == 0 {
		return len(old), true
	}
	if len(old) == 0 {
		return 0, true
	}
	// An empty remainder is deliberately excluded: an empty sequence is a prefix
	// of everything, so accepting it would report "aligned" for a completely
	// different feed and hand the caller a rebuild disguised as a shift.
	for drop := range len(old) {
		keep := old[drop:]
		if len(keep) > len(fresh) {
			continue
		}
		aligned := true
		for i := range keep {
			if keep[i].key != fresh[i].key {
				aligned = false
				break
			}
		}
		if aligned {
			return drop, true
		}
	}
	return 0, false
}

// maxOffset is how far the feed can scroll before its last line reaches the
// bottom of the panel.
func (s *screen) maxOffset() int {
	if content := s.contentHeight(); content > s.feedInnerH {
		return content - s.feedInnerH
	}
	return 0
}

// contentHeight is the whole laid-out feed, in pixels.
func (s *screen) contentHeight() int {
	if len(s.lines) == 0 {
		return 0
	}
	last := s.lines[len(s.lines)-1]
	return last.y + last.height
}

// The scrollbar's shape, at the reference resolution. Drawing and hit-testing
// both read it -- scaled with the screen -- from scrollbarGeometry below, so the
// bar someone aims at is the bar that was drawn, not a second copy of the
// arithmetic free to drift from it.
const (
	scrollbarW        = 6
	scrollbarInset    = 4
	scrollbarMinThumb = 24
)

// scrollbarGeometry is where the bar is and where its thumb is. ok is false when
// everything fits: a bar with nothing to indicate is not drawn and cannot be
// grabbed.
func (s *screen) scrollbarGeometry() (x, trackY, trackH, thumbY, thumbH int, ok bool) {
	content := s.contentHeight()
	x = s.feedX + s.feedW - s.padH + s.u(scrollbarInset)
	trackY = s.feedY + s.padV
	trackH = s.feedInnerH
	if content <= s.feedInnerH {
		return x, trackY, trackH, 0, 0, false
	}
	thumbH = trackH * s.feedInnerH / content
	thumbH = max(thumbH, s.u(scrollbarMinThumb))
	thumbH = min(thumbH, trackH)
	pos := 0
	if m := s.maxOffset(); m > 0 {
		pos = s.offset * (trackH - thumbH) / m
	}
	return x, trackY, trackH, trackY + pos, thumbH, true
}

// drawScrollbar draws the indicator at the panel's right edge: where the reader
// is, and how much there is.
//
// It repaints its own strip before drawing, because the thumb MOVES: leaving the
// old one behind is the classic scrollbar trail. The strip is a few pixels wide,
// so this costs nothing to do on every scroll.
func (s *screen) drawScrollbar() {
	x, trackY, trackH, thumbY, thumbH, ok := s.scrollbarGeometry()
	barW := s.u(scrollbarW)

	// The strip, cleared to the panel's own fill.
	s.setColour(colourPanelFill)
	s.ctx.Rect(float64(x), float64(trackY), float64(barW), float64(trackH))
	s.ctx.Fill()
	if !ok {
		return
	}

	s.setColour(colourPanelEdge)
	s.ctx.Rounded(float64(x), float64(trackY), float64(barW), float64(trackH), float64(barW)/2)
	s.ctx.Fill()

	s.setColour(colourScrollThumb)
	s.ctx.Rounded(float64(x), float64(thumbY), float64(barW), float64(thumbH), float64(barW)/2)
	s.ctx.Fill()
}

// grabScrollbar takes hold of the bar when the left button goes down on it, and
// does nothing otherwise: a click anywhere else on the console is not a command.
func (s *screen) grabScrollbar(px, py int) {
	x, trackY, trackH, thumbY, thumbH, ok := s.scrollbarGeometry()
	if clickDebug {
		fmt.Fprintf(os.Stderr, "console: click at %d,%d; bar x=%d track %d..%d thumb %d..%d ok=%v\n",
			px, py, x, trackY, trackY+trackH, thumbY, thumbY+thumbH, ok)
	}
	if !ok {
		return
	}
	if px < x || px >= x+s.u(scrollbarW) || py < trackY || py >= trackY+trackH {
		return
	}
	if py >= thumbY && py < thumbY+thumbH {
		// On the thumb: keep the hold where it was taken, or the thumb jumps
		// under the pointer and the drag begins with a jolt.
		s.dragGrab = py - thumbY
	} else {
		// On the track: bring the thumb here, centred on the click -- what every
		// scrollbar does, and what makes a page jump possible.
		s.dragGrab = thumbH / 2
		s.dragTo(py - s.dragGrab)
	}
	s.dragging = true
}

// dragTo puts the top of the thumb at a screen row and scrolls the feed to
// match. It is the inverse of the arithmetic that places the thumb.
func (s *screen) dragTo(thumbTop int) {
	_, trackY, trackH, _, thumbH, ok := s.scrollbarGeometry()
	if !ok {
		return
	}
	span := trackH - thumbH
	if span <= 0 {
		return
	}
	off := (thumbTop - trackY) * s.maxOffset() / span
	off = max(off, 0)
	if m := s.maxOffset(); off > m {
		off = m
	}
	s.scroll(off - s.offset)
}

// releaseScrollbar ends the drag. Dragging to the bottom leaves the feed
// following, which is what a reader who dragged there wants.
func (s *screen) releaseScrollbar() { s.dragging = false }

// drawFeed draws the whole visible window of the feed, and its indicator.
func (s *screen) drawFeed() {
	s.drawFeedBand(0, s.feedInnerH)
}

// drawFeedBand draws the lines that intersect a horizontal band of the panel,
// measured from the panel's interior top. The band is what a scroll has just
// exposed; drawing the band and not the panel is the whole of the saving.
//
// The band is FILLED with the panel's own colour before the lines go in. A
// scroll moves pixels, so the strip a move exposes holds a copy of whatever was
// next to it -- text that is now in the wrong place. Drawing the new lines over
// that leaves their ghosts in every gap, and because a scroll is a burst of
// small steps the ghosts accumulate: what an operator sees is a panel full of
// overlapping text. Filling first costs one rectangle.
func (s *screen) drawFeedBand(y0, y1 int) {
	if y1 <= y0 {
		return
	}
	top, bottom := s.offset+y0, s.offset+y1

	x0 := float64(s.feedX + s.padH)
	y0f := float64(s.feedY + s.padV + y0)
	w := float64(s.feedInnerW)
	h := float64(y1 - y0)
	s.ctx.ClipRect(x0, y0f, w, h)
	s.setColour(colourPanelFill)
	s.ctx.Rect(x0, y0f, w, h)
	s.ctx.Fill()

	x := s.feedX + s.padH
	for i := range s.lines {
		l := &s.lines[i]
		if l.y+l.height <= top {
			continue
		}
		if l.y >= bottom {
			break
		}
		s.ctx.Text(s.fonts.event, l.markup, x, s.feedY+s.padV+l.y-s.offset, s.feedInnerW)
	}
	s.ctx.ResetClip()
}

// scroll moves the feed by delta pixels: positive is towards the newest.
//
// This is the expensive path made cheap. The pixels already on the screen are
// moved by copying scanlines, and only the strip that the move exposes is drawn
// -- a few lines at one edge. Nothing above or below is laid out again.
func (s *screen) scroll(delta int) {
	if delta == 0 || s.feedInnerH <= 0 {
		return
	}
	old := s.offset
	s.offset += delta
	s.clampOffset()
	// Being at the bottom IS following; leaving it is what stops the feed
	// pushing new events under the reader's eyes.
	s.follow = s.offset >= s.maxOffset()

	// shift says which way the CONTENT slides on the screen. offset is the
	// topmost content coordinate the panel shows, so a SMALLER offset shows
	// earlier events -- and moves the content DOWN. Getting this backwards is
	// invisible while the feed is following (a reshape, not a copy, is what puts
	// it at the bottom) and obvious the moment a hand touches the wheel: the bar
	// moves one way and the text the other.
	shift := old - s.offset // positive: towards the older events, content slides down
	if shift == 0 {
		return
	}

	// The feed is about to move UNDER the arrow. Put the arrow back first, or the
	// copy below would carry its pixels along with the text -- a stale cursor
	// trail.
	s.restoreCursor()

	exposed := shift
	if exposed < 0 {
		exposed = -exposed
	}
	if exposed >= s.feedInnerH {
		// A jump longer than the panel has nothing to copy: redraw it whole.
		s.drawFeed()
		s.drawScrollbar()
		s.drawCursor()
		s.presentRect(s.panelX, s.panelY, s.panelW, s.panelH)
		return
	}

	// The pixels already drawn are moved inside our own image, and only the
	// strip the move exposes is laid out again.
	if shift > 0 {
		// Content down: every row takes what was above it, and the top is
		// exposed.
		s.surface.MoveRows(s.feedX+s.padH, s.feedY+s.padV, s.feedInnerW, s.feedInnerH-exposed, exposed)
	} else {
		// Content up: every row takes what was below it, and the bottom is
		// exposed.
		s.surface.MoveRows(s.feedX+s.padH, s.feedY+s.padV+exposed, s.feedInnerW, s.feedInnerH-exposed, -exposed)
	}

	if shift > 0 {
		s.drawFeedBand(0, exposed)
	} else {
		s.drawFeedBand(s.feedInnerH-exposed, s.feedInnerH)
	}
	s.drawScrollbar()
	s.drawCursor()
	// Only the panel changed: the tiles, the header and the footer did not, and
	// saying so is what keeps the virtual display from handing the whole screen
	// back to the host on every notch. The arrow, when it is over the panel, is
	// inside this rectangle; when it is not, restoring and redrawing it left its
	// pixels exactly as they were.
	s.presentRect(s.panelX, s.panelY, s.panelW, s.panelH)
}
