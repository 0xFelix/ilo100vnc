package ilo

import (
	"encoding/binary"
	"errors"
	"image"
	"sync"
)

// Blit types, see docs/PROTOCOL.md 6.2 and 6.3.
const (
	bltCopy           = 0x100
	bltTextFirst      = 0x101
	bltTextCombo      = 0x108
	bltEnhanceRaw     = 0x1F0
	bltEnhanceHLC     = 0x1F2
	bltSpecial4bpp    = 0x1F3
	bltNewFrameMarker = 0x8000

	textASCII = 1
	textAttr  = 2
	textFont  = 4

	glyphBytes = 32
	maxDim     = 4096

	// smallest graphics mode (VGA mode 0x13); see VesaMode.Usable
	minGraphicsW = 320
	minGraphicsH = 200
)

var errShortData = errors.New("blit data too short")

// Framebuffer holds the decoded remote screen as 0x00RRGGBB pixels.
// All methods are safe for concurrent use.
type Framebuffer struct {
	mu sync.Mutex

	w, h   int
	mode   int
	srcBPP int // bpp as announced by InformVesaMode
	text   bool

	pix   []uint32 // final RGB
	index []byte   // palette index per pixel (indexed modes)
	attr4 []byte   // 4-bit color value per pixel (text and 4bpp modes)

	palette      [256][3]byte
	attrs        [64]byte
	validPalette bool
	colorIndex   int
	special4bpp  bool

	// text mode state, kept per screen cell
	fontW, fontH int
	cols, rows   int
	chars, cattr []byte
	attrValid    bool
	font         [65536]byte
	curCol       int // -1 = hidden
	curRow       int
	curStart     int
	curStop      int
	blinking     bool // blink attribute enabled (bit 7 of attr = blink)
	blinkOn      bool // blink phase: cursor shown, blinking text hidden
	dbe          bool
	primaryOff   int
	secondaryOff int

	cursor *MatroxCursor
}

// NewFramebuffer returns a black 640x480 16bpp framebuffer, the viewer's
// initial state.
func NewFramebuffer() *Framebuffer {
	f := &Framebuffer{curCol: -1, blinkOn: true}
	f.defaultPalette()
	f.setMode(-1, 640, 480, 16)
	return f
}

func (f *Framebuffer) defaultPalette() {
	for i := range f.palette {
		f.palette[i] = [3]byte{
			byte(((i&7)*255 + 3) / 7),
			byte(((i>>3&7)*255 + 3) / 7),
			byte(((i>>6&3)*255 + 1) / 3),
		}
	}
	for i := range f.attrs {
		f.attrs[i] = byte(i)
	}
}

// Size returns the framebuffer dimensions.
func (f *Framebuffer) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, f.h
}

// Read copies the pixels of r (clipped to the framebuffer) row by row into
// dst, which must hold r.Dx()*r.Dy() entries. It returns the clipped rect.
func (f *Framebuffer) Read(r image.Rectangle, dst []uint32) image.Rectangle {
	f.mu.Lock()
	defer f.mu.Unlock()
	r = r.Intersect(image.Rect(0, 0, f.w, f.h))
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		n += copy(dst[n:], f.pix[y*f.w+r.Min.X:y*f.w+r.Max.X])
	}
	return r
}

// Cursor returns the last hardware cursor image sent by the server, or nil.
func (f *Framebuffer) Cursor() *MatroxCursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor
}

// Update is the result of applying a message.
type Update struct {
	Dirty   image.Rectangle // area that changed, empty if none
	Resized bool            // dimensions changed
}

// Apply updates the framebuffer with a decoded server message. Messages
// that do not affect the screen are ignored.
func (f *Framebuffer) Apply(msg any) (Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch m := msg.(type) {
	case VesaMode:
		return f.setMode(int(m.Mode), int(m.Width), int(m.Height), int(m.BPP)), nil
	case StandbyPower:
		u := f.setMode(-1, 640, 480, 16)
		clear(f.pix)
		u.Dirty = f.bounds()
		return u, nil
	case SetPalette:
		return f.setPalette(m), nil
	case BitBlt:
		return f.bitBlt(m)
	case EnhanceBitBlt:
		return f.enhanceBitBlt(m)
	case TextCursor:
		return f.setTextCursor(m), nil
	case SpecialGraphicsBit:
		return f.specialBits(m), nil
	case *MatroxCursor:
		f.cursor = m
	}
	return Update{}, nil
}

func (f *Framebuffer) bounds() image.Rectangle { return image.Rect(0, 0, f.w, f.h) }

// FullRegion is the region the viewer invalidates after a mode change:
// pixels in graphics modes, character cells in text modes.
func (f *Framebuffer) FullRegion() Region {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.text {
		return Region{W: int16(f.cols), H: int16(f.rows)}
	}
	return Region{W: int16(f.w), H: int16(f.h)}
}

func isTextMode(mode int) (cols, rows int, ok bool) {
	switch mode {
	case 0, 2, 3, 7:
		return 80, 25, true
	case 1:
		return 40, 25, true
	case 0x10A:
		return 132, 43, true
	}
	return 0, 0, false
}

// Usable reports whether the mode can be displayed. During some graphics
// modes (seen: the POST screen on firmware 4.26) the iLO reports nonsense
// like mode 0x200, 8x1 pixels, and sends palettes but no picture.
func (m VesaMode) Usable() bool {
	if m.Width <= 0 || m.Height <= 0 || m.Width > maxDim || m.Height > maxDim {
		return false
	}
	if _, _, text := isTextMode(int(m.Mode)); text {
		return true
	}
	return m.Width >= minGraphicsW && m.Height >= minGraphicsH
}

func (f *Framebuffer) setMode(mode, w, h, bpp int) Update {
	if w <= 0 || h <= 0 || w > maxDim || h > maxDim {
		return Update{}
	}
	cols, rows, text := isTextMode(mode)
	if f.pix != nil && mode == f.mode && bpp == f.srcBPP && w == f.w && h == f.h && text == f.text {
		return Update{}
	}
	resized := w != f.w || h != f.h
	f.mode, f.srcBPP, f.w, f.h, f.text = mode, bpp, w, h, text
	f.special4bpp = false
	f.pix = make([]uint32, w*h)
	f.index = make([]byte, w*h)
	f.attr4 = make([]byte, w*h)

	if text {
		f.fontW, f.fontH = 8, 16
		f.setGrid(cols, rows)
		f.curCol = -1
		if !f.validPalette {
			f.palette[0] = [3]byte{0, 0, 0}
			f.palette[1] = [3]byte{255, 255, 255}
			f.attrs[0], f.attrs[1] = 0, 1
		}
	} else if bpp < 15 && !f.validPalette {
		f.defaultPalette()
	}
	f.refreshIndexed(f.bounds())
	return Update{Dirty: f.bounds(), Resized: resized}
}

func (f *Framebuffer) setGrid(cols, rows int) {
	if cols == f.cols && rows == f.rows && f.chars != nil {
		return
	}
	f.cols, f.rows = cols, rows
	f.chars = make([]byte, cols*rows)
	f.cattr = make([]byte, cols*rows)
}

// --- color helpers ---

func (f *Framebuffer) indexed() bool { return f.text || f.srcBPP <= 8 }

func (f *Framebuffer) rgb(idx byte) uint32 {
	p := f.palette[idx]
	return uint32(p[0])<<16 | uint32(p[1])<<8 | uint32(p[2])
}

// attrToIndex maps a 4-bit value to a palette index (PROTOCOL.md 6.4).
func (f *Framebuffer) attrToIndex(v byte) byte {
	p := f.attrs[(int(v)|f.colorIndex<<4)&63]
	if f.special4bpp && f.attrs[16]&0x80 != 0 {
		p = p&0xCF | (f.attrs[20]&3)<<4
	}
	return p
}

func (f *Framebuffer) setIndex(i int, idx byte) {
	f.index[i] = idx
	f.pix[i] = f.rgb(idx)
}

func (f *Framebuffer) setAttr(i int, v byte) {
	f.attr4[i] = v
	f.setIndex(i, f.attrToIndex(v))
}

// refreshIndexed recomputes pixels of r after a palette/attribute change.
func (f *Framebuffer) refreshIndexed(r image.Rectangle) {
	if !f.indexed() {
		return
	}
	viaAttr := f.text || f.srcBPP < 8
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			i := y*f.w + x
			if viaAttr {
				f.setAttr(i, f.attr4[i])
			} else {
				f.setIndex(i, f.index[i])
			}
		}
	}
}

func rgb565(v uint32) uint32 {
	r, g, b := v>>11&0x1F, v>>5&0x3F, v&0x1F
	return (r<<3|r>>2)<<16 | (g<<2|g>>4)<<8 | (b<<3 | b>>2)
}

func rgb555(v uint32) uint32 {
	r, g, b := v>>10&0x1F, v>>5&0x1F, v&0x1F
	return (r<<3|r>>2)<<16 | (g<<3|g>>2)<<8 | (b<<3 | b>>2)
}

// direct converts a little-endian wire pixel value to RGB.
func (f *Framebuffer) direct(v uint32) uint32 {
	switch f.srcBPP {
	case 15:
		return rgb555(v & 0xFFFF)
	case 16:
		return rgb565(v & 0xFFFF)
	}
	return v & 0xFFFFFF
}

// bytesPerPixel on the wire for direct and 8bpp modes.
func (f *Framebuffer) bytesPerPixel() int {
	switch {
	case f.srcBPP <= 8:
		return 1
	case f.srcBPP <= 16:
		return 2
	case f.srcBPP == 24:
		return 3
	}
	return 4
}

func pixelAt(data []byte, off, n int) uint32 {
	var v uint32
	for i := 0; i < n && i < 3; i++ {
		v |= uint32(data[off+i]) << (8 * i)
	}
	return v
}

// --- palette ---

func (f *Framebuffer) setPalette(m SetPalette) Update {
	f.validPalette = true
	for _, a := range m.Attributes {
		f.attrs[a.Index&63] = a.Value
	}
	for _, c := range m.Colors {
		f.palette[c.Index] = [3]byte{c.R, c.G, c.B}
	}
	if !f.indexed() {
		return Update{}
	}
	f.refreshIndexed(f.bounds())
	return Update{Dirty: f.bounds()}
}

// --- BitBlt ---

func (f *Framebuffer) bitBlt(m BitBlt) (Update, error) {
	t := int(m.Type)
	switch {
	case t == bltCopy:
		f.special4bpp = false
		return f.blitRect(m)
	case t == bltSpecial4bpp:
		f.special4bpp = true
		return f.blitRect(m)
	case t >= bltTextFirst && t <= bltTextCombo:
		if !f.text {
			return Update{}, nil
		}
		f.special4bpp = false
		return f.blitText(m)
	}
	return Update{}, nil
}

func (f *Framebuffer) blitRect(m BitBlt) (Update, error) {
	x0, y0, w, h := int(m.Dst.X), int(m.Dst.Y), int(m.Dst.W), int(m.Dst.H)
	if w <= 0 || h <= 0 {
		return Update{}, nil
	}
	d := m.Data
	switch {
	case f.srcBPP < 8:
		// nibbles; within a row k counts from w-1 down to 0,
		// even k takes the high nibble
		if len(d) < (w*h+1)/2 {
			return Update{}, errShortData
		}
		i := 0
		for y := range h {
			for k := w - 1; k >= 0; k-- {
				v := d[i>>1]
				if k&1 == 0 {
					v >>= 4
				}
				f.putAttr(x0+(w-1-k), y0+y, v&0x0F)
				i++
			}
		}
	case f.srcBPP == 8:
		if len(d) < w*h {
			return Update{}, errShortData
		}
		for y := range h {
			for x := range w {
				f.putIndex(x0+x, y0+y, d[y*w+x])
			}
		}
	default:
		bpp := f.bytesPerPixel()
		if len(d) < w*h*bpp {
			return Update{}, errShortData
		}
		for y := range h {
			for x := range w {
				f.putRGB(x0+x, y0+y, f.direct(pixelAt(d, (y*w+x)*bpp, bpp)))
			}
		}
	}
	return Update{Dirty: image.Rect(x0, y0, x0+w, y0+h).Intersect(f.bounds())}, nil
}

func (f *Framebuffer) inside(x, y int) bool { return x >= 0 && y >= 0 && x < f.w && y < f.h }

func (f *Framebuffer) putAttr(x, y int, v byte) {
	if f.inside(x, y) {
		f.setAttr(y*f.w+x, v)
	}
}

func (f *Framebuffer) putIndex(x, y int, v byte) {
	if f.inside(x, y) {
		f.setIndex(y*f.w+x, v)
	}
}

func (f *Framebuffer) putRGB(x, y int, v uint32) {
	if f.inside(x, y) {
		f.pix[y*f.w+x] = v
	}
}

// --- text mode ---

func (f *Framebuffer) blitText(m BitBlt) (Update, error) {
	if m.FontWidth > 0 && m.FontHeight > 0 && m.FontHeight <= glyphBytes &&
		(int(m.FontWidth) != f.fontW || int(m.FontHeight) != f.fontH) {
		f.fontW, f.fontH = int(m.FontWidth), int(m.FontHeight)
		f.setGrid(max(f.w/f.fontW, 1), max(f.h/f.fontH, 1))
	}
	x0, y0, w, h := int(m.Dst.X), int(m.Dst.Y), int(m.Dst.W), int(m.Dst.H)
	if w <= 0 || h <= 0 {
		return Update{}, nil
	}
	d := m.Data
	cell := func(i int) (int, bool) {
		col, row := x0+i%w, y0+i/w
		if col < 0 || row < 0 || col >= f.cols || row >= f.rows {
			return 0, false
		}
		return row*f.cols + col, true
	}
	if int(m.Type) == bltTextCombo {
		f.attrValid = true
		for i := 0; i < w*h && 2*i+1 < len(d); i++ {
			if c, ok := cell(i); ok {
				f.chars[c], f.cattr[c] = d[2*i], d[2*i+1]
			}
		}
	} else {
		off := 0
		section := func(dst []byte) error {
			if len(d)-off < w*h {
				return errShortData
			}
			for i := range w * h {
				if c, ok := cell(i); ok {
					dst[c] = d[off+i]
				}
			}
			off += w * h
			return nil
		}
		if m.Type&textASCII != 0 {
			if err := section(f.chars); err != nil {
				return Update{}, err
			}
		}
		if m.Type&textAttr != 0 {
			f.attrValid = true
			if err := section(f.cattr); err != nil {
				return Update{}, err
			}
		}
		if m.Type&textFont != 0 {
			copy(f.font[:], d[off:])
		}
	}
	for row := y0; row < y0+h; row++ {
		for col := x0; col < x0+w; col++ {
			f.renderCell(col, row)
		}
	}
	r := image.Rect(x0*f.fontW, y0*f.fontH, (x0+w)*f.fontW, (y0+h)*f.fontH)
	return Update{Dirty: r.Intersect(f.bounds())}, nil
}

// renderCell draws one character cell (PROTOCOL.md 6.3) in the current
// blink phase: the cursor and blinking text alternate, as in the original
// viewer.
func (f *Framebuffer) renderCell(col, row int) {
	if col < 0 || row < 0 || col >= f.cols || row >= f.rows {
		return
	}
	c := row*f.cols + col
	glyph := int(f.chars[c])
	fg, bg := byte(1), byte(0)
	if f.attrValid {
		a := f.cattr[c]
		fg, bg = a&0x0F, a>>4&0x0F
		if f.dbe {
			fg &= 0x07
			if a&0x08 != 0 {
				glyph += f.primaryOff
			} else {
				glyph += f.secondaryOff
			}
		}
		if f.blinking {
			bg &= 0x07
			if a&0x80 != 0 && f.blinkOn {
				fg = bg
			}
		}
	}
	glyph &= 2047
	isCursor := f.blinkOn && f.curCol == col && f.curRow == row
	for s := range f.fontH {
		bits := f.font[glyph*glyphBytes+s]
		cur := isCursor && s >= f.curStart && s <= f.curStop
		y := row*f.fontH + s
		for px := range f.fontW {
			// MSB first; with fonts wider than 8 the extra leading
			// columns stay background, as in the original viewer
			b := f.fontW - 1 - px
			on := cur || (b < 8 && bits>>b&1 != 0)
			v := bg
			if on {
				v = fg
			}
			f.putAttr(col*f.fontW+px, y, v)
		}
	}
}

// ToggleBlink switches the text blink phase (cursor and blinking text).
// The original viewer does this every 500 ms in text mode.
func (f *Framebuffer) ToggleBlink() Update {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.text {
		return Update{}
	}
	f.blinkOn = !f.blinkOn
	var dirty image.Rectangle
	if f.curCol >= 0 {
		f.renderCell(f.curCol, f.curRow)
		dirty = f.cellRect(f.curCol, f.curRow)
	}
	if f.blinking && f.attrValid {
		for c, a := range f.cattr {
			if a&0x80 != 0 {
				col, row := c%f.cols, c/f.cols
				f.renderCell(col, row)
				dirty = dirty.Union(f.cellRect(col, row))
			}
		}
	}
	return Update{Dirty: dirty}
}

// Blank clears the screen to black at the current size, used while the
// iLO reports a mode it cannot capture.
func (f *Framebuffer) Blank() Update {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.pix)
	clear(f.index)
	clear(f.attr4)
	clear(f.chars)
	clear(f.cattr)
	f.curCol = -1
	return Update{Dirty: f.bounds()}
}

func (f *Framebuffer) cellRect(col, row int) image.Rectangle {
	return image.Rect(col*f.fontW, row*f.fontH, (col+1)*f.fontW, (row+1)*f.fontH).Intersect(f.bounds())
}

func (f *Framebuffer) setTextCursor(m TextCursor) Update {
	if !f.text {
		return Update{}
	}
	col, row := int(m.Col), int(m.Row)
	if col >= f.cols || row >= f.rows || (m.Start != m.Stop && int(m.Stop) >= f.fontH) {
		return Update{}
	}
	oldCol, oldRow := f.curCol, f.curRow
	dirty := image.Rectangle{}
	if oldCol >= 0 {
		dirty = f.cellRect(oldCol, oldRow)
	}
	if m.Start == m.Stop {
		f.curCol = -1
	} else {
		f.curCol, f.curRow = col, row
		f.curStart, f.curStop = int(m.Start), int(m.Stop)
	}
	if oldCol >= 0 {
		f.renderCell(oldCol, oldRow)
	}
	if f.curCol >= 0 {
		f.renderCell(f.curCol, f.curRow)
		dirty = dirty.Union(f.cellRect(f.curCol, f.curRow))
	}
	return Update{Dirty: dirty}
}

func (f *Framebuffer) specialBits(m SpecialGraphicsBit) Update {
	textChanged := m.Blinking() != f.blinking || m.DBESwap() != f.dbe ||
		m.PrimaryOffset() != f.primaryOff || m.SecondaryOffset() != f.secondaryOff
	colorChanged := m.ColorIndex() != f.colorIndex
	f.blinking, f.dbe = m.Blinking(), m.DBESwap()
	f.primaryOff, f.secondaryOff = m.PrimaryOffset(), m.SecondaryOffset()
	f.colorIndex = m.ColorIndex()
	switch {
	case f.text && textChanged:
		for row := range f.rows {
			for col := range f.cols {
				f.renderCell(col, row)
			}
		}
		return Update{Dirty: f.bounds()}
	case colorChanged && (f.text || f.srcBPP < 8):
		f.refreshIndexed(f.bounds())
		return Update{Dirty: f.bounds()}
	}
	return Update{}
}

// --- EnhanceBitBlt ---

// tileWalk calls fn for every present tile in wire order.
func tileWalk(m EnhanceBitBlt, w, h int, fn func(tx, ty int) error) (image.Rectangle, error) {
	tw, th := int(m.TileW), int(m.TileH)
	if tw == 0 || th == 0 {
		return image.Rectangle{}, nil
	}
	cols := min((w+tw-1)/tw, 64)
	rows := min((h+th-1)/th, 64)
	var dirty image.Rectangle
	for ty := range rows {
		bits := uint64(m.Hi[ty])<<32 | uint64(m.Lo[ty])
		for tx := range cols {
			if bits>>tx&1 == 0 {
				continue
			}
			if err := fn(tx*tw, ty*th); err != nil {
				return dirty, err
			}
			dirty = dirty.Union(image.Rect(tx*tw, ty*th, (tx+1)*tw, (ty+1)*th))
		}
	}
	return dirty.Intersect(image.Rect(0, 0, w, h)), nil
}

func (f *Framebuffer) enhanceBitBlt(m EnhanceBitBlt) (Update, error) {
	tw, th := int(m.TileW), int(m.TileH)
	var dirty image.Rectangle
	var err error
	switch int(m.Type &^ bltNewFrameMarker) {
	case bltEnhanceRaw:
		f.special4bpp = false
		bpp := f.bytesPerPixel()
		off := 0
		dirty, err = tileWalk(m, f.w, f.h, func(x0, y0 int) error {
			if len(m.Data)-off < tw*th*bpp {
				return errShortData
			}
			for y := range th {
				for x := range tw {
					if bpp == 1 {
						f.putIndex(x0+x, y0+y, m.Data[off])
					} else {
						f.putRGB(x0+x, y0+y, f.direct(pixelAt(m.Data, off, bpp)))
					}
					off += bpp
				}
			}
			return nil
		})

	case bltEnhanceHLC:
		f.special4bpp = false
		var planes []*rlePlane
		planes, err = f.hlcPlanes(m)
		if err != nil {
			return Update{}, err
		}
		dirty, err = tileWalk(m, f.w, f.h, func(x0, y0 int) error {
			for y := range th {
				for x := range tw {
					var v uint32
					for i, p := range planes {
						b, err := p.next()
						if err != nil {
							return err
						}
						v |= uint32(b) << (8 * i)
					}
					if len(planes) == 1 {
						f.putIndex(x0+x, y0+y, byte(v))
					} else {
						f.putRGB(x0+x, y0+y, f.direct(v))
					}
				}
			}
			return nil
		})

	case bltSpecial4bpp:
		f.special4bpp = true
		n := 0
		dirty, err = tileWalk(m, f.w, f.h, func(x0, y0 int) error {
			if len(m.Data)*2 < n+tw*th {
				return errShortData
			}
			for y := range th {
				for x := range tw {
					v := m.Data[n>>1]
					if x&1 != 0 {
						v >>= 4
					}
					f.putAttr(x0+x, y0+y, v&0x0F)
					n++
				}
			}
			return nil
		})
	}
	return Update{Dirty: dirty}, err
}

// hlcPlanes splits an HLC payload into its RLE byte planes (PROTOCOL.md 6.2).
func (f *Framebuffer) hlcPlanes(m EnhanceBitBlt) ([]*rlePlane, error) {
	d := m.Data
	mk := func(b []byte) *rlePlane {
		return &rlePlane{data: b, triplet: int(m.TripletCode), repeat: int(m.RepeatCode)}
	}
	if f.srcBPP <= 8 {
		return []*rlePlane{mk(d)}, nil
	}
	n := 2
	if f.srcBPP > 16 {
		n = 3
	}
	if len(d) < 4*n {
		return nil, errShortData
	}
	planes := make([]*rlePlane, n)
	off := 4 * n
	for i := range n {
		l := int(binary.LittleEndian.Uint32(d[4*i:]))
		if l < 0 || l > len(d)-off {
			return nil, errShortData
		}
		planes[i] = mk(d[off : off+l])
		off += l
	}
	return planes, nil
}

// rlePlane decodes one HLC byte plane.
type rlePlane struct {
	data            []byte
	pos             int
	count           int
	value           byte
	triplet, repeat int
}

func (p *rlePlane) byte() (byte, error) {
	if p.pos >= len(p.data) {
		return 0, errShortData
	}
	b := p.data[p.pos]
	p.pos++
	return b, nil
}

func (p *rlePlane) next() (byte, error) {
	if p.count < 1 {
		b, err := p.byte()
		if err != nil {
			return 0, err
		}
		switch int(b) {
		case p.triplet:
			p.count = 3
			if p.value, err = p.byte(); err != nil {
				return 0, err
			}
		case p.repeat:
			n, err := p.byte()
			if err != nil {
				return 0, err
			}
			switch n {
			case 1:
				p.count, p.value = 1, byte(p.triplet)
			case 0:
				p.count, p.value = 1, byte(p.repeat)
			default:
				p.count = int(n) + 1
				if p.value, err = p.byte(); err != nil {
					return 0, err
				}
			}
		default:
			p.count, p.value = 1, b
		}
	}
	p.count--
	return p.value, nil
}
