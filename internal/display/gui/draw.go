package gui

import (
	"fmt"
	"time"

	"github.com/vatesfr/vates-kube-os/internal/dashboard"
	"github.com/vatesfr/vates-kube-os/internal/display/gui/render"
)

// cardColumns is how many tiles fit on a row: three rows of four, the cluster,
// the machine, its load. Six on a row cut an address at 1024 pixels.
const cardColumns = 4

// The colours are the charte's, the same ones the stylesheet gives the windowed
// console. They are written as hex and converted here so that the two renderings
// can be compared line by line.
const (
	colourCream  = 0xfffce4
	colourPurple = 0x8f84ff
	colourRed    = 0xbe1621
	colourGreen  = 0x2ca878
	colourOrange = 0xf08019
	colourBlack  = 0x000000

	// The tile and panel fills are arithmetic on the palette, not taste: five
	// percent of the cream over black, and a quarter of the purple for the
	// tile's edge.
	colourTileFill  = 0x0d0d0b
	colourTileEdge  = 0x393440
	colourPanelFill = 0x080807
	colourPanelEdge = 0x1a182e
	colourWhen      = 0x737167
	colourNote      = 0x8c8b7d
	colourDim       = 0xbfbdab

	// The scrollbar's thumb sits between the panel's edge and the body text:
	// visible enough to find, quiet enough not to compete with an event.
	colourScrollThumb = 0x5a5470
)

// redraw paints everything. It is called on a refresh -- once every two seconds
// -- and not on a scroll: a scroll moves pixels, it does not redraw them.
func (s *screen) redraw() {
	s.fillBackground()
	s.drawBrand()
	s.drawHeader()
	s.drawTiles()
	s.drawSection()
	s.drawPanel()
	s.drawFeed()
	s.drawScrollbar()
	s.drawFooter()
	// A full repaint has just erased the arrow: whatever background was saved
	// under it is now the wrong background, so it is dropped rather than put
	// back, and the arrow is drawn again where it stands.
	if s.cursors != nil {
		s.cursors.saved = false
		s.drawCursor()
	}
}

// drawBrand is the accent bar at the very top, where the eye lands first.
func (s *screen) drawBrand() {
	s.setColour(colourRed)
	s.ctx.Rect(0, 0, float64(s.w), float64(s.brandH))
	s.ctx.Fill()
}

// drawHeader is the wordmark on the left, the node's name in the middle of the
// WINDOW, and the state on the right.
//
// The name is centred in the window rather than in the space between its two
// neighbours: with a mark and a pill of unequal width, "centred" drifts sideways
// as either changes, which reads as the header coming loose.
func (s *screen) drawHeader() {
	logoW := 0
	if s.logo.Valid() {
		logoW = s.headerH * 288 / 100 // the wordmark is 2.88 times wider than tall
		s.ctx.DrawSurface(s.logo, float64(s.padding), float64(s.headerTop), float64(logoW), float64(s.headerH))
	}

	// The pill first: its width is what the centre has to fit between.
	pillText, pillFill, pillInk := s.pillState()
	pw, ph := s.ctx.MeasureSize(s.fonts.pill, dashboard.MarkupEscape(pillText), 0)
	pillW, pillH := pw+s.u(32), ph+s.u(10)
	pillX := s.w - s.padding - pillW
	pillY := s.headerTop + (s.headerH-pillH)/2

	s.setColour(pillFill)
	s.ctx.Rounded(float64(pillX), float64(pillY), float64(pillW), float64(pillH), float64(pillH)/2)
	s.ctx.Fill()
	// Centred on the ink, not the line box: the logotype subset's ascent is
	// taller than its line height, and the box would leave the capitals low.
	s.ctx.TextCentered(s.fonts.pill, span(pillInk, pillText), pillX, pillY, pillW, pillH)

	name := s.snap.Node.Metadata.Name
	if name == "" {
		name = "waiting"
	}
	left := s.padding + logoW + s.u(24)
	right := pillX - s.u(24)
	if right-left < s.u(80) {
		// A screen too narrow for three things: the name is the one that can be
		// dropped, because the pill says the state and the mark says the brand.
		return
	}
	tw, _ := s.ctx.MeasureSize(s.fonts.hostname, dashboard.MarkupEscape(name), 0)
	x := left + (right-left-tw)/2
	x = max(x, left)
	s.textLineAt(s.fonts.hostname, colourCream, name, x, s.headerTop+(s.headerH-s.u(34))/2, right-x)
}

// pillState is the text and the colours of the state pill.
func (s *screen) pillState() (text string, fill, ink uint32) {
	switch {
	case s.snap.Node.Metadata.Name == "":
		return "STARTING", colourOrange, colourBlack
	case s.snap.Ready:
		return "READY", colourGreen, colourBlack
	default:
		return "NOT READY", colourRed, colourCream
	}
}

// drawTiles is the grid of labelled values.
func (s *screen) drawTiles() {
	tiles := s.snap.Tiles(s.info)
	labels := dashboard.TileLabels

	totalW := s.w - 2*s.padding
	tileW := (totalW - (cardColumns-1)*s.tileGap) / cardColumns
	inset := s.u(refTileInset)

	for i := range tiles {
		if i >= len(labels) {
			break
		}
		t := tiles[i]
		col, row := i%cardColumns, i/cardColumns
		x := s.padding + col*(tileW+s.tileGap)
		y := s.tilesTop + row*(s.tileH+s.tileGap)

		// The fill and then the hairline, the way the stylesheet does it: five
		// percent of the cream, a quarter of the purple for the edge.
		s.setColour(colourTileFill)
		s.ctx.Rounded(float64(x), float64(y), float64(tileW), float64(s.tileH), float64(s.u(10)))
		s.ctx.Fill()
		s.setColour(colourTileEdge)
		s.ctx.Rounded(float64(x)+0.5, float64(y)+0.5, float64(tileW)-1, float64(s.tileH)-1, float64(s.u(10)))
		s.ctx.Stroke(1)

		s.textAt(s.fonts.label, colourPurple, labels[i], x+inset, y+s.u(8), 0)

		// The value, and beside it the note when there is one: the percentage is
		// what is watched, the capacity is what it means. The note is bottom
		// aligned with the value, because centring two sizes on one line lifts
		// the small one visibly.
		noteW := 0
		if t.Note != "" {
			nw, _ := s.ctx.MeasureSize(s.fonts.note, dashboard.MarkupEscape(t.Note), 0)
			noteW = nw + s.u(6)
		}
		vy := y + s.u(24)
		vw, vh := s.ctx.MeasureSize(s.fonts.value, dashboard.MarkupEscape(t.Value), 0)
		avail := tileW - 2*inset - noteW
		if vw > avail {
			vw = avail
		}
		s.textLineAt(s.fonts.value, toneColour(t.Tone), t.Value, x+inset, vy, avail)
		if t.Note != "" {
			_, nh := s.ctx.MeasureSize(s.fonts.note, dashboard.MarkupEscape(t.Note), 0)
			s.textAt(s.fonts.note, colourNote, t.Note, x+inset+vw+s.u(6), vy+vh-nh, 0)
		}
	}
}

// drawSection is the heading above the feed.
func (s *screen) drawSection() {
	s.textAt(s.fonts.section, colourPurple, "EVENTS", s.padding, s.sectionTop, 0)
}

// drawPanel is the box the feed lives in.
func (s *screen) drawPanel() {
	s.setColour(colourPanelFill)
	s.ctx.Rounded(float64(s.panelX), float64(s.panelY), float64(s.panelW), float64(s.panelH), float64(s.u(10)))
	s.ctx.Fill()
	s.setColour(colourPanelEdge)
	s.ctx.Rounded(float64(s.panelX)+0.5, float64(s.panelY)+0.5, float64(s.panelW)-1, float64(s.panelH)-1, float64(s.u(10)))
	s.ctx.Stroke(1)
}

// drawFooter is the line of machine facts at the bottom.
func (s *screen) drawFooter() {
	uptime := s.info.Uptime.Round(time.Minute)
	hours, minutes := int(uptime.Hours()), int(uptime.Minutes())%60
	stamp := time.Now().Format("15:04:05")
	if s.err != nil && s.snap.Node.Metadata.Name != "" {
		stamp = "stale: " + s.err.Error()
	} else if s.err != nil {
		stamp = s.err.Error()
	}
	line := fmt.Sprintf("uptime %dh%02dm   ·   load %.2f   ·   %s   ·   updated %s",
		hours, minutes, s.info.Load[0], s.snap.Node.Status.NodeInfo.OSImage, stamp)
	// While the node is coming up, what it is DOING leads the footer: it is the
	// answer to the only question asked in front of a machine that is not Ready.
	// The line is hidden once the node reports Ready, so a stale phase cannot
	// outlive the wait it described.
	if p, ok := dashboard.ReadPhase(); ok && !s.snap.Ready {
		line = "booting: " + p.Line(time.Now()) + "   ·   " + line
	}
	s.textAt(s.fonts.footer, colourWhen, line, s.padding, s.footerTop, s.w-2*s.padding)
}

// --- text helpers -----------------------------------------------------------

// textAt draws plain text in a colour, wrapping inside width when it is given
// one, and returns the height it took.
func (s *screen) textAt(f render.Font, colour uint32, text string, x, y, width int) int {
	return s.ctx.Text(f, span(colour, text), x, y, width)
}

// textLineAt draws plain text on ONE line, ellipsised at the given width.
func (s *screen) textLineAt(f render.Font, colour uint32, text string, x, y, width int) int {
	return s.ctx.TextLine(f, span(colour, text), x, y, width)
}

// span is a coloured run of text, escaped so that a value containing an
// ampersand or an angle bracket stays data rather than becoming markup.
func span(colour uint32, text string) string {
	return `<span foreground="#` + hex(colour) + `">` + dashboard.MarkupEscape(text) + `</span>`
}

func hex(c uint32) string { return fmt.Sprintf("%06x", c) }

// setColour makes a palette entry the current drawing colour.
func (s *screen) setColour(c uint32) {
	s.ctx.RGB(float64((c>>16)&0xff)/255, float64((c>>8)&0xff)/255, float64(c&0xff)/255)
}

// toneColour maps a dashboard tone to the palette.
func toneColour(tone string) uint32 {
	switch tone {
	case dashboard.ToneGood:
		return colourGreen
	case dashboard.ToneWarn:
		return colourOrange
	case dashboard.ToneBad:
		return colourRed
	case dashboard.ToneDim:
		return colourDim
	default:
		return colourCream
	}
}
