package ilo

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Config configures a Console.
type Config struct {
	URL      *url.URL // iLO web interface, e.g. http://10.0.0.5
	User     string
	Password string

	HTTPClient *http.Client // for the web login

	// IdleTimeout disconnects from the iLO this long after the last viewer
	// left. Zero keeps the session open once established.
	IdleTimeout time.Duration
	// RelativeMouse uses relative instead of absolute pointer reports.
	RelativeMouse bool

	Log   *slog.Logger
	Debug bool
}

// Console owns the decoded screen and the (re)connecting iLO session, and
// translates viewer input into iLO messages.
type Console struct {
	cfg Config
	log *slog.Logger
	fb  *Framebuffer

	mu       sync.Mutex
	viewers  int
	cancel   context.CancelFunc
	done     chan struct{}
	idle     *time.Timer
	sess     *Session
	onUpdate func(Update)

	// input state, guarded by mu
	buttons  Buttons
	lastX    int
	lastY    int
	havePos  bool
	keysDown map[uint16]int
}

// NewConsole creates a console. Nothing connects until Attach.
func NewConsole(cfg Config) *Console {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Console{
		cfg:      cfg,
		log:      cfg.Log,
		fb:       NewFramebuffer(),
		keysDown: map[uint16]int{},
	}
}

// Framebuffer returns the decoded screen.
func (c *Console) Framebuffer() *Framebuffer { return c.fb }

// OnUpdate registers the function called after every screen change.
func (c *Console) OnUpdate(fn func(Update)) {
	c.mu.Lock()
	c.onUpdate = fn
	c.mu.Unlock()
}

func (c *Console) notify(u Update) {
	c.mu.Lock()
	fn := c.onUpdate
	c.mu.Unlock()
	if fn != nil {
		fn(u)
	}
}

// Attach registers a viewer and starts the iLO session if needed.
func (c *Console) Attach() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.viewers++
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	if c.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel, c.done = cancel, make(chan struct{})
		go c.run(ctx, c.done)
	}
}

// Detach unregisters a viewer. The session is closed after IdleTimeout
// once no viewer is left.
func (c *Console) Detach() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.viewers--
	if c.viewers > 0 || c.cfg.IdleTimeout <= 0 || c.cancel == nil {
		return
	}
	c.idle = time.AfterFunc(c.cfg.IdleTimeout, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.viewers == 0 && c.cancel != nil {
			c.log.Info("no viewers left, closing iLO session")
			c.cancel()
			c.cancel = nil
		}
	})
}

// Close stops the session and waits for it to end.
func (c *Console) Close() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (c *Console) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		c.log.Error("iLO session ended", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Console) session(ctx context.Context) error {
	p, err := Login(ctx, c.cfg.HTTPClient, c.cfg.URL, c.cfg.User, c.cfg.Password)
	if err != nil {
		return err
	}
	c.log.Info("logged in", "kvm_port", p.Port, "ssl", p.SSL, "page_host", p.PageHost)
	if c.cfg.Debug {
		c.log.Debug("kvm token", "len", len(p.Token))
	}
	s, err := Dial(ctx, p, c.fb, SessionOptions{
		Relative: c.cfg.RelativeMouse,
		Log:      c.log,
		Debug:    c.cfg.Debug,
		OnUpdate: c.notify,
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sess = s
	c.havePos = false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.sess = nil
		c.mu.Unlock()
		_ = s.Close()
	}()
	user := p.Username
	if user == "" {
		user = c.cfg.User
	}
	err = s.Run(ctx, user, p.Token)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// send delivers an input message if a session with control exists.
// Must be called with c.mu held.
func (c *Console) send(b []byte) {
	if c.sess == nil || !c.sess.HasControl() {
		return
	}
	if err := c.sess.Send(b); err != nil {
		c.log.Warn("send failed", "err", err)
	}
}

// Key sends a key press or release for an X11 keysym.
func (c *Console) Key(keysym uint32, down bool) {
	usage := KeysymToHID(keysym)
	if usage == 0 {
		c.log.Debug("unmapped keysym", "keysym", keysym)
		return
	}
	c.key(usage, down)
}

// Scancode sends a key press or release for a QEMU qnum scancode.
func (c *Console) Scancode(code uint32, down bool) {
	usage := ScancodeToHID(code)
	if usage == 0 {
		c.log.Debug("unmapped scancode", "code", code)
		return
	}
	c.key(usage, down)
}

func (c *Console) key(usage uint16, down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if down {
		c.keysDown[usage]++
	} else if c.keysDown[usage] > 0 {
		c.keysDown[usage]--
		if c.keysDown[usage] == 0 {
			delete(c.keysDown, usage)
		}
	}
	c.send(EncodeKey(usage, down))
}

// ReleaseAll releases all keys and buttons, used when a viewer leaves.
func (c *Console) ReleaseAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for usage := range c.keysDown {
		c.send(EncodeKey(usage, false))
	}
	clear(c.keysDown)
	if c.buttons != (Buttons{}) {
		c.buttons = Buttons{}
		c.send(EncodeButtonsAbsolute(int32(c.lastX), int32(c.lastY), c.buttons))
	}
}

// RFB pointer mask bits.
const (
	maskLeft      = 1 << 0
	maskMiddle    = 1 << 1
	maskRight     = 1 << 2
	maskWheelUp   = 1 << 3
	maskWheelDown = 1 << 4
)

// Pointer handles an RFB pointer event at framebuffer position x, y.
func (c *Console) Pointer(mask uint8, x, y int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, h := c.fb.Size()
	x, y = max(0, min(x, w-1)), max(0, min(y, h-1))

	bt := Buttons{Left: mask&maskLeft != 0, Middle: mask&maskMiddle != 0, Right: mask&maskRight != 0}
	wheel := 0
	if mask&maskWheelUp != 0 {
		wheel = -1
	} else if mask&maskWheelDown != 0 {
		wheel = 1
	}

	// absolute: send the position; relative: the delta to the last event
	px, py, moved := x, y, !c.havePos || x != c.lastX || y != c.lastY
	encode := EncodeButtonsAbsolute
	if c.cfg.RelativeMouse {
		px, py, encode = 0, 0, EncodeButtonsRelative
		if c.havePos {
			px, py = x-c.lastX, y-c.lastY
		}
		moved = px != 0 || py != 0
	}
	c.lastX, c.lastY, c.havePos = x, y, true

	switch {
	case bt != c.buttons || wheel != 0:
		c.buttons = bt
		bt.Wheel = wheel
		c.send(encode(int32(px), int32(py), bt))
	case moved:
		c.send(EncodeMouseMove(int32(px), int32(py)))
	}
}
