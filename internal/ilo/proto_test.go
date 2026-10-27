package ilo

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEncodeClientNOP(t *testing.T) {
	want := unhex(t, "D2 00 00 00 00 00 00 00")
	if got := EncodeClientNOP(); !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestEncodeClientHandshake(t *testing.T) {
	token := strings.Repeat("0123456789abcdef", 6) // 96 hex chars like httpdata
	b := EncodeClientHandshake(SignatureEmbedded, "admin", token, RightAll)
	if len(b) != 189 {
		t.Fatalf("length %d, want 189", len(b))
	}
	head := unhex(t, `DD 5A5A5A5A 10000000 14000000 04000000 00000000`)
	if !bytes.Equal(b[:21], head) {
		t.Fatalf("header % X, want % X", b[:21], head)
	}
	if got := string(b[21:37]); got != "admin\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("username field %q", got)
	}
	if got := string(b[37:57]); got != token[:20] {
		t.Fatalf("password field %q, want first 20 chars of token", got)
	}
	if got := binary.LittleEndian.Uint32(b[57:61]); got != 0x1F {
		t.Fatalf("rights 0x%X", got)
	}
	full := b[61:189]
	if string(full[:96]) != token || !bytes.Equal(full[96:], make([]byte, 32)) {
		t.Fatalf("full password field wrong: %q", full)
	}
}

func TestEncodeInput(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want string
	}{
		// PROTOCOL.md 5.1 example: press "A"
		{"key A down", EncodeKey(0x04, true), "D1 0400 01 00"},
		{"key ctrl up", EncodeKey(0xE0, false), "D1 E000 00 00"},
		// PROTOCOL.md 5.2 example: left click at (100,50)
		{"left down", EncodeButtonsAbsolute(100, 50, Buttons{Left: true}), "B3 6400 3200 03 81 80 80"},
		{"left up", EncodeButtonsAbsolute(100, 50, Buttons{}), "B3 6400 3200 03 80 80 80"},
		{"right+middle", EncodeButtonsAbsolute(1, 2, Buttons{Right: true, Middle: true}), "B3 0100 0200 03 80 81 81"},
		{"wheel up", EncodeButtonsAbsolute(0, 0, Buttons{Wheel: -1}), "B3 0000 0000 03 80 80 7E"},
		{"wheel down", EncodeButtonsAbsolute(0, 0, Buttons{Wheel: 1}), "B3 0000 0000 03 80 80 82"},
		{"relative", EncodeButtonsRelative(-1, 2, Buttons{}), "B4 FFFF 0200 03 80 80 80"},
		{"move", EncodeMouseMove(640, 480), "B5 8002 E001"},
		{"absolute on", EncodeAbsoluteMode(true), "B1 01"},
		{"relative mode", EncodeRelativeMode(true, false), "B2 01"},
		{"hide mode", EncodeRelativeMode(true, true), "B2 11"},
		{"vesa", EncodeRequestVesaMode(), "F1 00"},
		{"primary control", EncodeRequestPrimaryControl(), "D3 00"},
		{"invalidate", EncodeInvalidate(Region{W: 80, H: 25}), "F2 0100 0000 0000 5000 1900"},
		{"disconnect", EncodeClientDisconnect(0, "ok"), "D8 00000000 02000000 6F6B"},
	}
	for _, tc := range tests {
		if want := unhex(t, tc.want); !bytes.Equal(tc.got, want) {
			t.Errorf("%s: got % X, want % X", tc.name, tc.got, want)
		}
	}
}

func TestReaderSessionSetup(t *testing.T) {
	var s []byte
	// ServerHandshake: 32 bytes, privileges after the first space
	hs := make([]byte, 32)
	copy(hs, "2.0 kvms")
	s = append(append(s, 0xC8), hs...)
	// FirmwareVersion
	s = append(s, unhex(t, "C9 04000000")...)
	s = append(s, "4.26"...)
	// MultiUserState: flags = full control status (0) + storage, 2 users
	s = append(s, unhex(t, "C5 00010000 05 03 81 01")...)
	s = append(s, "adminbob"...)
	// InformVesaMode: mode 3, 720x400, bpp 4
	s = append(s, unhex(t, "E1 0300 D002 9001 0400")...)
	// InformKeyIndicators
	s = append(s, unhex(t, "D5 01 00 01")...)

	rd := NewReader(bytes.NewReader(s))
	var traced []byte
	rd.Trace = func(id byte, body []byte) { traced = append(traced, id) }

	m, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	h := m.(ServerHandshake)
	if h.Version != "2 0 kvms" || !h.Keyboard || !h.Video || !h.Mouse || !h.Storage {
		t.Fatalf("handshake %+v", h)
	}
	if m, _ := rd.Next(); m.(FirmwareVersion).Version != "4.26" {
		t.Fatalf("firmware %+v", m)
	}
	m, err = rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	mu := m.(MultiUserState)
	if mu.UserStatus() != UserStatusFullControl || mu.Flags&FlagStorageAllowed == 0 {
		t.Fatalf("flags 0x%X", mu.Flags)
	}
	if mu.Users[0].Name != "admin" || !mu.Users[0].Self() || mu.Users[1].Name != "bob" || mu.Users[1].Self() {
		t.Fatalf("users %+v", mu.Users)
	}
	if m, _ := rd.Next(); m.(VesaMode) != (VesaMode{Mode: 3, Width: 720, Height: 400, BPP: 4}) {
		t.Fatalf("vesa %+v", m)
	}
	if m, _ := rd.Next(); m.(KeyIndicators) != (KeyIndicators{Caps: true, Scroll: true}) {
		t.Fatalf("indicators %+v", m)
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if !bytes.Equal(traced, []byte{0xC8, 0xC9, 0xC5, 0xE1, 0xD5}) {
		t.Fatalf("trace % X", traced)
	}
}

func TestReaderBlits(t *testing.T) {
	var s []byte
	// BitBlt copy: type 0x100, font 16x8, src, dst (1,2,2,1), 4 bytes
	s = append(s, unhex(t, "E2 0001 10 08 0000000000000000 0100020002000100 04000000 AABBCCDD")...)
	// EnhanceBitBlt: type 0x81F2, tile 16x16, codes 0xFE/0xFD, raw 0, size 2
	e := unhex(t, "E3 F281 10 10 FE000000 FD000000 00000000 02000000")
	maps := make([]byte, 64*8)
	maps[0] = 0x05 // lo[0] = tiles 0 and 2
	maps[4] = 0x01 // hi[0] = tile 32
	maps[8] = 0x80 // lo[1] = tile 7
	e = append(append(e, maps...), 0x11, 0x22)
	s = append(s, e...)
	// SetPalette: 1 attribute, 2 colors
	s = append(s, unhex(t, "E6 0100 0F 3F 0200 00 010203 FF 0A0B0C")...)
	// SetTextCursor with swapped lines
	s = append(s, unhex(t, "EA 05 06 0F 0E")...)
	// StandbyPower
	s = append(s, 0xE4)
	s = append(s, make([]byte, 15)...)

	rd := NewReader(bytes.NewReader(s))
	m, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	bb := m.(BitBlt)
	if bb.Type != 0x100 || bb.FontHeight != 16 || bb.FontWidth != 8 ||
		bb.Dst != (Region{1, 2, 2, 1}) || !bytes.Equal(bb.Data, unhex(t, "AABBCCDD")) {
		t.Fatalf("bitblt %+v", bb)
	}
	m, err = rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	eb := m.(EnhanceBitBlt)
	if eb.Type != 0x81F2 || eb.TileW != 16 || eb.TripletCode != 0xFE || eb.RepeatCode != 0xFD ||
		eb.Lo[0] != 5 || eb.Hi[0] != 1 || eb.Lo[1] != 0x80 || !bytes.Equal(eb.Data, []byte{0x11, 0x22}) {
		t.Fatalf("enhance %+v", eb)
	}
	m, _ = rd.Next()
	sp := m.(SetPalette)
	if len(sp.Attributes) != 1 || sp.Attributes[0] != (AttributeEntry{0x0F, 0x3F}) ||
		len(sp.Colors) != 2 || sp.Colors[1] != (PaletteEntry{0xFF, 0x0A, 0x0B, 0x0C}) {
		t.Fatalf("palette %+v", sp)
	}
	if m, _ := rd.Next(); m.(TextCursor) != (TextCursor{5, 6, 0x0E, 0x0F}) {
		t.Fatalf("cursor %+v", m)
	}
	if m, _ := rd.Next(); m != (StandbyPower{}) {
		t.Fatalf("standby %+v", m)
	}
}

func TestReaderErrors(t *testing.T) {
	_, err := NewReader(bytes.NewReader([]byte{0x42})).Next()
	if !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("unknown id: %v", err)
	}
	_, err = NewReader(bytes.NewReader([]byte{0xE1, 0x03})).Next()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
}

func TestSpecialGraphicsBit(t *testing.T) {
	// blink, swap, primary select 5 (768), secondary select 2 (1024), color index 3
	s := SpecialGraphicsBit{Value: 1 | 1<<1 | 5<<2 | 2<<5 | 3<<8}
	if !s.Blinking() || !s.DBESwap() || s.PrimaryOffset() != 768 || s.SecondaryOffset() != 1024 || s.ColorIndex() != 3 {
		t.Fatalf("%+v", s)
	}
}

func TestRedactHandshake(t *testing.T) {
	b := EncodeClientHandshake(SignatureEmbedded, "admin", strings.Repeat("ab", 48), RightAll)
	r := redact(b)
	if strings.Contains(string(r), "ab") || !strings.Contains(string(r), "admin") {
		t.Fatalf("not redacted: %q", r)
	}
	if string(b[37:39]) != "ab" {
		t.Fatal("original modified")
	}
}
