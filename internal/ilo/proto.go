package ilo

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Message ids, see docs/PROTOCOL.md sections 5 and 6.
const (
	// client -> server
	msgStorageClientConnect    = 0x99
	msgStorageClientDisconnect = 0x9A
	msgClientAbsoluteMode      = 0xB1
	msgClientRelativeMode      = 0xB2
	msgButtonStateAtAbsolute   = 0xB3
	msgButtonStateAtRelative   = 0xB4
	msgMouseMove               = 0xB5
	msgRequestKeyIndicators    = 0xC1
	msgKeyStateChange          = 0xD1
	msgClientNOP               = 0xD2
	msgRequestPrimaryControl   = 0xD3
	msgClientDisconnect        = 0xD8
	msgClientHandshake         = 0xDD
	msgRequestVesaMode         = 0xF1
	msgInvalidate              = 0xF2
	msgInformHLevelCompression = 0xF3

	// server -> client
	msgStorageStatus         = 0x89
	msgMultiUserState        = 0xC5
	msgServerDisconnect      = 0xC6
	msgServerHandshake       = 0xC8
	msgFirmwareVersion       = 0xC9
	msgInformKeyIndicators   = 0xD5
	msgInformVesaMode        = 0xE1
	msgBitBlt                = 0xE2
	msgEnhanceBitBlt         = 0xE3
	msgStandbyPower          = 0xE4
	msgSetPalette            = 0xE6
	msgSetTextCursor         = 0xEA
	msgSpecialGraphicsBit    = 0xEB
	msgMatroxGraphicsCursor  = 0xEC
	msgGraphicsRegisterValue = 0xEE
)

// Handshake signatures (ClientHandshake offset 0).
const (
	SignatureEmbedded   = 0x5A5A5A5A
	SignatureStandalone = 0x12121212
)

// Rights requested in the ClientHandshake.
const (
	RightVideo    = 1 << 0
	RightMouse    = 1 << 1
	RightKeyboard = 1 << 2
	RightStorage0 = 1 << 3
	RightStorage1 = 1 << 4
	RightAll      = 0x1F
)

// MultiUserState session flags.
const (
	FlagCanTakeFullControl = 0x00000001
	FlagStorageAllowed     = 0x00000100
	FlagStorageEnabled     = 0x00200000
	userStatusMask         = 0x18
	userStatusShift        = 3
)

// User status values from MultiUserState flags bits 3-4.
const (
	UserStatusFullControl = 0
	UserStatusViewOnly    = 1
	UserStatusEphemeral   = 2
	UserStatusDisconnect  = 3
)

const (
	maxBlitData = 16 << 20 // sanity cap for BitBlt/EnhanceBitBlt payloads
	snoopRows   = 64
)

// Region is a rectangle as sent on the wire (4 x i16).
type Region struct {
	X, Y, W, H int16
}

// --- client message encoders ---

func le16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func le32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// fixed returns s as exactly n bytes, NUL padded or truncated.
func fixed(s string, n int) []byte {
	b := make([]byte, n)
	copy(b, s)
	return b
}

// EncodeClientNOP is 0xD2 plus 7 zero bytes, sent once right after connecting.
func EncodeClientNOP() []byte {
	b := make([]byte, 8)
	b[0] = msgClientNOP
	return b
}

// EncodeClientHandshake builds the 0xDD message (189 bytes).
func EncodeClientHandshake(signature uint32, user, secret string, rights uint32) []byte {
	b := []byte{msgClientHandshake}
	b = le32(b, signature)
	b = le32(b, 16) // username length
	b = le32(b, 20) // password length
	b = le32(b, 4)  // config length
	b = le32(b, 0)  // key length
	b = append(b, fixed(user, 16)...)
	b = append(b, fixed(secret, 20)...)
	b = le32(b, rights)
	b = append(b, fixed(secret, 128)...)
	return b
}

func encodeByteMsg(id byte, v byte) []byte { return []byte{id, v} }

// EncodeAbsoluteMode is 0xB1.
func EncodeAbsoluteMode(on bool) []byte { return encodeByteMsg(msgClientAbsoluteMode, boolByte(on)) }

// EncodeRelativeMode is 0xB2: bit0 relative, bit4 hide cursor.
func EncodeRelativeMode(relative, hide bool) []byte {
	var v byte
	if relative {
		v |= 0x01
	}
	if hide {
		v |= 0x10
	}
	return encodeByteMsg(msgClientRelativeMode, v)
}

// Buttons is the pressed state of the three buttons.
type Buttons struct {
	Left, Right, Middle bool
	// Wheel is the signed notch count (positive = down), 0 for none.
	Wheel int
}

func (bt Buttons) bytes() []byte {
	enc := func(pressed bool, wheel int) byte {
		v := byte(64+wheel) << 1
		if pressed {
			v |= 1
		}
		return v
	}
	return []byte{enc(bt.Left, 0), enc(bt.Right, 0), enc(bt.Middle, bt.Wheel)}
}

// EncodeButtonsAbsolute is 0xB3.
func EncodeButtonsAbsolute(x, y int32, bt Buttons) []byte {
	b := []byte{msgButtonStateAtAbsolute}
	b = le32(b, uint32(x))
	b = le32(b, uint32(y))
	b = append(b, 3)
	return append(b, bt.bytes()...)
}

// EncodeButtonsRelative is 0xB4.
func EncodeButtonsRelative(dx, dy int32, bt Buttons) []byte {
	b := EncodeButtonsAbsolute(dx, dy, bt)
	b[0] = msgButtonStateAtRelative
	return b
}

// EncodeMouseMove is 0xB5 (absolute pixel, or delta in relative mode).
func EncodeMouseMove(x, y int32) []byte {
	b := []byte{msgMouseMove}
	b = le32(b, uint32(x))
	return le32(b, uint32(y))
}

// EncodeKey is 0xD1 with a USB HID usage id.
func EncodeKey(usage uint16, pressed bool) []byte {
	b := []byte{msgKeyStateChange}
	b = le16(b, usage)
	return append(b, boolByte(pressed), 0)
}

// EncodeRequestKeyIndicators is 0xC1.
func EncodeRequestKeyIndicators() []byte { return encodeByteMsg(msgRequestKeyIndicators, 0) }

// EncodeRequestPrimaryControl is 0xD3.
func EncodeRequestPrimaryControl() []byte { return encodeByteMsg(msgRequestPrimaryControl, 0) }

// EncodeRequestVesaMode is 0xF1.
func EncodeRequestVesaMode() []byte { return encodeByteMsg(msgRequestVesaMode, 0) }

// EncodeHLevelCompression is 0xF3.
func EncodeHLevelCompression(on bool) []byte {
	return encodeByteMsg(msgInformHLevelCompression, boolByte(on))
}

// EncodeInvalidate is 0xF2.
func EncodeInvalidate(regions ...Region) []byte {
	b := []byte{msgInvalidate}
	b = le16(b, uint16(len(regions)))
	for _, r := range regions {
		b = le16(b, uint16(r.X))
		b = le16(b, uint16(r.Y))
		b = le16(b, uint16(r.W))
		b = le16(b, uint16(r.H))
	}
	return b
}

// EncodeClientDisconnect is 0xD8.
func EncodeClientDisconnect(reason int32, msg string) []byte {
	b := []byte{msgClientDisconnect}
	b = le32(b, uint32(reason))
	b = le32(b, uint32(len(msg)))
	return append(b, msg...)
}

// --- server messages ---

// ServerHandshake (0xC8).
type ServerHandshake struct {
	Version                         string
	Keyboard, Video, Mouse, Storage bool
}

// FirmwareVersion (0xC9).
type FirmwareVersion struct{ Version string }

// User is one entry of the MultiUserState user table.
type User struct {
	Name  string
	Flags byte
}

// Valid reports whether the slot is in use.
func (u User) Valid() bool { return u.Flags&0x01 != 0 }

// Self reports whether the slot is this session.
func (u User) Self() bool { return u.Flags&0x80 != 0 }

// MultiUserState (0xC5).
type MultiUserState struct {
	Flags uint32
	Users [2]User
}

// UserStatus returns UserStatus* for this session.
func (m MultiUserState) UserStatus() int { return int(m.Flags&userStatusMask) >> userStatusShift }

// ServerDisconnect (0xC6).
type ServerDisconnect struct {
	Reason  int32
	Message string
}

// KeyIndicators (0xD5).
type KeyIndicators struct{ Caps, Num, Scroll bool }

// VesaMode (0xE1).
type VesaMode struct {
	Mode, Width, Height, BPP int16
}

// BitBlt (0xE2).
type BitBlt struct {
	Type                  uint16
	FontHeight, FontWidth uint8
	Src, Dst              Region
	Data                  []byte
}

// EnhanceBitBlt (0xE3).
type EnhanceBitBlt struct {
	Type                    uint16
	TileW, TileH            uint8
	TripletCode, RepeatCode uint32
	RawSize                 uint32
	Lo, Hi                  [snoopRows]uint32
	Data                    []byte
}

// StandbyPower (0xE4).
type StandbyPower struct{}

// PaletteEntry is one SetPalette color.
type PaletteEntry struct{ Index, R, G, B uint8 }

// AttributeEntry is one SetPalette attribute table entry.
type AttributeEntry struct{ Index, Value uint8 }

// SetPalette (0xE6).
type SetPalette struct {
	Attributes []AttributeEntry
	Colors     []PaletteEntry
}

// TextCursor (0xEA).
type TextCursor struct{ Col, Row, Start, Stop uint8 }

// SpecialGraphicsBit (0xEB).
type SpecialGraphicsBit struct{ Value uint32 }

// Blinking reports whether blinking text is enabled.
func (s SpecialGraphicsBit) Blinking() bool { return s.Value&1 != 0 }

// DBESwap reports whether dual character sets are active.
func (s SpecialGraphicsBit) DBESwap() bool { return s.Value>>1&1 != 0 }

var charMapOffset = [8]int{0, 512, 1024, 1536, 256, 768, 1280, 1792}

// PrimaryOffset is the glyph offset of the primary character map.
func (s SpecialGraphicsBit) PrimaryOffset() int { return charMapOffset[s.Value>>2&7] }

// SecondaryOffset is the glyph offset of the secondary character map.
func (s SpecialGraphicsBit) SecondaryOffset() int { return charMapOffset[s.Value>>5&7] }

// ColorIndex is the 4bpp attribute table bank.
func (s SpecialGraphicsBit) ColorIndex() int { return int(s.Value >> 8 & 3) }

// MatroxCursor (0xEC).
type MatroxCursor struct {
	Control [256]byte
	Data    [64][48]byte
}

// Mode returns the cursor mode (control byte 6).
func (m *MatroxCursor) Mode() byte { return m.Control[6] }

// StorageStatus (0x89), kept raw until virtual media is implemented.
type StorageStatus struct{ Raw []byte }

// GraphicsRegisters (0xEE), ignored.
type GraphicsRegisters struct{}

// ErrUnknownMessage is returned for an id the protocol does not define.
// The stream cannot be resynchronized after it.
var ErrUnknownMessage = errors.New("unknown message id")

// Reader decodes server messages from a stream.
type Reader struct {
	r   *bufio.Reader
	buf []byte
	// Trace, if set, receives the id and raw body of every message.
	Trace func(id byte, body []byte)
	body  []byte
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10)}
}

func (rd *Reader) read(n int) ([]byte, error) {
	if cap(rd.buf) < n {
		rd.buf = make([]byte, n)
	}
	b := rd.buf[:n]
	if _, err := io.ReadFull(rd.r, b); err != nil {
		return nil, err
	}
	if rd.Trace != nil {
		rd.body = append(rd.body, b...)
	}
	return b, nil
}

func (rd *Reader) u16() (uint16, error) {
	b, err := rd.read(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (rd *Reader) u32() (uint32, error) {
	b, err := rd.read(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// bytes reads n bytes into a fresh slice.
func (rd *Reader) bytes(n int) ([]byte, error) {
	b, err := rd.read(n)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), b...), nil
}

func (rd *Reader) region() (Region, error) {
	b, err := rd.read(8)
	if err != nil {
		return Region{}, err
	}
	return Region{
		X: int16(binary.LittleEndian.Uint16(b[0:])),
		Y: int16(binary.LittleEndian.Uint16(b[2:])),
		W: int16(binary.LittleEndian.Uint16(b[4:])),
		H: int16(binary.LittleEndian.Uint16(b[6:])),
	}, nil
}

// Next reads one message. It returns one of the message types above.
func (rd *Reader) Next() (any, error) {
	id, err := rd.r.ReadByte()
	if err != nil {
		return nil, err
	}
	rd.body = rd.body[:0]
	msg, err := rd.parse(id)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("message 0x%02X: %w", id, err)
	}
	if rd.Trace != nil {
		rd.Trace(id, rd.body)
	}
	return msg, nil
}

func (rd *Reader) parse(id byte) (any, error) {
	switch id {
	case msgServerHandshake:
		b, err := rd.read(32)
		if err != nil {
			return nil, err
		}
		return parseServerHandshake(b), nil

	case msgFirmwareVersion:
		n, err := rd.u32()
		if err != nil {
			return nil, err
		}
		if n > 4096 {
			return nil, fmt.Errorf("firmware version length %d", n)
		}
		b, err := rd.read(int(n))
		if err != nil {
			return nil, err
		}
		return FirmwareVersion{Version: string(b)}, nil

	case msgMultiUserState:
		h, err := rd.read(8)
		if err != nil {
			return nil, err
		}
		m := MultiUserState{Flags: binary.LittleEndian.Uint32(h)}
		l0, l1 := int(h[4]), int(h[5])
		m.Users[0].Flags, m.Users[1].Flags = h[6], h[7]
		n0, err := rd.bytes(l0)
		if err != nil {
			return nil, err
		}
		n1, err := rd.bytes(l1)
		if err != nil {
			return nil, err
		}
		m.Users[0].Name, m.Users[1].Name = trimNUL(n0), trimNUL(n1)
		return m, nil

	case msgServerDisconnect:
		reason, err := rd.u32()
		if err != nil {
			return nil, err
		}
		n, err := rd.u16()
		if err != nil {
			return nil, err
		}
		b, err := rd.read(int(n))
		if err != nil {
			return nil, err
		}
		return ServerDisconnect{Reason: int32(reason), Message: string(b)}, nil

	case msgInformKeyIndicators:
		b, err := rd.read(3)
		if err != nil {
			return nil, err
		}
		return KeyIndicators{Caps: b[0] != 0, Num: b[1] != 0, Scroll: b[2] != 0}, nil

	case msgInformVesaMode:
		b, err := rd.read(8)
		if err != nil {
			return nil, err
		}
		return VesaMode{
			Mode:   int16(binary.LittleEndian.Uint16(b[0:])),
			Width:  int16(binary.LittleEndian.Uint16(b[2:])),
			Height: int16(binary.LittleEndian.Uint16(b[4:])),
			BPP:    int16(binary.LittleEndian.Uint16(b[6:])),
		}, nil

	case msgBitBlt:
		h, err := rd.read(4)
		if err != nil {
			return nil, err
		}
		m := BitBlt{
			Type:       binary.LittleEndian.Uint16(h),
			FontHeight: h[2],
			FontWidth:  h[3],
		}
		if m.Src, err = rd.region(); err != nil {
			return nil, err
		}
		if m.Dst, err = rd.region(); err != nil {
			return nil, err
		}
		n, err := rd.u32()
		if err != nil {
			return nil, err
		}
		if n > maxBlitData {
			return nil, fmt.Errorf("bitblt size %d", n)
		}
		if m.Data, err = rd.bytes(int(n)); err != nil {
			return nil, err
		}
		return m, nil

	case msgEnhanceBitBlt:
		h, err := rd.read(20 + snoopRows*8)
		if err != nil {
			return nil, err
		}
		m := EnhanceBitBlt{
			Type:        binary.LittleEndian.Uint16(h),
			TileW:       h[2],
			TileH:       h[3],
			TripletCode: binary.LittleEndian.Uint32(h[4:]),
			RepeatCode:  binary.LittleEndian.Uint32(h[8:]),
			RawSize:     binary.LittleEndian.Uint32(h[12:]),
		}
		n := binary.LittleEndian.Uint32(h[16:])
		for i := range snoopRows {
			m.Lo[i] = binary.LittleEndian.Uint32(h[20+i*8:])
			m.Hi[i] = binary.LittleEndian.Uint32(h[24+i*8:])
		}
		if n > maxBlitData {
			return nil, fmt.Errorf("enhance bitblt size %d", n)
		}
		if m.Data, err = rd.bytes(int(n)); err != nil {
			return nil, err
		}
		return m, nil

	case msgStandbyPower:
		if _, err := rd.read(15); err != nil {
			return nil, err
		}
		return StandbyPower{}, nil

	case msgSetPalette:
		var m SetPalette
		na, err := rd.u16()
		if err != nil {
			return nil, err
		}
		b, err := rd.read(int(na) * 2)
		if err != nil {
			return nil, err
		}
		for i := range int(na) {
			m.Attributes = append(m.Attributes, AttributeEntry{b[i*2], b[i*2+1]})
		}
		np, err := rd.u16()
		if err != nil {
			return nil, err
		}
		b, err = rd.read(int(np) * 4)
		if err != nil {
			return nil, err
		}
		for i := range int(np) {
			m.Colors = append(m.Colors, PaletteEntry{b[i*4], b[i*4+1], b[i*4+2], b[i*4+3]})
		}
		return m, nil

	case msgSetTextCursor:
		b, err := rd.read(4)
		if err != nil {
			return nil, err
		}
		c := TextCursor{Col: b[0], Row: b[1], Start: b[2], Stop: b[3]}
		if c.Stop < c.Start {
			c.Start, c.Stop = c.Stop, c.Start
		}
		return c, nil

	case msgSpecialGraphicsBit:
		v, err := rd.u32()
		if err != nil {
			return nil, err
		}
		return SpecialGraphicsBit{Value: v}, nil

	case msgMatroxGraphicsCursor:
		b, err := rd.read(256 + 64*48)
		if err != nil {
			return nil, err
		}
		m := &MatroxCursor{}
		copy(m.Control[:], b)
		for i := range m.Data {
			copy(m.Data[i][:], b[256+i*48:])
		}
		return m, nil

	case msgGraphicsRegisterValue:
		if _, err := rd.read(17 * 4); err != nil {
			return nil, err
		}
		return GraphicsRegisters{}, nil

	case msgStorageStatus:
		b, err := rd.bytes(1056)
		if err != nil {
			return nil, err
		}
		return StorageStatus{Raw: b}, nil
	}
	return nil, fmt.Errorf("%w 0x%02X", ErrUnknownMessage, id)
}

func parseServerHandshake(b []byte) ServerHandshake {
	s := make([]byte, len(b))
	for i, c := range b {
		if c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			s[i] = c
		} else {
			s[i] = ' '
		}
	}
	h := ServerHandshake{Version: strings.Trim(string(s), " ")}
	// rights follow the first space, e.g. "... KVMS"
	_, rights, _ := strings.Cut(h.Version, " ")
	for _, c := range rights {
		switch c | 0x20 {
		case 'k':
			h.Keyboard = true
		case 'v':
			h.Video = true
		case 'm':
			h.Mouse = true
		case 's':
			h.Storage = true
		}
	}
	return h
}

func trimNUL(b []byte) string {
	b, _, _ = bytes.Cut(b, []byte{0})
	return string(b)
}
