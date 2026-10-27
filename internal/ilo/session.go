package ilo

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	traceLimit    = 256                    // bytes of each received body included in debug dumps
	blinkInterval = 500 * time.Millisecond // text cursor/blink phase, as in the original viewer
	statsInterval = 2 * time.Second        // debug video summary
)

// Session is one KVM connection to the iLO.
type Session struct {
	conn  net.Conn
	log   *slog.Logger
	debug bool
	fb    *Framebuffer

	wmu sync.Mutex

	mu        sync.Mutex
	state     MultiUserState
	ready     bool
	relative  bool
	onUpdate  func(Update)
	onReady   func()
	mode      VesaMode
	stats     *videoStats // debug only
	requested bool        // full control requested, awaiting the answer
	noControl string      // why input is disabled, logged once per reason
}

// SessionOptions configure Dial.
type SessionOptions struct {
	Relative bool // relative instead of absolute mouse
	Log      *slog.Logger
	Debug    bool // hex dump all traffic
	OnUpdate func(Update)
	OnReady  func()
}

// ErrKVMSSL is returned for an encrypted KVM session (sessiontype kvmssl).
// The iLO 100 only offers SSLv3/TLS 1.0 with DHE-DSS-3DES, which Go does
// not support.
var ErrKVMSSL = errors.New("the iLO asks for an encrypted KVM session (kvmssl), which is not supported")

// Dial connects to the KVM port and sends the initial NOP.
func Dial(ctx context.Context, p Params, fb *Framebuffer, o SessionOptions) (*Session, error) {
	if p.SSL {
		return nil, ErrKVMSSL
	}
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Session{
		conn:     conn,
		log:      o.Log,
		debug:    o.Debug,
		fb:       fb,
		relative: o.Relative,
		onUpdate: o.OnUpdate,
		onReady:  o.OnReady,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.log.Info("connected to KVM port", "addr", addr)
	if err := s.Send(EncodeClientNOP()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

// Send writes one encoded client message.
func (s *Session) Send(b []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.debug {
		s.log.Debug(fmt.Sprintf("tx 0x%02X len=%d\n%s", b[0], len(b)-1, hex.Dump(redact(b))))
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := s.conn.Write(b)
	return err
}

// redact masks the secret fields of a ClientHandshake for logging.
func redact(b []byte) []byte {
	if len(b) != 189 || b[0] != msgClientHandshake {
		return b
	}
	r := append([]byte(nil), b...)
	for _, f := range [][2]int{{37, 57}, {61, 189}} { // password, full password
		for i := f[0]; i < f[1]; i++ {
			if r[i] != 0 {
				r[i] = '*'
			}
		}
	}
	return r
}

// Ready reports whether the handshake has completed.
func (s *Session) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// HasControl reports whether this session may send input.
func (s *Session) HasControl() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready && s.state.UserStatus() == UserStatusFullControl
}

// Close politely disconnects and closes the socket.
func (s *Session) Close() error {
	_ = s.Send(EncodeClientDisconnect(0, "Exit with success"))
	return s.conn.Close()
}

// Run reads and handles server messages until the connection fails or ctx
// is canceled. token is sent in the handshake.
func (s *Session) Run(ctx context.Context, user, token string) error {
	stop := context.AfterFunc(ctx, func() { _ = s.conn.Close() })
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.blink(ctx)
	if s.debug {
		s.stats = &videoStats{}
		go s.summarize(ctx)
	}

	rd := NewReader(s.conn)
	if s.debug {
		rd.Trace = func(id byte, body []byte) {
			n := len(body)
			if n > traceLimit {
				body = body[:traceLimit]
			}
			s.log.Debug(fmt.Sprintf("rx 0x%02X len=%d\n%s", id, n, hex.Dump(append([]byte{id}, body...))))
		}
	}
	for {
		msg, err := rd.Next()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if err := s.handle(msg, user, token); err != nil {
			return err
		}
	}
}

// ErrServerDisconnect is wrapped by Run when the iLO ends the session.
var ErrServerDisconnect = errors.New("server disconnected")

func (s *Session) handle(msg any, user, token string) error {
	if s.stats != nil {
		s.stats.count(msg)
	}
	switch m := msg.(type) {
	case ServerHandshake:
		s.log.Info("server handshake", "version", m.Version,
			"video", m.Video, "keyboard", m.Keyboard, "mouse", m.Mouse, "storage", m.Storage)
		if !m.Video {
			s.log.Warn("iLO did not grant video (license or permissions?)")
		}
		return s.Send(EncodeClientHandshake(SignatureEmbedded, user, token, RightAll))

	case FirmwareVersion:
		s.log.Info("firmware version", "version", m.Version)

	case MultiUserState:
		return s.onMultiUserState(m)

	case ServerDisconnect:
		return fmt.Errorf("%w: reason %d: %s", ErrServerDisconnect, m.Reason, m.Message)

	case StorageStatus:
		s.log.Debug("storage status ignored (virtual media not implemented)")

	case VesaMode:
		if !m.Usable() {
			return s.onUnusableMode(m)
		}
		u, _ := s.fb.Apply(m)
		s.update(u)
		s.mu.Lock()
		s.mode = m
		s.mu.Unlock()
		s.log.Info("video mode", modeAttrs(m)...)
		return s.Send(EncodeInvalidate(s.fb.FullRegion()))

	default:
		u, err := s.fb.Apply(msg)
		if err != nil {
			// a malformed blit only damages the picture, the stream is
			// still in sync because the length was known
			s.log.Warn("decode error", "type", fmt.Sprintf("%T", msg), "err", err)
		}
		s.update(u)
	}
	return nil
}

// update passes a screen change on to OnUpdate.
func (s *Session) update(u Update) {
	if (u.Resized || !u.Dirty.Empty()) && s.onUpdate != nil {
		s.onUpdate(u)
	}
}

// onUnusableMode shows a black screen at the current size. The original
// viewer resizes to the reported (tiny) size, which is effectively blank
// too; the iLO sends no picture until the next mode change.
func (s *Session) onUnusableMode(m VesaMode) error {
	s.mu.Lock()
	changed := m != s.mode
	s.mode = m
	s.mu.Unlock()
	if changed {
		s.log.Warn("iLO reports an unusable video mode, it cannot capture this screen", modeAttrs(m)...)
	}
	s.update(s.fb.Blank())
	return nil
}

// summarize logs the video summary every statsInterval until ctx ends.
func (s *Session) summarize(ctx context.Context) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			mode := s.mode
			s.mu.Unlock()
			s.stats.flush(s.log, mode)
		}
	}
}

// blink toggles the text cursor and blinking text until ctx ends.
func (s *Session) blink(ctx context.Context) {
	t := time.NewTicker(blinkInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.update(s.fb.ToggleBlink())
		}
	}
}

func modeAttrs(m VesaMode) []any {
	return []any{"mode", fmt.Sprintf("0x%X", uint16(m.Mode)), "width", m.Width, "height", m.Height, "bpp", m.BPP}
}

func (s *Session) onMultiUserState(m MultiUserState) error {
	s.mu.Lock()
	first := !s.ready
	prev := s.state
	s.state, s.ready = m, true
	if m.UserStatus() == UserStatusFullControl {
		s.requested, s.noControl = false, ""
	}
	s.mu.Unlock()

	status := m.UserStatus()
	if first || status != prev.UserStatus() {
		names := []string{}
		for _, u := range m.Users {
			if u.Valid() {
				names = append(names, u.Name)
			}
		}
		s.log.Info("session state", "status", statusName(status), "flags", fmt.Sprintf("0x%08X", m.Flags), "users", names)
	}
	if status == UserStatusDisconnect {
		return fmt.Errorf("%w: forcibly disconnected by another user", ErrServerDisconnect)
	}
	var msgs [][]byte
	if first {
		msgs = append(msgs,
			EncodeAbsoluteMode(!s.relative),
			EncodeRelativeMode(s.relative, false),
			EncodeRequestVesaMode())
	}
	if status == UserStatusFullControl {
		if first || prev.UserStatus() != UserStatusFullControl {
			msgs = append(msgs, EncodeRequestKeyIndicators())
		}
	} else if b := s.requestControl(m); b != nil {
		msgs = append(msgs, b)
	}
	for _, b := range msgs {
		if err := s.Send(b); err != nil {
			return err
		}
	}
	if first && s.onReady != nil {
		s.onReady()
	}
	return nil
}

// requestControl returns a RequestPrimaryControl message if this session
// may take full control and no other user is connected. The iLO starts new
// sessions view-only even when they are alone; the original viewer then
// offers to request control.
func (s *Session) requestControl(m MultiUserState) []byte {
	var others []string
	for _, u := range m.Users {
		if u.Valid() && !u.Self() {
			others = append(others, u.Name)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var reason string
	switch {
	case s.requested:
		return nil
	case m.Flags&FlagCanTakeFullControl == 0:
		reason = "not allowed to take full control"
	case len(others) > 0:
		reason = "another user is connected: " + strings.Join(others, ", ")
	default:
		s.log.Info("requesting full control")
		s.requested, s.noControl = true, ""
		return EncodeRequestPrimaryControl()
	}
	if reason != s.noControl {
		s.log.Warn("input is disabled", "reason", reason)
		s.noControl = reason
	}
	return nil
}

func statusName(v int) string {
	switch v {
	case UserStatusFullControl:
		return "full-control"
	case UserStatusViewOnly:
		return "view-only"
	case UserStatusEphemeral:
		return "ephemeral"
	}
	return "disconnect"
}
