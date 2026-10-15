package render

import (
	"image"
	"math"

	"github.com/fogleman/gg"
	xdraw "golang.org/x/image/draw"
)

// Context is the drawing state: the current colour, the current path, and the
// rectangular clip. It wraps gg for the shapes and carries the text layout on
// top.
type Context struct {
	dc  *gg.Context
	img *image.RGBA

	// clip is the one kind of clipping this console uses: a rectangle. It is
	// applied to text by drawing into a sub-image of the frame, which the font
	// drawer clips at its bounds for free. Shapes drawn while it is set are the
	// band's own fill, which is the clip, so they need no help.
	clip   *image.Rectangle
	colour uint32
}

// New prepares a context on a surface.
func New(s Surface) *Context {
	return &Context{
		dc:  gg.NewContextForRGBA(s.img),
		img: s.img,
	}
}

func (c *Context) Free() {}

// RGB makes a colour the current one, for both shapes and the default colour of
// text that does not carry its own.
func (c *Context) RGB(r, g, b float64) {
	c.colour = uint32(math.Round(r*255))<<16 | uint32(math.Round(g*255))<<8 | uint32(math.Round(b*255))
	c.dc.SetRGB(r, g, b)
}

// --- shapes -----------------------------------------------------------------

// Rect adds a rectangle to the path, for Fill or Clip.
func (c *Context) Rect(x, y, w, h float64) { c.dc.DrawRectangle(x, y, w, h) }

func (c *Context) Fill() { c.dc.Fill() }

// Rounded is a rectangle with round corners. The radius is clamped the way the
// previous renderer clamped it: a radius larger than half a side is not a shape.
func (c *Context) Rounded(x, y, w, h, r float64) {
	if r > h/2 {
		r = h / 2
	}
	if r > w/2 {
		r = w / 2
	}
	if r <= 0 {
		c.dc.DrawRectangle(x, y, w, h)
		return
	}
	c.dc.DrawRoundedRectangle(x, y, w, h, r)
}

// Stroke draws the outline of the current path at the given width.
func (c *Context) Stroke(width float64) {
	c.dc.SetLineWidth(width)
	c.dc.Stroke()
}

func (c *Context) MoveTo(x, y float64) { c.dc.MoveTo(x, y) }
func (c *Context) LineTo(x, y float64) { c.dc.LineTo(x, y) }

// ClosePath closes the current path, which a filled arrow needs so that its
// stroke does not run the closing line twice.
func (c *Context) ClosePath() { c.dc.ClosePath() }

// --- clip -------------------------------------------------------------------

// ClipRect restricts what text draws to a rectangle, until ResetClip.
func (c *Context) ClipRect(x, y, w, h float64) {
	r := image.Rect(
		int(math.Round(x)), int(math.Round(y)),
		int(math.Round(x+w)), int(math.Round(y+h)),
	)
	r = r.Intersect(c.img.Bounds())
	c.clip = &r
}

// ResetClip removes the clip.
func (c *Context) ResetClip() { c.clip = nil }

// textDst is where text is painted: the frame, or the clipped part of it.
func (c *Context) textDst() *image.RGBA {
	if c.clip == nil {
		return c.img
	}
	sub, ok := c.img.SubImage(*c.clip).(*image.RGBA)
	if !ok {
		return c.img
	}
	return sub
}

// --- text -------------------------------------------------------------------

// Text lays out markup, wrapping inside wrapWidth when it is given one, and
// returns the height it took. The text is markup: colour spans are honoured,
// anything without one uses the current colour.
func (c *Context) Text(f Font, markup string, x, y, wrapWidth int) int {
	if !f.Valid() {
		return 0
	}
	cells := flatten(parseMarkup(markup, c.colour))
	lines := f.wrap(cells, float64(wrapWidth))
	dst := c.textDst()
	for i, ln := range lines {
		baseline := float64(y) + f.ascent + float64(i)*f.lineHeight()
		f.drawCells(dst, ln, float64(x), baseline, c.colour)
	}
	return int(math.Round(float64(len(lines)) * f.lineHeight()))
}

// TextLine draws ONE line, cut off with an ellipsis rather than wrapped or left
// to run off the edge: what a tile value and a hostname need.
func (c *Context) TextLine(f Font, markup string, x, y, width int) int {
	if !f.Valid() {
		return 0
	}
	cells := flatten(parseMarkup(markup, c.colour))
	if width > 0 {
		cells = f.ellipsize(cells, float64(width))
	}
	f.drawCells(c.textDst(), cells, float64(x), float64(y)+f.ascent, c.colour)
	return int(math.Ceil(f.lineHeight()))
}

// TextCentered draws ONE line, ellipsised at width, centred horizontally in
// [x, x+w] and vertically on the rectangle's middle.
//
// It centres the INK, not the line box. The logotype subset's ascent overflows
// its line height, so a baseline placed by the box -- the pill used to do that
// -- leaves the capitals low in the pill.
func (c *Context) TextCentered(f Font, markup string, x, y, w, h int) {
	if !f.Valid() {
		return
	}
	cells := flatten(parseMarkup(markup, c.colour))
	if w > 0 {
		cells = f.ellipsize(cells, float64(w))
	}
	tw := f.measureCells(cells)
	baseline := float64(y) + float64(h)/2
	if top, bottom, ok := f.inkBounds(cells); ok {
		baseline -= (top + bottom) / 2
	} else {
		baseline += f.ascent - f.lineHeight()/2
	}
	f.drawCells(c.textDst(), cells, float64(x)+(float64(w)-tw)/2, baseline, c.colour)
}

// Measure is how tall a run of markup will be, at the given wrap width.
func (c *Context) Measure(f Font, markup string, wrapWidth int) int {
	_, h := c.MeasureSize(f, markup, wrapWidth)
	return h
}

// MeasureSize is both dimensions of a laid-out run of markup.
func (c *Context) MeasureSize(f Font, markup string, wrapWidth int) (int, int) {
	if !f.Valid() {
		return 0, 0
	}
	cells := flatten(parseMarkup(markup, c.colour))
	lines := f.wrap(cells, float64(wrapWidth))
	w := 0.0
	for _, ln := range lines {
		if lw := f.measureCells(ln); lw > w {
			w = lw
		}
	}
	h := float64(len(lines)) * f.lineHeight()
	return int(math.Ceil(w)), int(math.Ceil(h))
}

// DrawSurface places an image inside a box, scaling it without distorting it.
func (c *Context) DrawSurface(s Surface, x, y, w, h float64) {
	if !s.Valid() || w <= 0 || h <= 0 {
		return
	}
	sw, sh := float64(s.Width()), float64(s.Height())
	if sw <= 0 || sh <= 0 {
		return
	}
	scale := math.Min(w/sw, h/sh)
	dw, dh := sw*scale, sh*scale
	dr := image.Rect(
		int(math.Round(x)), int(math.Round(y)),
		int(math.Round(x+dw)), int(math.Round(y+dh)),
	)
	xdraw.CatmullRom.Scale(c.img, dr, s.img, s.img.Bounds(), xdraw.Over, nil)
}
