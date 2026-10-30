// Package rfb implements the server side of the RFB (VNC) protocol, RFC 6143.
package rfb

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/des"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Display is the screen being served. Pixels are 0x00RRGGBB.
type Display interface {
	Size() (w, h int)
	// Read copies r (clipped) row by row into dst and returns the clipped rect.
	Read(r image.Rectangle, dst []uint32) image.Rectangle
}

// Input receives events from viewers.
type Input interface {
	Key(keysym uint32, down bool)
	// Scancode is a QEMU extended key event (PC set 1, extended = 0x80|code).
	Scancode(code uint32, down bool)
	Pointer(mask uint8, x, y int)
	// ReleaseAll is called when a viewer disconnects.
	ReleaseAll()
}

// PowerOp is an XVP message code. Codes from 0x80 are ilo100vnc extensions
// sent by its noVNC add-on.
type PowerOp uint8

// Power operations.
const (
	PowerShutdown  PowerOp = 2    // XVP shutdown: clean (ACPI) shutdown
	PowerReboot    PowerOp = 3    // XVP reboot
	PowerReset     PowerOp = 4    // XVP reset: hard reset
	PowerOn        PowerOp = 0x80 // extension: power on
	PowerBIOSSetup PowerOp = 0x81 // extension: boot into BIOS Setup
	PowerOff       PowerOp = 0x82 // extension: hard power off
)

func (op PowerOp) String() string {
	switch op {
	case PowerShutdown:
		return "shutdown"
	case PowerReboot:
		return "reboot"
	case PowerReset:
		return "reset"
	case PowerOn:
		return "power-on"
	case PowerBIOSSetup:
		return "bios-setup"
	case PowerOff:
		return "power-off"
	}
	return fmt.Sprintf("unknown(%d)", uint8(op))
}

// Options configure a Server.
type Options struct {
	Display  Display
	Input    Input
	Name     string
	Password string // VNC authentication if non-empty (only 8 chars are used)
	Log      *slog.Logger
	// OnAttach and OnDetach are called when an authenticated viewer
	// connects and disconnects.
	OnAttach func()
	OnDetach func()
	// Power, if set, is announced to viewers through the XVP extension and
	// runs their power operations. A returned error is reported as XVP_FAIL.
	Power func(PowerOp) error
}

// Encodings.
const (
	encRaw                  = 0
	encZlib                 = 6
	encDesktopSize          = -223
	encCursor               = -239
	encQEMUExtendedKeyEvent = -258
	encXVP                  = -309
)

// XVP server message codes.
const (
	xvpFail = 0
	xvpInit = 1
)

// Server serves one Display to many viewers.
type Server struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	clients map[*client]struct{}
}

// NewServer creates a server.
func NewServer(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Name == "" {
		o.Name = "ilo100vnc"
	}
	return &Server{opts: o, log: o.Log, clients: map[*client]struct{}{}}
}

// Serve accepts connections on l until it fails.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.ServeConn(c)
	}
}

// Damage marks r as changed for all viewers.
func (s *Server) Damage(r image.Rectangle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		c.damage(r)
	}
}

// Resize tells all viewers that the display size changed.
func (s *Server) Resize() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		c.resize()
	}
}

// ServeConn runs the RFB protocol on conn and blocks until it ends.
func (s *Server) ServeConn(conn net.Conn) {
	defer conn.Close()
	log := s.log.With("viewer", conn.RemoteAddr().String())
	c := &client{
		srv:    s,
		conn:   conn,
		r:      bufio.NewReader(conn),
		w:      bufio.NewWriterSize(conn, 64<<10),
		log:    log,
		pf:     defaultPixelFormat,
		wake:   make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := c.handshake(); err != nil {
		log.Info("handshake failed", "err", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	log.Info("viewer connected")

	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	if s.opts.OnAttach != nil {
		s.opts.OnAttach()
	}
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
		if s.opts.Input != nil {
			s.opts.Input.ReleaseAll()
		}
		if s.opts.OnDetach != nil {
			s.opts.OnDetach()
		}
	}()

	go c.writer()
	err := c.reader()
	close(c.closed)
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		log.Info("viewer disconnected")
	} else {
		log.Info("viewer disconnected", "err", err)
	}
}

// PixelFormat is the RFB PIXEL_FORMAT structure.
type PixelFormat struct {
	BPP, Depth                      uint8
	BigEndian, TrueColor            bool
	RedMax, GreenMax, BlueMax       uint16
	RedShift, GreenShift, BlueShift uint8
}

var defaultPixelFormat = PixelFormat{
	BPP: 32, Depth: 24, TrueColor: true,
	RedMax: 255, GreenMax: 255, BlueMax: 255,
	RedShift: 16, GreenShift: 8, BlueShift: 0,
}

func (pf PixelFormat) marshal() []byte {
	b := make([]byte, 16)
	b[0], b[1] = pf.BPP, pf.Depth
	if pf.BigEndian {
		b[2] = 1
	}
	if pf.TrueColor {
		b[3] = 1
	}
	binary.BigEndian.PutUint16(b[4:], pf.RedMax)
	binary.BigEndian.PutUint16(b[6:], pf.GreenMax)
	binary.BigEndian.PutUint16(b[8:], pf.BlueMax)
	b[10], b[11], b[12] = pf.RedShift, pf.GreenShift, pf.BlueShift
	return b
}

func unmarshalPixelFormat(b []byte) PixelFormat {
	return PixelFormat{
		BPP: b[0], Depth: b[1], BigEndian: b[2] != 0, TrueColor: b[3] != 0,
		RedMax:   binary.BigEndian.Uint16(b[4:]),
		GreenMax: binary.BigEndian.Uint16(b[6:]),
		BlueMax:  binary.BigEndian.Uint16(b[8:]),
		RedShift: b[10], GreenShift: b[11], BlueShift: b[12],
	}
}

func (pf PixelFormat) bytesPerPixel() int { return int(pf.BPP) / 8 }

// encode appends pixel p (0xRRGGBB) in this format.
func (pf PixelFormat) encode(dst []byte, p uint32) []byte {
	r, g, b := p>>16&0xFF, p>>8&0xFF, p&0xFF
	v := (r*uint32(pf.RedMax)+127)/255<<pf.RedShift |
		(g*uint32(pf.GreenMax)+127)/255<<pf.GreenShift |
		(b*uint32(pf.BlueMax)+127)/255<<pf.BlueShift
	switch pf.BPP {
	case 8:
		return append(dst, byte(v))
	case 16:
		if pf.BigEndian {
			return binary.BigEndian.AppendUint16(dst, uint16(v))
		}
		return binary.LittleEndian.AppendUint16(dst, uint16(v))
	default:
		if pf.BigEndian {
			return binary.BigEndian.AppendUint32(dst, v)
		}
		return binary.LittleEndian.AppendUint32(dst, v)
	}
}

type client struct {
	srv  *Server
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	log  *slog.Logger

	wake   chan struct{}
	closed chan struct{}

	mu          sync.Mutex
	pf          PixelFormat
	encoding    int32
	desktopSize bool
	cursor      bool
	qemuKeys    bool
	xvp         bool // XVP_INIT was sent
	sentPseudo  bool
	dirty       image.Rectangle
	want        bool // an update request is outstanding
	needResize  bool
	w0, h0      int    // size last told to the viewer
	out         []byte // server messages waiting for the writer

	zw   *zlib.Writer
	zbuf bytes.Buffer
}

func (c *client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *client) damage(r image.Rectangle) {
	c.mu.Lock()
	c.dirty = c.dirty.Union(r)
	c.mu.Unlock()
	c.signal()
}

// send queues a server message for the writer.
func (c *client) send(b ...byte) {
	c.mu.Lock()
	c.out = append(c.out, b...)
	c.mu.Unlock()
	c.signal()
}

func (c *client) resize() {
	c.mu.Lock()
	c.needResize = true
	c.mu.Unlock()
	c.signal()
}

func (c *client) handshake() error {
	if _, err := io.WriteString(c.conn, "RFB 003.008\n"); err != nil {
		return err
	}
	var ver [12]byte
	if _, err := io.ReadFull(c.r, ver[:]); err != nil {
		return err
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(ver[:]), "RFB %03d.%03d\n", &major, &minor); err != nil || major != 3 {
		return fmt.Errorf("bad protocol version %q", ver)
	}
	if minor < 7 {
		minor = 3
	} else if minor > 8 {
		minor = 8
	}

	secType := byte(1) // None
	if c.srv.opts.Password != "" {
		secType = 2 // VNC authentication
	}
	if minor == 3 {
		if err := binary.Write(c.conn, binary.BigEndian, uint32(secType)); err != nil {
			return err
		}
	} else {
		if _, err := c.conn.Write([]byte{1, secType}); err != nil {
			return err
		}
		chosen, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		if chosen != secType {
			return fmt.Errorf("client chose security type %d", chosen)
		}
	}

	ok := true
	if secType == 2 {
		var err error
		if ok, err = c.vncAuth(); err != nil {
			return err
		}
	}
	if secType == 2 || minor == 8 {
		res := uint32(0)
		if !ok {
			res = 1
		}
		b := binary.BigEndian.AppendUint32(nil, res)
		if !ok && minor == 8 {
			msg := "authentication failed"
			b = binary.BigEndian.AppendUint32(b, uint32(len(msg)))
			b = append(b, msg...)
		}
		if _, err := c.conn.Write(b); err != nil {
			return err
		}
	}
	if !ok {
		return errors.New("authentication failed")
	}

	if _, err := c.r.ReadByte(); err != nil { // ClientInit shared flag
		return err
	}
	w, h := c.srv.opts.Display.Size()
	c.w0, c.h0 = w, h
	b := binary.BigEndian.AppendUint16(nil, uint16(w))
	b = binary.BigEndian.AppendUint16(b, uint16(h))
	b = append(b, c.pf.marshal()...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(c.srv.opts.Name)))
	b = append(b, c.srv.opts.Name...)
	_, err := c.conn.Write(b)
	return err
}

// vncKey builds the DES key: password padded to 8 bytes, bits reversed.
func vncKey(password string) []byte {
	key := make([]byte, 8)
	copy(key, password)
	for i, b := range key {
		var r byte
		for j := range 8 {
			r |= (b >> j & 1) << (7 - j)
		}
		key[i] = r
	}
	return key
}

func (c *client) vncAuth() (bool, error) {
	challenge := make([]byte, 16)
	if _, err := rand.Read(challenge); err != nil {
		return false, err
	}
	if _, err := c.conn.Write(challenge); err != nil {
		return false, err
	}
	resp := make([]byte, 16)
	if _, err := io.ReadFull(c.r, resp); err != nil {
		return false, err
	}
	block, err := des.NewCipher(vncKey(c.srv.opts.Password))
	if err != nil {
		return false, err
	}
	want := make([]byte, 16)
	block.Encrypt(want[:8], challenge[:8])
	block.Encrypt(want[8:], challenge[8:])
	return subtle.ConstantTimeCompare(want, resp) == 1, nil
}

func (c *client) reader() error {
	in := c.srv.opts.Input
	buf := make([]byte, 64)
	read := func(n int) ([]byte, error) {
		if cap(buf) < n {
			buf = make([]byte, n)
		}
		_, err := io.ReadFull(c.r, buf[:n])
		return buf[:n], err
	}
	for {
		t, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0: // SetPixelFormat
			b, err := read(19)
			if err != nil {
				return err
			}
			pf := unmarshalPixelFormat(b[3:])
			if !pf.TrueColor || (pf.BPP != 8 && pf.BPP != 16 && pf.BPP != 32) {
				return fmt.Errorf("unsupported pixel format %+v", pf)
			}
			c.mu.Lock()
			c.pf = pf
			c.mu.Unlock()

		case 2: // SetEncodings
			b, err := read(3)
			if err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(b[1:]))
			b, err = read(4 * n)
			if err != nil {
				return err
			}
			c.setEncodings(b)

		case 3: // FramebufferUpdateRequest
			b, err := read(9)
			if err != nil {
				return err
			}
			incremental := b[0] != 0
			x, y := int(binary.BigEndian.Uint16(b[1:])), int(binary.BigEndian.Uint16(b[3:]))
			w, h := int(binary.BigEndian.Uint16(b[5:])), int(binary.BigEndian.Uint16(b[7:]))
			c.mu.Lock()
			c.want = true
			if !incremental {
				c.dirty = c.dirty.Union(image.Rect(x, y, x+w, y+h))
			}
			c.mu.Unlock()
			c.signal()

		case 4: // KeyEvent
			b, err := read(7)
			if err != nil {
				return err
			}
			if in != nil {
				in.Key(binary.BigEndian.Uint32(b[3:]), b[0] != 0)
			}

		case 5: // PointerEvent
			b, err := read(5)
			if err != nil {
				return err
			}
			if in != nil {
				in.Pointer(b[0], int(binary.BigEndian.Uint16(b[1:])), int(binary.BigEndian.Uint16(b[3:])))
			}

		case 6: // ClientCutText, ignored
			b, err := read(7)
			if err != nil {
				return err
			}
			n := int32(binary.BigEndian.Uint32(b[3:]))
			if n < 0 { // extended clipboard
				n = -n
			}
			if _, err := io.CopyN(io.Discard, c.r, int64(n)); err != nil {
				return err
			}

		case 250: // XVP
			b, err := read(3)
			if err != nil {
				return err
			}
			c.xvpOp(b[1], PowerOp(b[2]))

		case 255: // QEMU client message
			sub, err := c.r.ReadByte()
			if err != nil {
				return err
			}
			if sub != 0 {
				return fmt.Errorf("unsupported QEMU message %d", sub)
			}
			b, err := read(10)
			if err != nil {
				return err
			}
			down := binary.BigEndian.Uint16(b) != 0
			keysym, code := binary.BigEndian.Uint32(b[2:]), binary.BigEndian.Uint32(b[6:])
			if in != nil {
				if code != 0 {
					in.Scancode(code, down)
				} else {
					in.Key(keysym, down)
				}
			}

		default:
			return fmt.Errorf("unsupported client message %d", t)
		}
	}
}

// xvpOp runs a power operation in the background; it may take seconds.
func (c *client) xvpOp(ver byte, op PowerOp) {
	power := c.srv.opts.Power
	if power == nil || ver != 1 {
		c.log.Warn("unsupported XVP operation", "version", ver, "op", op)
		c.send(250, 0, 1, xvpFail)
		return
	}
	c.log.Info("power operation", "op", op)
	go func() {
		if err := power(op); err != nil {
			c.log.Warn("power operation failed", "op", op, "err", err)
			c.send(250, 0, 1, xvpFail)
			return
		}
		c.log.Info("power operation done", "op", op)
	}()
}

func (c *client) setEncodings(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.encoding = -1
	c.desktopSize, c.cursor, c.qemuKeys, c.sentPseudo = false, false, false, false
	var names []string
	for i := 0; i+4 <= len(b); i += 4 {
		e := int32(binary.BigEndian.Uint32(b[i:]))
		switch e {
		case encRaw, encZlib:
			if c.encoding == -1 {
				c.encoding = e
			}
		case encDesktopSize:
			c.desktopSize = true
		case encCursor:
			c.cursor = true
		case encQEMUExtendedKeyEvent:
			c.qemuKeys = true
		case encXVP:
			if c.srv.opts.Power != nil && !c.xvp {
				c.xvp = true
				c.out = append(c.out, 250, 0, 1, xvpInit)
				c.signal()
			}
		}
		names = append(names, fmt.Sprint(e))
	}
	if c.encoding == -1 {
		c.encoding = encRaw
	}
	c.log.Debug("encodings", "client", strings.Join(names, ","), "using", c.encoding)
}

func (c *client) writer() {
	defer c.conn.Close()
	for {
		select {
		case <-c.closed:
			return
		case <-c.wake:
		}
		err := c.writeQueued()
		if err == nil {
			err = c.maybeUpdate()
		}
		if err != nil {
			c.log.Info("write failed", "err", err)
			return
		}
	}
}

// writeQueued writes the server messages queued by send.
func (c *client) writeQueued() error {
	c.mu.Lock()
	out := c.out
	c.out = nil
	c.mu.Unlock()
	if len(out) == 0 {
		return nil
	}
	if _, err := c.w.Write(out); err != nil {
		return err
	}
	return c.w.Flush()
}

type rect struct {
	r   image.Rectangle
	enc int32
}

// maybeUpdate sends one FramebufferUpdate if a request is outstanding and
// there is something to send.
func (c *client) maybeUpdate() error {
	d := c.srv.opts.Display
	w, h := d.Size()

	c.mu.Lock()
	if !c.want {
		c.mu.Unlock()
		return nil
	}
	var rects []rect
	if !c.sentPseudo {
		if c.qemuKeys {
			rects = append(rects, rect{enc: encQEMUExtendedKeyEvent})
		}
		if c.cursor {
			rects = append(rects, rect{r: image.Rect(arrowHotX, arrowHotY, arrowHotX+arrowW, arrowHotY+arrowH), enc: encCursor})
		}
		c.sentPseudo = true
	}
	if c.needResize || w != c.w0 || h != c.h0 {
		c.needResize = false
		if w != c.w0 || h != c.h0 {
			if c.desktopSize {
				rects = append(rects, rect{r: image.Rect(0, 0, w, h), enc: encDesktopSize})
			} else {
				c.log.Warn("display resized but viewer does not support DesktopSize", "width", w, "height", h)
			}
			c.w0, c.h0 = w, h
			c.dirty = image.Rect(0, 0, w, h)
		}
	}
	dirty := c.dirty.Intersect(image.Rect(0, 0, w, h))
	c.dirty = image.Rectangle{}
	if !dirty.Empty() {
		rects = append(rects, rect{r: dirty, enc: c.encoding})
	}
	if len(rects) == 0 {
		c.mu.Unlock()
		return nil
	}
	c.want = false
	pf := c.pf
	c.mu.Unlock()

	hdr := []byte{0, 0}
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(len(rects)))
	if _, err := c.w.Write(hdr); err != nil {
		return err
	}
	var pix []uint32
	var out []byte
	for _, rc := range rects {
		r := rc.r
		if rc.enc == encRaw || rc.enc == encZlib {
			n := r.Dx() * r.Dy()
			pix = slices.Grow(pix[:0], n)[:n]
			r = d.Read(r, pix)
			pix = pix[:r.Dx()*r.Dy()]
		}
		rh := make([]byte, 0, 12)
		rh = binary.BigEndian.AppendUint16(rh, uint16(r.Min.X))
		rh = binary.BigEndian.AppendUint16(rh, uint16(r.Min.Y))
		rh = binary.BigEndian.AppendUint16(rh, uint16(r.Dx()))
		rh = binary.BigEndian.AppendUint16(rh, uint16(r.Dy()))
		rh = binary.BigEndian.AppendUint32(rh, uint32(rc.enc))
		if _, err := c.w.Write(rh); err != nil {
			return err
		}
		switch rc.enc {
		case encRaw:
			out = out[:0]
			for _, p := range pix {
				out = pf.encode(out, p)
			}
			if _, err := c.w.Write(out); err != nil {
				return err
			}
		case encZlib:
			if err := c.writeZlib(pf, pix); err != nil {
				return err
			}
		case encCursor:
			if _, err := c.w.Write(arrowCursor(pf)); err != nil {
				return err
			}
		}
	}
	return c.w.Flush()
}

func (c *client) writeZlib(pf PixelFormat, pix []uint32) error {
	c.zbuf.Reset()
	if c.zw == nil {
		c.zw, _ = zlib.NewWriterLevel(&c.zbuf, zlib.BestSpeed)
	}
	raw := make([]byte, 0, len(pix)*pf.bytesPerPixel())
	for _, p := range pix {
		raw = pf.encode(raw, p)
	}
	if _, err := c.zw.Write(raw); err != nil {
		return err
	}
	if err := c.zw.Flush(); err != nil {
		return err
	}
	if err := binary.Write(c.w, binary.BigEndian, uint32(c.zbuf.Len())); err != nil {
		return err
	}
	_, err := c.w.Write(c.zbuf.Bytes())
	return err
}
