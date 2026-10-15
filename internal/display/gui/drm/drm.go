package drm

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// --- the kernel's structures -------------------------------------------------
//
// Mirrored from drm_mode.h. The layout IS the ABI here: a field out of place and
// the kernel answers something else entirely, without an error -- which is
// exactly how the old ADDFB ioctl behaves below.

type modeRes struct {
	CountFbs        int32
	_               int32
	Fbs             unsafe.Pointer
	CountCrtcs      int32
	_               int32
	Crtcs           unsafe.Pointer
	CountConnectors int32
	_               int32
	Connectors      unsafe.Pointer
	CountEncoders   int32
	_               int32
	Encoders        unsafe.Pointer
	MinWidth        uint32
	MaxWidth        uint32
	MinHeight       uint32
	MaxHeight       uint32
}

type modeInfo struct {
	Clock      uint32
	Hdisplay   uint16
	HsyncStart uint16
	HsyncEnd   uint16
	Htotal     uint16
	Hskew      uint16
	Vdisplay   uint16
	VsyncStart uint16
	VsyncEnd   uint16
	Vtotal     uint16
	Vscan      uint16
	Vrefresh   uint32
	Flags      uint32
	Type       uint32
	Name       [32]byte
}

type connector struct {
	ConnectorID     uint32
	EncoderID       uint32
	ConnectorType   uint32
	ConnectorTypeID uint32
	Connection      uint32
	MmWidth         uint32
	MmHeight        uint32
	Subpixel        uint32
	CountModes      int32
	_               int32
	Modes           unsafe.Pointer
	CountProps      int32
	_               int32
	Props           unsafe.Pointer
	PropValues      unsafe.Pointer
	CountEncoders   int32
	_               int32
	Encoders        unsafe.Pointer
}

type encoder struct {
	EncoderID      uint32
	EncoderType    uint32
	CrtcID         uint32
	PossibleCrtcs  uint32
	PossibleClones uint32
}

type createDumb struct {
	Height uint32
	Width  uint32
	Bpp    uint32
	Flags  uint32
	Handle uint32
	Pitch  uint32
	Size   uint64
}

type mapDumb struct {
	Handle uint32
	Pad    uint32
	Offset uint64
}

type fbCmd struct {
	FBID   uint32
	Width  uint32
	Height uint32
	Pitch  uint32
	Bpp    uint32
	Depth  uint32
	Handle uint32
}

// fbCmd2 is the framebuffer interface modern drivers want, with an explicit
// pixel format.
type fbCmd2 struct {
	FBID        uint32
	Width       uint32
	Height      uint32
	PixelFormat uint32
	Flags       uint32
	Handles     [4]uint32
	Pitches     [4]uint32
	Offsets     [4]uint32
	Modifier    [4]uint64
}

type fbDirtyCmd struct {
	FBID     uint32
	Flags    uint32
	Color    uint32
	NumClips uint32
	Clips    uint64
}

// clipRect is one rectangle of the screen to refresh, x2 and y2 exclusive.
// Two bytes per coordinate, which is why the header cannot be taller than 65535
// -- true of any screen this runs on.
type clipRect struct {
	X1, Y1, X2, Y2 uint16
}

// ioctl requests. _IOWR(dir, type, nr, size) = 3<<30 | size<<16 | type<<8 | nr,
// with 'd' as the DRM type.
//
// CreateDumb, MapDumb, AddFB2 and DirtyFB are called DIRECTLY rather than
// through libdrm's wrappers. Measured on this machine: drmModeCreateDumbBuffer
// segfaulted, and the old drmModeAddFB answered success while writing nothing --
// see the note on AddFB2 below. An ioctl is a constant and a structure, so it is
// cheaper to say it here than to trust a layer that lies.
const (
	ioctlSetMaster  = uintptr(0x641e) // _IO('d', 0x1e)
	ioctlCreateDumb = uintptr(3<<30 | 32<<16 | 0x64<<8 | 0xb2)
	ioctlMapDumb    = uintptr(3<<30 | 16<<16 | 0x64<<8 | 0xb3)
	ioctlAddFB      = uintptr(3<<30 | 28<<16 | 0x64<<8 | 0xb5)
	ioctlAddFB2     = uintptr(3<<30 | 104<<16 | 0x64<<8 | 0xb8)
	ioctlDirtyFB    = uintptr(3<<30 | 24<<16 | 0x64<<8 | 0xb1)

	formatXRGB8888 = uint32(0x34325258) // fourcc 'X', 'R', '2', '4'
)

var (
	drm                uintptr
	getResources       func(int32) unsafe.Pointer
	getConnector       func(int32, uint32) unsafe.Pointer
	getEncoder         func(int32, uint32) unsafe.Pointer
	setCrtc            func(int32, uint32, uint32, uint32, uint32, unsafe.Pointer, int32, *modeInfo) int32
	freeResources      func(unsafe.Pointer)
	freeConnector      func(unsafe.Pointer)
	freeEncoder        func(unsafe.Pointer)
	errNoConnectedMode = fmt.Errorf("drm: no connected screen")
)

func ensure() {
	loadDRM.Do(func() {
		h, err := purego.Dlopen("libdrm.so.2", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			panic(fmt.Sprintf("drm: libdrm: %v", err))
		}
		drm = h
		purego.RegisterLibFunc(&getResources, drm, "drmModeGetResources")
		purego.RegisterLibFunc(&getConnector, drm, "drmModeGetConnector")
		purego.RegisterLibFunc(&getEncoder, drm, "drmModeGetEncoder")
		purego.RegisterLibFunc(&setCrtc, drm, "drmModeSetCrtc")
		purego.RegisterLibFunc(&freeResources, drm, "drmModeFreeResources")
		purego.RegisterLibFunc(&freeConnector, drm, "drmModeFreeConnector")
		purego.RegisterLibFunc(&freeEncoder, drm, "drmModeFreeEncoder")
	})
}

// loadDRM is run once, on the first screen open, NOT in an init(): this package
// is linked into the one binary the image carries, which also runs as the
// kubelet launcher in a container that has no libdrm. An eager load made that
// container panic before it could say anything.
var loadDRM sync.Once

func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// Display is the screen, with a buffer to draw in.
type Display struct {
	mem    []byte
	fd     int
	width  int
	height int
	stride int
	crtc   uint32
	fb     uint32
}

// Open takes the screen.
//
// The device must be free: on a machine where the console is already running,
// stop it first. That is what the unit does.
func Open(device string) (*Display, error) {
	ensure()
	fd, err := syscall.Open(device, syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("drm: opening %s: %w", device, err)
	}
	// Being master is what allows the mode ioctls. A machine with nothing else
	// running has nobody else to ask.
	_ = ioctl(fd, ioctlSetMaster, nil)

	res := (*modeRes)(getResources(int32(fd)))
	if res == nil {
		_ = syscall.Close(fd) // cleanup on an error path: the error worth reporting is the one above
		return nil, fmt.Errorf("drm: reading the resources of %s failed", device)
	}
	defer freeResources(unsafe.Pointer(res))

	var (
		connID    uint32
		mode      modeInfo
		encoderID uint32
	)
	for i := 0; i < int(res.CountConnectors); i++ {
		id := *(*uint32)(unsafe.Add(res.Connectors, uintptr(i)*4))
		c := (*connector)(getConnector(int32(fd), id))
		if c == nil {
			continue
		}
		if c.Connection == 1 && c.CountModes > 0 {
			connID = id
			mode = *(*modeInfo)(unsafe.Add(c.Modes, 0)) // the first mode is the preferred one
			encoderID = c.EncoderID
			freeConnector(unsafe.Pointer(c))
			break
		}
		freeConnector(unsafe.Pointer(c))
	}
	if connID == 0 {
		_ = syscall.Close(fd) // cleanup on an error path: the error worth reporting is the one above
		return nil, errNoConnectedMode
	}

	crtc := uint32(0)
	if enc := (*encoder)(getEncoder(int32(fd), encoderID)); enc != nil {
		crtc = enc.CrtcID
		freeEncoder(unsafe.Pointer(enc))
	}
	if crtc == 0 && res.CountCrtcs > 0 {
		crtc = *(*uint32)(unsafe.Add(res.Crtcs, 0))
	}

	d := &Display{fd: fd, crtc: crtc, width: int(mode.Hdisplay), height: int(mode.Vdisplay)}

	dumb := createDumb{Width: uint32(d.width), Height: uint32(d.height), Bpp: 32}
	if err := ioctl(fd, ioctlCreateDumb, unsafe.Pointer(&dumb)); err != nil {
		d.Close()
		return nil, fmt.Errorf("drm: creating the buffer: %w", err)
	}
	d.stride = int(dumb.Pitch)

	offset := mapDumb{Handle: dumb.Handle}
	if err := ioctl(fd, ioctlMapDumb, unsafe.Pointer(&offset)); err != nil {
		d.Close()
		return nil, fmt.Errorf("drm: mapping the buffer: %w", err)
	}
	d.mem, err = syscall.Mmap(fd, int64(offset.Offset), int(dumb.Size),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("drm: mmap: %w", err)
	}

	// The width and height come from US, not from the structure the ioctl
	// returned: the kernel zeroes height in that one, and a framebuffer of no
	// height is refused without a word -- the id comes back 0, and the next call
	// then fails with "unknown CRTC", which says nothing about the real cause.
	//
	// AddFB2 rather than AddFB: measured here, the old request answers success
	// and writes nothing at all, so its result is a framebuffer with no id.
	_ = ioctl(fd, ioctlAddFB, unsafe.Pointer(&fbCmd{
		Width: uint32(d.width), Height: uint32(d.height), Pitch: uint32(d.stride),
		Bpp: 32, Depth: 24, Handle: dumb.Handle,
	}))
	cmd := fbCmd2{
		Width: uint32(d.width), Height: uint32(d.height),
		PixelFormat: formatXRGB8888,
		Handles:     [4]uint32{dumb.Handle},
		Pitches:     [4]uint32{uint32(d.stride)},
	}
	if err := ioctl(fd, ioctlAddFB2, unsafe.Pointer(&cmd)); err != nil {
		d.Close()
		return nil, fmt.Errorf("drm: publishing the framebuffer: %w", err)
	}
	d.fb = cmd.FBID

	connectors := []uint32{connID}
	if r := setCrtc(int32(fd), d.crtc, d.fb, 0, 0, unsafe.Pointer(&connectors[0]), 1, &mode); r != 0 {
		d.Close()
		return nil, fmt.Errorf("drm: setting the mode: error %d", r)
	}
	return d, nil
}

func (d *Display) Width() int  { return d.width }
func (d *Display) Height() int { return d.height }
func (d *Display) Stride() int { return d.stride }

// Memory is the buffer the controller reads: the console's image is blitted
// straight into it.
func (d *Display) Memory() unsafe.Pointer { return unsafe.Pointer(&d.mem[0]) }

// Pixels is the whole buffer, row by row, with the padding the stride leaves.
func (d *Display) Pixels() []byte { return d.mem }

// Present tells the kernel that the buffer changed, so the screen is refreshed.
// Without it the machine keeps showing the frame it was given.
func (d *Display) Present() {
	cmd := fbDirtyCmd{FBID: d.fb}
	_ = ioctl(d.fd, ioctlDirtyFB, unsafe.Pointer(&cmd))
}

// PresentRect is Present for a rectangle: only that part of the screen is
// declared changed.
//
// It matters because of what is on the OTHER side of this call. Marking the
// whole screen dirty asks the virtual display to hand the whole framebuffer back
// to the host, every time -- measured on this machine with a fast wheel, qemu on
// the host peaked at 279 % of a core, where the console itself peaked at 99 %.
// The console pays for its own drawing; the host pays for the transfer, and
// telling it that half the screen did not move is half the work.
//
// The driver is free to ignore the rectangles -- they are a hint, not a
// contract -- but the ones that do not ignore them are exactly the ones with a
// framebuffer to re-upload.
func (d *Display) PresentRect(x, y, w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	clip := clipRect{
		X1: uint16(x),
		Y1: uint16(y),
		X2: uint16(x + w),
		Y2: uint16(y + h),
	}
	cmd := fbDirtyCmd{
		FBID:     d.fb,
		NumClips: 1,
		Clips:    uint64(uintptr(unsafe.Pointer(&clip))),
	}
	_ = ioctl(d.fd, ioctlDirtyFB, unsafe.Pointer(&cmd))
}

// MoveRows copies a band of the screen by dy scanlines: what is at y arrives at
// y+dy, for height rows, across the x..x+width columns.
//
// This is what scrolling costs here: a memory copy, with no layout, no widget
// and no toolkit. The direction matters -- a downward move has to start from the
// last row or it overwrites what it has not read yet -- so both are written out
// rather than trusted to a single loop with a clever index.
//
// The caller is responsible for the bounds; a band that does not fit is a bug,
// not a case.
func (d *Display) MoveRows(x, y, width, height, dy int) {
	if dy == 0 || width <= 0 || height <= 0 {
		return
	}
	rowBytes := width * 4
	switch {
	case dy > 0:
		for i := height - 1; i >= 0; i-- {
			src := (y+i)*d.stride + x*4
			dst := (y+i+dy)*d.stride + x*4
			copy(d.mem[dst:dst+rowBytes], d.mem[src:src+rowBytes])
		}
	default:
		for i := range height {
			src := (y+i)*d.stride + x*4
			dst := (y+i+dy)*d.stride + x*4
			copy(d.mem[dst:dst+rowBytes], d.mem[src:src+rowBytes])
		}
	}
}

func (d *Display) Close() {
	if d.mem != nil {
		_ = syscall.Munmap(d.mem)
		d.mem = nil
	}
	if d.fd > 0 {
		_ = syscall.Close(d.fd)
		d.fd = 0
	}
}

// --- input -------------------------------------------------------------------
//
// A kiosk console has two ways in: the wheel, and the keyboard. Reading the
// input devices directly is a handful of lines and needs no seat, no compositor
// and no library. What comes out is a signed number of TEXT LINES to move --
// positive towards the older events -- because that is the unit the feed thinks
// in, and the device layer has no business knowing about fonts.

type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

const (
	evKey = 0x01
	evRel = 0x02
	evAbs = 0x03

	relX     = 0x00 // pointer motion, relative
	relY     = 0x01
	relWheel = 0x08 // one notch
	relHiRes = 0x0b // finer deltas, on devices that have them

	absX = 0x00 // pointer position, absolute
	absY = 0x01

	btnLeft = 0x110 // the left button, in EV_KEY space

	keyUp       = 103
	keyPageUp   = 104
	keyDown     = 108
	keyPageDown = 109
	keyHome     = 102
	keyEnd      = 107

	eventSize = 24 // struct input_event, 64-bit
)

// How far one input moves the feed, in lines. A wheel notch is two lines, which
// is what this machine's operator found comfortable -- three was a notch too
// fast; an arrow is one; a page is ten, about what fits on the screen this was
// written for.
const (
	LinesWheel = 2
	LinesArrow = 1
	LinesPage  = 10
)

// Wheel is the console's input. It is called a Wheel because that was the first
// thing it listened to, not because it only listens to one.
type Wheel struct {
	steps chan int
	done  chan struct{}
	mu    sync.Mutex
	files []*os.File
	open  map[string]bool

	// The pointer. A relative mouse does not say where something IS; it says how
	// far it moved, so the position is accumulated here, clamped to the screen,
	// and drawn by the console. There is no cursor plane, no compositor and no
	// display server to hold one.
	sw, sh  int
	mx, my  int
	pointer bool
	motion  chan struct{}

	// The left button, for grabbing the scrollbar. Presses and releases, in
	// order: a grab is a sequence, not a state to be sampled.
	buttons chan bool
}

// OpenWheel listens to every input device, and keeps listening for new ones.
//
// It used to scan once, at startup. On a machine whose keyboard and mouse exist
// at boot that is enough -- and it is exactly why a device that appears later,
// like the one a test fabricates, was silently ignored: the reader was holding
// the list the machine had when the console started, and a list is not a
// subscription. The rescan is one directory read a second.
// wheelDebug turns on a trace of what the input layer sees. It is read once: an
// environment variable does not change while a console runs.
var wheelDebug = os.Getenv("VATES_WHEEL_DEBUG") != ""

func OpenWheel(screenWidth, screenHeight int) *Wheel {
	w := &Wheel{
		steps:   make(chan int, 32),
		done:    make(chan struct{}),
		open:    make(map[string]bool),
		sw:      screenWidth,
		sh:      screenHeight,
		mx:      screenWidth / 2,
		my:      screenHeight / 2,
		motion:  make(chan struct{}, 1),
		buttons: make(chan bool, 8),
	}
	w.scan()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				w.scan()
			}
		}
	}()
	return w
}

func (w *Wheel) scan() {
	entries, err := os.ReadDir("/dev/input")
	if err != nil {
		if wheelDebug {
			fmt.Fprintf(os.Stderr, "input: reading /dev/input: %v\n", err)
		}
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "event") {
			continue
		}
		w.mu.Lock()
		if w.open[name] {
			w.mu.Unlock()
			continue
		}
		f, err := os.OpenFile("/dev/input/"+name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			if wheelDebug {
				fmt.Fprintf(os.Stderr, "input: opening %s: %v\n", name, err)
			}
			w.mu.Unlock()
			continue
		}
		w.open[name] = true
		w.files = append(w.files, f)
		w.mu.Unlock()
		if wheelDebug {
			fmt.Fprintf(os.Stderr, "input: opened /dev/input/%s\n", name)
		}
		// A tablet is told apart from a mouse by ASKING, not by its name: an
		// absolute axis with a range answers, a relative one does not.
		var abs *absAxes
		if xi, ok := absRange(f, absX); ok {
			if yi, ok2 := absRange(f, absY); ok2 {
				abs = &absAxes{x: xi, y: yi}
				if wheelDebug {
					fmt.Fprintf(os.Stderr, "input: %s is absolute, x %d..%d y %d..%d\n",
						name, xi.Minimum, xi.Maximum, yi.Minimum, yi.Maximum)
				}
			}
		}
		go w.read(name, f, abs)
	}
}

// absAxes is the range of a tablet's two axes.
type absAxes struct{ x, y inputAbsinfo }

// forget drops a device whose kernel object is gone.
//
// The "already open" set is keyed by the node's NAME, and names are REUSED: when
// a device disappears and another appears, the second usually takes the same
// /dev/input/eventN. Keyed by name alone, the reader then holds "event7 is
// open", sees the new device under event7 and skips it -- so a mouse unplugged
// and plugged back in never works again, and the console says nothing. That is
// not a test artifact: it is a keyboard swapped on a machine nobody can log
// into. The kernel says ENODEV on the dead file descriptor; that is the signal,
// and this is what acts on it.
func (w *Wheel) forget(name string, f *os.File) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.open[name] {
		return
	}
	delete(w.open, name)
	for i, g := range w.files {
		if g == f {
			w.files = append(w.files[:i], w.files[i+1:]...)
			break
		}
	}
	_ = f.Close()
	if wheelDebug {
		fmt.Fprintf(os.Stderr, "input: /dev/input/%s went away\n", name)
	}
}

func (w *Wheel) read(name string, f *os.File, abs *absAxes) {
	buf := make([]byte, eventSize*16)
	acc := 0
	// The fine-delta accumulator lives with the reader: one device, one
	// accumulator, nothing shared between goroutines.
	hiRes := 0
	for {
		select {
		case <-w.done:
			return
		default:
		}
		n, err := f.Read(buf[acc:])
		if n > 0 {
			acc += n
			for acc >= eventSize {
				ev := (*inputEvent)(unsafe.Pointer(&buf[0]))
				switch ev.Type {
				case evRel:
					switch ev.Code {
					case relX:
						w.move(int(ev.Value), 0)
					case relY:
						w.move(0, int(ev.Value))
					case relWheel:
						w.send(int(ev.Value) * LinesWheel)
					case relHiRes:
						// A high-resolution wheel sends many small deltas for
						// one notch; accumulate until they make one.
						hiRes += int(ev.Value)
						for hiRes >= 120 {
							hiRes -= 120
							w.send(LinesWheel)
						}
						for hiRes <= -120 {
							hiRes += 120
							w.send(-LinesWheel)
						}
					}
				case evAbs:
					// A tablet, reporting where the pointer IS. The two axes
					// arrive in the same SYN group, so the other one is taken
					// from the position as it stands; setting X then Y lands on
					// the right place, and reading the other axis costs nothing.
					if abs == nil {
						break
					}
					switch ev.Code {
					case absX:
						_, y, _ := w.Position()
						w.moveTo(scale(abs.x, ev.Value, w.sw), y)
					case absY:
						x, _, _ := w.Position()
						w.moveTo(x, scale(abs.y, ev.Value, w.sh))
					}
				case evKey:
					// The button is a key, and its RELEASE matters as much as
					// its press, so it is taken before the "ignore releases"
					// rule below -- which exists for the keyboard's auto-repeat.
					if ev.Code == btnLeft {
						w.button(ev.Value != 0)
						break
					}
					if ev.Value == 0 { // released
						break
					}
					switch ev.Code {
					case keyUp:
						w.send(LinesArrow)
					case keyDown:
						w.send(-LinesArrow)
					case keyPageUp:
						w.send(LinesPage)
					case keyPageDown:
						w.send(-LinesPage)
					case keyHome:
						w.send(1 << 20) // far enough to reach the top
					case keyEnd:
						w.send(-(1 << 20))
					}
				}
				copy(buf, buf[eventSize:acc])
				acc -= eventSize
			}
		}
		if err != nil {
			// ENODEV is the one error that means "this device is gone", and it
			// must not be slept through: the node will be reused by the next
			// device to appear, and a reader that keeps the name in its open set
			// would ignore it for ever.
			if errors.Is(err, syscall.ENODEV) {
				w.forget(name, f)
				return
			}
			// EAGAIN is the normal answer on a non-blocking device between two
			// events: wait a little rather than spin.
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func (w *Wheel) send(lines int) {
	if lines == 0 {
		return
	}
	if os.Getenv("VATES_WHEEL_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "input: %d lines\n", lines)
	}
	select {
	case w.steps <- lines:
	default: // the reader is behind: dropping a step beats blocking the device
	}
}

// Steps is the channel of scroll amounts, in text lines, positive towards the
// older events. It is never closed.
func (w *Wheel) Steps() <-chan int { return w.steps }

// Position is where the pointer is, and whether anything has moved it yet. A
// console with no mouse never draws a cursor: an arrow nobody can move is one
// more thing on a screen that has to be read from a photograph.
func (w *Wheel) Position() (int, int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mx, w.my, w.pointer
}

// Motion is signalled when the pointer moves. It is a signal, not a queue: what
// matters is where the pointer IS, not every place it went.
func (w *Wheel) Motion() <-chan struct{} { return w.motion }

// Buttons carries the left button's presses (true) and releases (false), in
// order. A grab is a sequence -- press, move, release -- and losing the release
// would leave the console dragging for ever.
func (w *Wheel) Buttons() <-chan bool { return w.buttons }

func (w *Wheel) button(down bool) {
	if wheelDebug {
		fmt.Fprintf(os.Stderr, "input: button %v\n", down)
	}
	select {
	case w.buttons <- down:
	default: // nobody is listening: dropping beats blocking the device
	}
}

// inputAbsinfo is struct input_absinfo: the range a device reports for one axis.
// A tablet does not say "two pixels left", it says "x = 4133", and 4133 has no
// meaning until this says what the axis runs from and to.
type inputAbsinfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

// absRange asks a device what an absolute axis runs between. A device without
// the axis answers with an error, which is how a relative mouse is told from a
// tablet without guessing from its name.
func absRange(f *os.File, code uintptr) (inputAbsinfo, bool) {
	const (
		iocRead = 2
		nrBase  = 0x40 // EVIOCGABS(abs)
		size    = 24   // struct input_absinfo
	)
	request := uintptr(iocRead<<30 | size<<16 | 'E'<<8 | (nrBase + code))
	var info inputAbsinfo
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(unsafe.Pointer(&info)))
	if errno != 0 || info.Maximum <= info.Minimum {
		return inputAbsinfo{}, false
	}
	return info, true
}

// scale maps an absolute axis reading onto a pixel count.
func scale(info inputAbsinfo, value int32, pixels int) int {
	span := info.Maximum - info.Minimum
	if span <= 0 || pixels <= 0 {
		return 0
	}
	p := int((value - info.Minimum) * int32(pixels-1) / span)
	if p < 0 {
		return 0
	}
	if p > pixels-1 {
		return pixels - 1
	}
	return p
}

// moveTo places the pointer, for a device that knows where it is.
func (w *Wheel) moveTo(x, y int) {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x > w.sw-1 {
		x = w.sw - 1
	}
	if y > w.sh-1 {
		y = w.sh - 1
	}
	w.mu.Lock()
	w.mx, w.my, w.pointer = x, y, true
	w.mu.Unlock()
	select {
	case w.motion <- struct{}{}:
	default:
	}
}

// move takes one relative motion event and clamps the result to the screen.
// A relative mouse pushed past an edge reports motion that cannot be honoured;
// without the clamp the accumulated position runs away and the cursor is lost.
func (w *Wheel) move(dx, dy int) {
	w.mu.Lock()
	w.mx += dx
	w.my += dy
	if w.mx < 0 {
		w.mx = 0
	}
	if w.my < 0 {
		w.my = 0
	}
	if w.mx > w.sw-1 {
		w.mx = w.sw - 1
	}
	if w.my > w.sh-1 {
		w.my = w.sh - 1
	}
	w.pointer = true
	w.mu.Unlock()
	select {
	case w.motion <- struct{}{}:
	default:
	}
}

func (w *Wheel) Close() {
	close(w.done)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range w.files {
		_ = f.Close()
	}
}
