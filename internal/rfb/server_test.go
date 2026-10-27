package rfb

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/des"
	"encoding/binary"
	"image"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeDisplay struct {
	mu   sync.Mutex
	w, h int
	pix  []uint32
}

func newFakeDisplay(w, h int) *fakeDisplay {
	d := &fakeDisplay{w: w, h: h, pix: make([]uint32, w*h)}
	for i := range d.pix {
		d.pix[i] = uint32(i)
	}
	return d
}

func (d *fakeDisplay) Size() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.w, d.h
}

func (d *fakeDisplay) Read(r image.Rectangle, dst []uint32) image.Rectangle {
	d.mu.Lock()
	defer d.mu.Unlock()
	r = r.Intersect(image.Rect(0, 0, d.w, d.h))
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		n += copy(dst[n:], d.pix[y*d.w+r.Min.X:y*d.w+r.Max.X])
	}
	return r
}

type event struct {
	kind    string
	a, b, c int
	down    bool
}

type fakeInput struct{ ch chan event }

func (f *fakeInput) Key(k uint32, down bool) { f.ch <- event{kind: "key", a: int(k), down: down} }

func (f *fakeInput) Scancode(c uint32, down bool) { f.ch <- event{kind: "scan", a: int(c), down: down} }

func (f *fakeInput) Pointer(m uint8, x, y int) { f.ch <- event{kind: "ptr", a: int(m), b: x, c: y} }

func (f *fakeInput) ReleaseAll() { f.ch <- event{kind: "release"} }

// viewer is a minimal scripted RFB client.
type viewer struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (v *viewer) read(n int) []byte {
	v.t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(v.r, b); err != nil {
		v.t.Fatalf("read %d: %v", n, err)
	}
	return b
}

func (v *viewer) write(b ...byte) {
	v.t.Helper()
	if _, err := v.conn.Write(b); err != nil {
		v.t.Fatal(err)
	}
}

func (v *viewer) u16() int    { return int(binary.BigEndian.Uint16(v.read(2))) }
func (v *viewer) u32() uint32 { return binary.BigEndian.Uint32(v.read(4)) }

func startServer(t *testing.T, o Options) (*Server, *viewer, func()) {
	t.Helper()
	srv := NewServer(o)
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { srv.ServeConn(a); close(done) }()
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	v := &viewer{t: t, conn: b, r: bufio.NewReader(b)}
	return srv, v, func() { b.Close(); <-done }
}

// init38 performs a 3.8 handshake with security None and returns w, h.
func (v *viewer) init38() (int, int) {
	v.t.Helper()
	if got := string(v.read(12)); got != "RFB 003.008\n" {
		v.t.Fatalf("version %q", got)
	}
	v.write([]byte("RFB 003.008\n")...)
	if got := v.read(2); !bytes.Equal(got, []byte{1, 1}) {
		v.t.Fatalf("security types % X", got)
	}
	v.write(1)
	if res := v.u32(); res != 0 {
		v.t.Fatalf("security result %d", res)
	}
	v.write(1) // shared
	w, h := v.u16(), v.u16()
	pf := v.read(16)
	if pf[0] != 32 || pf[3] != 1 {
		v.t.Fatalf("pixel format % X", pf)
	}
	name := v.read(int(v.u32()))
	if string(name) != "test" {
		v.t.Fatalf("name %q", name)
	}
	return w, h
}

func (v *viewer) setEncodings(encs ...int32) {
	b := []byte{2, 0}
	b = binary.BigEndian.AppendUint16(b, uint16(len(encs)))
	for _, e := range encs {
		b = binary.BigEndian.AppendUint32(b, uint32(e))
	}
	v.write(b...)
}

func (v *viewer) request(incremental bool, x, y, w, h int) {
	b := []byte{3, 0}
	if incremental {
		b[1] = 1
	}
	for _, n := range []int{x, y, w, h} {
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	}
	v.write(b...)
}

type rectHdr struct {
	x, y, w, h int
	enc        int32
}

func (v *viewer) updateHeader() int {
	v.t.Helper()
	if t := v.read(2); t[0] != 0 {
		v.t.Fatalf("message type %d", t[0])
	}
	return v.u16()
}

func (v *viewer) rect() rectHdr {
	return rectHdr{v.u16(), v.u16(), v.u16(), v.u16(), int32(v.u32())}
}

func TestHandshakeAndRawUpdate(t *testing.T) {
	d := newFakeDisplay(4, 3)
	attached := make(chan struct{}, 1)
	_, v, stop := startServer(t, Options{Display: d, Name: "test", OnAttach: func() { attached <- struct{}{} }})
	defer stop()
	if w, h := v.init38(); w != 4 || h != 3 {
		t.Fatalf("size %dx%d", w, h)
	}
	<-attached
	v.setEncodings(encRaw)
	v.request(false, 1, 1, 2, 2)
	if n := v.updateHeader(); n != 1 {
		t.Fatalf("rects %d", n)
	}
	if r := v.rect(); r != (rectHdr{1, 1, 2, 2, encRaw}) {
		t.Fatalf("rect %+v", r)
	}
	px := v.read(2 * 2 * 4)
	// default format: 32bpp little endian, red shift 16 -> same as 0x00RRGGBB
	want := []uint32{5, 6, 9, 10}
	for i, w := range want {
		if got := binary.LittleEndian.Uint32(px[i*4:]); got != w {
			t.Fatalf("pixel %d = %d want %d", i, got, w)
		}
	}
}

func TestIncrementalWaitsForDamage(t *testing.T) {
	d := newFakeDisplay(4, 4)
	srv, v, stop := startServer(t, Options{Display: d, Name: "test"})
	defer stop()
	v.init38()
	v.setEncodings(encZlib, encRaw, encDesktopSize, encCursor, encQEMUExtendedKeyEvent)
	v.request(true, 0, 0, 4, 4)

	// first update carries the pseudo encodings only
	if n := v.updateHeader(); n != 2 {
		t.Fatalf("rects %d", n)
	}
	if r := v.rect(); r.enc != encQEMUExtendedKeyEvent {
		t.Fatalf("rect %+v", r)
	}
	r := v.rect()
	if r.enc != encCursor || r.w != arrowW || r.h != arrowH {
		t.Fatalf("cursor rect %+v", r)
	}
	v.read(arrowW*arrowH*4 + (arrowW+7)/8*arrowH)

	v.request(true, 0, 0, 4, 4)
	srv.Damage(image.Rect(2, 2, 3, 3))
	if n := v.updateHeader(); n != 1 {
		t.Fatalf("rects %d", n)
	}
	if r := v.rect(); r != (rectHdr{2, 2, 1, 1, encZlib}) {
		t.Fatalf("rect %+v", r)
	}
	z := v.read(int(v.u32()))
	zr, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	px := make([]byte, 4)
	if _, err := io.ReadFull(zr, px); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(px); got != 10 {
		t.Fatalf("pixel %d", got)
	}
}

func TestResize(t *testing.T) {
	d := newFakeDisplay(2, 2)
	srv, v, stop := startServer(t, Options{Display: d, Name: "test"})
	defer stop()
	v.init38()
	v.setEncodings(encRaw, encDesktopSize)
	v.request(true, 0, 0, 2, 2)

	d.mu.Lock()
	d.w, d.h, d.pix = 3, 1, []uint32{7, 8, 9}
	d.mu.Unlock()
	srv.Resize()

	if n := v.updateHeader(); n != 2 {
		t.Fatalf("rects %d", n)
	}
	if r := v.rect(); r != (rectHdr{0, 0, 3, 1, encDesktopSize}) {
		t.Fatalf("rect %+v", r)
	}
	if r := v.rect(); r != (rectHdr{0, 0, 3, 1, encRaw}) {
		t.Fatalf("rect %+v", r)
	}
	v.read(12)
}

func TestInputEvents(t *testing.T) {
	in := &fakeInput{ch: make(chan event, 10)}
	_, v, stop := startServer(t, Options{Display: newFakeDisplay(2, 2), Input: in, Name: "test"})
	v.init38()
	v.write(4, 1, 0, 0, 0, 0, 0xFF, 0x0D)               // KeyEvent Return down
	v.write(5, 0x05, 0, 10, 0, 20)                      // PointerEvent left+right at 10,20
	v.write(255, 0, 0, 1, 0, 0, 0, 0x61, 0, 0, 0, 0x1E) // QEMU key 'a' scancode down
	v.write(255, 0, 0, 0, 0, 0, 0, 0x62, 0, 0, 0, 0)    // QEMU key without scancode
	v.write(6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c')      // cut text, ignored
	want := []event{
		{kind: "key", a: 0xFF0D, down: true},
		{kind: "ptr", a: 5, b: 10, c: 20},
		{kind: "scan", a: 0x1E, down: true},
		{kind: "key", a: 0x62},
	}
	for _, w := range want {
		if got := <-in.ch; got != w {
			t.Fatalf("got %+v want %+v", got, w)
		}
	}
	stop()
	if got := <-in.ch; got.kind != "release" {
		t.Fatalf("got %+v want release", got)
	}
}

func TestVNCAuth(t *testing.T) {
	for _, tc := range []struct {
		pw string
		ok bool
	}{{"secret", true}, {"wrong", false}} {
		_, v, stop := startServer(t, Options{Display: newFakeDisplay(1, 1), Name: "test", Password: "secret"})
		v.read(12)
		v.write([]byte("RFB 003.008\n")...)
		if got := v.read(2); !bytes.Equal(got, []byte{1, 2}) {
			t.Fatalf("security types % X", got)
		}
		v.write(2)
		challenge := v.read(16)
		block, _ := des.NewCipher(vncKey(tc.pw))
		resp := make([]byte, 16)
		block.Encrypt(resp[:8], challenge[:8])
		block.Encrypt(resp[8:], challenge[8:])
		v.write(resp...)
		res := v.u32()
		if (res == 0) != tc.ok {
			t.Fatalf("password %q: result %d", tc.pw, res)
		}
		if !tc.ok {
			if msg := v.read(int(v.u32())); len(msg) == 0 {
				t.Fatal("missing failure reason")
			}
		}
		stop()
	}
}

func TestVersion33(t *testing.T) {
	_, v, stop := startServer(t, Options{Display: newFakeDisplay(1, 1), Name: "test"})
	defer stop()
	v.read(12)
	v.write([]byte("RFB 003.003\n")...)
	if st := v.u32(); st != 1 {
		t.Fatalf("security type %d", st)
	}
	v.write(0) // ClientInit, no SecurityResult for None in 3.3
	if w, h := v.u16(), v.u16(); w != 1 || h != 1 {
		t.Fatalf("size %dx%d", w, h)
	}
}

func TestVNCKey(t *testing.T) {
	// "a" = 0x61 = 01100001 -> reversed 10000110 = 0x86
	if got := vncKey("a"); !bytes.Equal(got, []byte{0x86, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("% X", got)
	}
}

func TestPixelFormatEncode(t *testing.T) {
	rgb565be := PixelFormat{
		BPP: 16, Depth: 16, BigEndian: true, TrueColor: true,
		RedMax: 31, GreenMax: 63, BlueMax: 31, RedShift: 11, GreenShift: 5,
	}
	if got := rgb565be.encode(nil, 0xFF0000); !bytes.Equal(got, []byte{0xF8, 0x00}) {
		t.Fatalf("565 red % X", got)
	}
	bgr233 := PixelFormat{
		BPP: 8, Depth: 8, TrueColor: true,
		RedMax: 7, GreenMax: 7, BlueMax: 3, RedShift: 0, GreenShift: 3, BlueShift: 6,
	}
	if got := bgr233.encode(nil, 0x0000FF); !bytes.Equal(got, []byte{0xC0}) {
		t.Fatalf("233 blue % X", got)
	}
}

func TestXVP(t *testing.T) {
	ops := make(chan PowerOp, 1)
	power := func(op PowerOp) error {
		ops <- op
		if op == PowerReset {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	_, v, stop := startServer(t, Options{Display: newFakeDisplay(4, 4), Name: "test", Power: power})
	defer stop()
	v.init38()
	v.setEncodings(encRaw, encXVP)
	if got := v.read(4); !bytes.Equal(got, []byte{250, 0, 1, xvpInit}) {
		t.Fatalf("XVP_INIT % X", got)
	}

	v.write(250, 0, 1, byte(PowerBIOSSetup))
	if op := <-ops; op != PowerBIOSSetup {
		t.Fatalf("op %v", op)
	}
	v.write(250, 0, 1, byte(PowerReset))
	if op := <-ops; op != PowerReset {
		t.Fatalf("op %v", op)
	}
	if got := v.read(4); !bytes.Equal(got, []byte{250, 0, 1, xvpFail}) {
		t.Fatalf("XVP_FAIL % X", got)
	}
	v.write(250, 0, 2, byte(PowerOn)) // unknown XVP version
	if got := v.read(4); !bytes.Equal(got, []byte{250, 0, 1, xvpFail}) {
		t.Fatalf("XVP_FAIL % X", got)
	}
}

func TestXVPWithoutPower(t *testing.T) {
	_, v, stop := startServer(t, Options{Display: newFakeDisplay(4, 4), Name: "test"})
	defer stop()
	v.init38()
	v.setEncodings(encRaw, encXVP)
	v.request(false, 0, 0, 1, 1)
	// no XVP_INIT, the first message is the update
	if n := v.updateHeader(); n != 1 {
		t.Fatalf("rects %d", n)
	}
	v.rect()
	v.read(4)
	v.write(250, 0, 1, byte(PowerShutdown))
	if got := v.read(4); !bytes.Equal(got, []byte{250, 0, 1, xvpFail}) {
		t.Fatalf("XVP_FAIL % X", got)
	}
}
