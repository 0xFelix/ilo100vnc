package ilo

import (
	"image"
	"testing"
)

func pixel(f *Framebuffer, x, y int) uint32 {
	var p [1]uint32
	f.Read(image.Rect(x, y, x+1, y+1), p[:])
	return p[0]
}

func mustApply(t *testing.T, f *Framebuffer, msg any) Update {
	t.Helper()
	u, err := f.Apply(msg)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRLEPlane(t *testing.T) {
	// triplet code T=0xFE, repeat code R=0xFD
	p := &rlePlane{
		data: []byte{
			0x07,       // literal 7
			0xFE, 0x09, // 3 x 9
			0xFD, 0x04, 0x05, // 5 x 5 (n+1)
			0xFD, 0x01, // literal T
			0xFD, 0x00, // literal R
			0x01,
		},
		triplet: 0xFE, repeat: 0xFD,
	}
	want := []byte{7, 9, 9, 9, 5, 5, 5, 5, 5, 0xFE, 0xFD, 1}
	for i, w := range want {
		b, err := p.next()
		if err != nil {
			t.Fatalf("byte %d: %v", i, err)
		}
		if b != w {
			t.Fatalf("byte %d: got 0x%02X want 0x%02X", i, b, w)
		}
	}
	if _, err := p.next(); err == nil {
		t.Fatal("expected error at end of data")
	}
}

func TestColorConversion(t *testing.T) {
	if got := rgb565(0xF800); got != 0xFF0000 {
		t.Errorf("565 red: %06X", got)
	}
	if got := rgb565(0x07E0); got != 0x00FF00 {
		t.Errorf("565 green: %06X", got)
	}
	if got := rgb565(0x001F); got != 0x0000FF {
		t.Errorf("565 blue: %06X", got)
	}
	if got := rgb555(0x7C00); got != 0xFF0000 {
		t.Errorf("555 red: %06X", got)
	}
	if got := rgb555(0x03E0); got != 0x00FF00 {
		t.Errorf("555 green: %06X", got)
	}
}

func TestDefaultPalette(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x101, Width: 640, Height: 480, BPP: 8})
	// 3:3:2 default: index 0xFF = white, index 0x07 = red
	if got := f.rgb(0xFF); got != 0xFFFFFF {
		t.Errorf("index FF: %06X", got)
	}
	if got := f.rgb(0x07); got != 0xFF0000 {
		t.Errorf("index 07: %06X", got)
	}
}

func TestEnhanceRaw16(t *testing.T) {
	f := NewFramebuffer()
	u := mustApply(t, f, VesaMode{Mode: 0x111, Width: 6, Height: 3, BPP: 16})
	if !u.Resized {
		t.Fatal("expected resize")
	}
	// 4x2 tiles: tile columns 0,1 (x 0-3, 4-7), rows 0,1 (y 0-1, 2-3).
	// Only tile (1,1) present: covers x 4..7, y 2..3, of which x 4,5 y 2
	// are inside the 6x3 framebuffer. Each tile is 8 pixels * 2 bytes.
	m := EnhanceBitBlt{Type: bltEnhanceRaw, TileW: 4, TileH: 2}
	m.Lo[1] = 1 << 1
	data := make([]byte, 16)
	px := []uint16{0xF800, 0x07E0, 0x001F, 0xFFFF, 0, 0, 0, 0}
	for i, v := range px {
		data[2*i], data[2*i+1] = byte(v), byte(v>>8)
	}
	m.Data = data
	u = mustApply(t, f, m)
	if u.Dirty != image.Rect(4, 2, 6, 3) {
		t.Fatalf("dirty %v", u.Dirty)
	}
	if pixel(f, 4, 2) != 0xFF0000 || pixel(f, 5, 2) != 0x00FF00 {
		t.Fatalf("pixels %06X %06X", pixel(f, 4, 2), pixel(f, 5, 2))
	}
	if pixel(f, 0, 0) != 0 {
		t.Fatal("untouched pixel changed")
	}
}

func TestEnhanceHLC24(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x112, Width: 2, Height: 2, BPP: 24})
	// one 2x2 tile, three planes (B, G, R), each RLE coded with T=0xF0, R=0xF1
	planeB := []byte{0xF0, 0x10, 0x11}       // 10 10 10 11
	planeG := []byte{0xF1, 0x02, 0x20}       // 20 20 20 (n=2 -> 3), then...
	planeG = append(planeG, 0xF1, 0x01)      // literal T (0xF0)
	planeR := []byte{0x30, 0x31, 0x32, 0x33} // literals
	data := []byte{}
	for _, l := range []int{len(planeB), len(planeG), len(planeR)} {
		data = append(data, byte(l), 0, 0, 0)
	}
	data = append(data, planeB...)
	data = append(data, planeG...)
	data = append(data, planeR...)
	m := EnhanceBitBlt{Type: bltEnhanceHLC | bltNewFrameMarker, TileW: 2, TileH: 2, TripletCode: 0xF0, RepeatCode: 0xF1, Data: data}
	m.Lo[0] = 1
	u := mustApply(t, f, m)
	if u.Dirty != image.Rect(0, 0, 2, 2) {
		t.Fatalf("dirty %v", u.Dirty)
	}
	want := []uint32{0x302010, 0x312010, 0x322010, 0x33F011}
	for i, w := range want {
		if got := pixel(f, i%2, i/2); got != w {
			t.Errorf("pixel %d: %06X want %06X", i, got, w)
		}
	}
}

func TestEnhanceHLC8Palette(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x101, Width: 2, Height: 1, BPP: 8})
	mustApply(t, f, SetPalette{Colors: []PaletteEntry{{Index: 5, R: 1, G: 2, B: 3}}})
	// 8bpp: single plane without length header; tile 2x2, row 1 outside
	m := EnhanceBitBlt{
		Type: bltEnhanceHLC, TileW: 2, TileH: 2, TripletCode: 0xFE, RepeatCode: 0xFD,
		Data: []byte{0xFD, 0x02, 0x05, 0x00}, // 3 x 5, then 0
	}
	m.Lo[0] = 1
	mustApply(t, f, m)
	if pixel(f, 0, 0) != 0x010203 || pixel(f, 1, 0) != 0x010203 {
		t.Fatalf("pixels %06X %06X", pixel(f, 0, 0), pixel(f, 1, 0))
	}
	// palette change re-colors existing pixels
	mustApply(t, f, SetPalette{Colors: []PaletteEntry{{Index: 5, R: 9, G: 9, B: 9}}})
	if pixel(f, 0, 0) != 0x090909 {
		t.Fatalf("after palette change %06X", pixel(f, 0, 0))
	}
}

func TestEnhance4bpp(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x102, Width: 4, Height: 1, BPP: 4})
	mustApply(t, f, SetPalette{Colors: []PaletteEntry{
		{Index: 1, R: 0x11}, {Index: 2, R: 0x22}, {Index: 3, R: 0x33}, {Index: 4, R: 0x44},
	}})
	// even x low nibble, odd x high nibble
	m := EnhanceBitBlt{Type: bltSpecial4bpp, TileW: 4, TileH: 1, Data: []byte{0x21, 0x43}}
	m.Lo[0] = 1
	mustApply(t, f, m)
	for x, want := range []uint32{0x110000, 0x220000, 0x330000, 0x440000} {
		if got := pixel(f, x, 0); got != want {
			t.Errorf("x=%d: %06X want %06X", x, got, want)
		}
	}
}

func TestBitBlt4bppNibbleOrder(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x102, Width: 2, Height: 1, BPP: 4})
	mustApply(t, f, SetPalette{Colors: []PaletteEntry{{Index: 1, B: 1}, {Index: 2, B: 2}}})
	// w=2: k=1 (odd) takes the low nibble for x=0, k=0 the high nibble for x=1
	mustApply(t, f, BitBlt{Type: bltCopy, Dst: Region{W: 2, H: 1}, Data: []byte{0x21}})
	if pixel(f, 0, 0) != 1 || pixel(f, 1, 0) != 2 {
		t.Fatalf("pixels %06X %06X", pixel(f, 0, 0), pixel(f, 1, 0))
	}
}

func TestBitBltRGB32(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x118, Width: 3, Height: 2, BPP: 32})
	u := mustApply(t, f, BitBlt{
		Type: bltCopy, Dst: Region{X: 1, Y: 1, W: 2, H: 1},
		Data: []byte{0x01, 0x02, 0x03, 0xFF, 0x0A, 0x0B, 0x0C, 0xFF},
	})
	if u.Dirty != image.Rect(1, 1, 3, 2) {
		t.Fatalf("dirty %v", u.Dirty)
	}
	if pixel(f, 1, 1) != 0x030201 || pixel(f, 2, 1) != 0x0C0B0A {
		t.Fatalf("pixels %06X %06X", pixel(f, 1, 1), pixel(f, 2, 1))
	}
}

func textFB(t *testing.T) *Framebuffer {
	t.Helper()
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 3, Width: 640, Height: 400, BPP: 4})
	// palette: attribute value v -> palette index v, colors 0..15 distinct
	var cols []PaletteEntry
	for i := range 16 {
		cols = append(cols, PaletteEntry{Index: uint8(i), R: uint8(i * 16)})
	}
	mustApply(t, f, SetPalette{Colors: cols})
	return f
}

func TestTextRender(t *testing.T) {
	f := textFB(t)
	// glyph 'A' (0x41): scanline 0 = 0b10000001
	font := make([]byte, 0x42*32)
	font[0x41*32] = 0x81
	// type 0x107: chars + attrs + font for cell (2,1), attr fg=0xE bg=0x1
	data := append([]byte{0x41, 0x1E}, font...)
	u := mustApply(t, f, BitBlt{Type: 0x107, FontHeight: 16, FontWidth: 8, Dst: Region{X: 2, Y: 1, W: 1, H: 1}, Data: data})
	if u.Dirty != image.Rect(16, 16, 24, 32) {
		t.Fatalf("dirty %v", u.Dirty)
	}
	fg, bg := uint32(0xE0)<<16, uint32(0x10)<<16
	if pixel(f, 16, 16) != fg || pixel(f, 17, 16) != bg || pixel(f, 23, 16) != fg {
		t.Fatalf("row 0: %06X %06X %06X", pixel(f, 16, 16), pixel(f, 17, 16), pixel(f, 23, 16))
	}
	if pixel(f, 16, 17) != bg {
		t.Fatalf("row 1: %06X", pixel(f, 16, 17))
	}

	// cursor on scanlines 14-15 of that cell paints fg
	u = mustApply(t, f, TextCursor{Col: 2, Row: 1, Start: 14, Stop: 15})
	if u.Dirty != image.Rect(16, 16, 24, 32) {
		t.Fatalf("cursor dirty %v", u.Dirty)
	}
	if pixel(f, 17, 16+14) != fg {
		t.Fatal("cursor not drawn")
	}
	// hide the cursor again
	mustApply(t, f, TextCursor{Col: 2, Row: 1, Start: 0, Stop: 0})
	if pixel(f, 17, 16+14) != bg {
		t.Fatal("cursor not removed")
	}

	// attribute-only update keeps the character
	mustApply(t, f, BitBlt{Type: 0x102, FontHeight: 16, FontWidth: 8, Dst: Region{X: 2, Y: 1, W: 1, H: 1}, Data: []byte{0x2F}})
	if pixel(f, 16, 16) != uint32(0xF0)<<16 || pixel(f, 17, 16) != uint32(0x20)<<16 {
		t.Fatalf("after attr update: %06X %06X", pixel(f, 16, 16), pixel(f, 17, 16))
	}
}

func TestTextCombo(t *testing.T) {
	f := textFB(t)
	font := make([]byte, 0x43*32)
	font[0x42*32] = 0xFF
	mustApply(t, f, BitBlt{Type: 0x104, FontHeight: 16, FontWidth: 8, Dst: Region{W: 1, H: 1}, Data: font})
	// combo: char/attr pairs for a 2x1 rect
	mustApply(t, f, BitBlt{
		Type: bltTextCombo, FontHeight: 16, FontWidth: 8, Dst: Region{W: 2, H: 1},
		Data: []byte{0x00, 0x07, 0x42, 0x0C},
	})
	if pixel(f, 0, 0) != 0 || pixel(f, 8, 0) != uint32(0xC0)<<16 {
		t.Fatalf("pixels %06X %06X", pixel(f, 0, 0), pixel(f, 8, 0))
	}
}

func TestAttributeColorIndex(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x102, Width: 1, Height: 1, BPP: 4})
	mustApply(t, f, SetPalette{
		Attributes: []AttributeEntry{{Index: 0x13, Value: 0x07}},
		Colors:     []PaletteEntry{{Index: 3, G: 3}, {Index: 7, G: 7}},
	})
	mustApply(t, f, BitBlt{Type: bltCopy, Dst: Region{W: 1, H: 1}, Data: []byte{0x30}}) // k=0 takes the high nibble
	if pixel(f, 0, 0) != 0x000300 {
		t.Fatalf("bank 0: %06X", pixel(f, 0, 0))
	}
	// color index 1 selects attribute entry 0x10|3
	u := mustApply(t, f, SpecialGraphicsBit{Value: 1 << 8})
	if u.Dirty.Empty() || pixel(f, 0, 0) != 0x000700 {
		t.Fatalf("bank 1: %06X", pixel(f, 0, 0))
	}
}

func TestStandby(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x118, Width: 800, Height: 600, BPP: 32})
	u := mustApply(t, f, StandbyPower{})
	if w, h := f.Size(); w != 640 || h != 480 || !u.Resized {
		t.Fatalf("size %dx%d resized=%v", w, h, u.Resized)
	}
}

func TestShortDataIsError(t *testing.T) {
	f := NewFramebuffer()
	mustApply(t, f, VesaMode{Mode: 0x111, Width: 4, Height: 4, BPP: 16})
	m := EnhanceBitBlt{Type: bltEnhanceRaw, TileW: 4, TileH: 4, Data: []byte{1, 2, 3}}
	m.Lo[0] = 1
	if _, err := f.Apply(m); err == nil {
		t.Fatal("expected error")
	}
	if _, err := f.Apply(BitBlt{Type: bltCopy, Dst: Region{W: 4, H: 4}, Data: []byte{1}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestKeymaps(t *testing.T) {
	keysyms := map[uint32]uint16{
		'a': 0x04, 'Z': 0x1D, '1': 0x1E, '0': 0x27, '!': 0x1E, ')': 0x27,
		0xFF0D: 0x28, 0xFFBE: 0x3A, 0xFFC9: 0x45, 0xFFE3: 0xE0, 0xFE03: 0xE6,
		0xFFB5: 0x5D, 0xFFFF: 0x4C, 0x20AC: 0,
	}
	for ks, want := range keysyms {
		if got := KeysymToHID(ks); got != want {
			t.Errorf("keysym 0x%X: 0x%02X want 0x%02X", ks, got, want)
		}
	}
	codes := map[uint32]uint16{
		0x1E: 0x04, 0x10: 0x14, 0x2C: 0x1D, 0x1C: 0x28, 0x9C: 0x58,
		0xB8: 0xE6, 0xC8: 0x52, 0xDB: 0xE3, 0x56: 0x64, 0x1234: 0,
	}
	for c, want := range codes {
		if got := ScancodeToHID(c); got != want {
			t.Errorf("scancode 0x%X: 0x%02X want 0x%02X", c, got, want)
		}
	}
}

func TestTextBlink(t *testing.T) {
	f := textFB(t)
	font := make([]byte, 0x42*32)
	font[0x41*32] = 0x81
	// 'A' at (0,0) with blink bit set (attr 0x9E: blink, bg 1, fg E),
	// plain 'A' at (1,0)
	data := []byte{0x41, 0x41, 0x9E, 0x1E}
	data = append(data, font...)
	mustApply(t, f, BitBlt{Type: 0x107, FontHeight: 16, FontWidth: 8, Dst: Region{W: 2, H: 1}, Data: data})
	mustApply(t, f, SpecialGraphicsBit{Value: 1}) // blink enabled
	mustApply(t, f, TextCursor{Col: 1, Row: 0, Start: 14, Stop: 15})
	fg, bg := uint32(0xE0)<<16, uint32(0x10)<<16

	// phase on: cursor shown, blinking text hidden
	if pixel(f, 8+1, 14) != fg || pixel(f, 0, 0) != bg || pixel(f, 8, 0) != fg {
		t.Fatalf("phase on: cursor %06X blink %06X plain %06X", pixel(f, 9, 14), pixel(f, 0, 0), pixel(f, 8, 0))
	}
	u := f.ToggleBlink()
	if u.Dirty != image.Rect(0, 0, 16, 16) {
		t.Fatalf("dirty %v", u.Dirty)
	}
	// phase off: no cursor, blinking text visible
	if pixel(f, 8+1, 14) != bg || pixel(f, 0, 0) != fg || pixel(f, 8, 0) != fg {
		t.Fatalf("phase off: cursor %06X blink %06X plain %06X", pixel(f, 9, 14), pixel(f, 0, 0), pixel(f, 8, 0))
	}
	f.ToggleBlink()
	if pixel(f, 8+1, 14) != fg {
		t.Fatal("cursor not back")
	}

	// graphics mode: nothing to blink
	mustApply(t, f, VesaMode{Mode: 0x111, Width: 640, Height: 480, BPP: 16})
	if u := f.ToggleBlink(); !u.Dirty.Empty() {
		t.Fatalf("graphics dirty %v", u.Dirty)
	}
}

func TestBlank(t *testing.T) {
	f := textFB(t)
	font := make([]byte, 0x42*32)
	font[0x41*32] = 0xFF
	mustApply(t, f, BitBlt{Type: 0x107, FontHeight: 16, FontWidth: 8, Dst: Region{W: 1, H: 1}, Data: append([]byte{0x41, 0x1E}, font...)})
	if u := f.Blank(); u.Dirty != image.Rect(0, 0, 640, 400) {
		t.Fatalf("dirty %v", u.Dirty)
	}
	if pixel(f, 0, 0) != 0 {
		t.Fatalf("pixel %06X", pixel(f, 0, 0))
	}
	// a later palette change must not bring the old picture back
	mustApply(t, f, SetPalette{Colors: []PaletteEntry{{Index: 0, R: 0x80}}})
	if p := pixel(f, 0, 0); p != 0x800000 {
		t.Fatalf("after palette %06X", p)
	}
}
