package ilo

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeILO serves kvms.html (Basic auth, new token per load) and a KVM port
// that checks the client handshake and then sends a video mode and one tile.
type fakeILO struct {
	t       *testing.T
	web     *httptest.Server
	kvm     net.Listener
	tokens  atomic.Int32
	gotKeys chan []byte
}

func newFakeILO(t *testing.T) *fakeILO {
	f := &fakeILO{t: t, gotKeys: make(chan []byte, 10)}
	var err error
	if f.kvm, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	port := f.kvm.Addr().(*net.TCPAddr).Port
	f.web = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "pw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="BMC"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := f.tokens.Add(1)
		fmt.Fprintf(w, `<PARAM NAME="NonSecure_KVMPort" VALUE="%d"><PARAM NAME="sessiontype" VALUE="kvm" >`+
			`<PARAM NAME="httpdata" VALUE="%s" ><PARAM NAME="username" VALUE="admin" >`, port, token(n))
	}))
	go f.serveKVM()
	t.Cleanup(func() { f.web.Close(); f.kvm.Close() })
	return f
}

func token(n int32) string { return fmt.Sprintf("%096x", n) }

func (f *fakeILO) serveKVM() {
	c, err := f.kvm.Accept()
	if err != nil {
		return
	}
	defer c.Close()
	expect := func(want []byte) bool {
		got := make([]byte, len(want))
		if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, want) {
			f.t.Errorf("kvm: got % X want % X (%v)", got, want, err)
			return false
		}
		return true
	}
	send := func(b []byte) {
		if _, err := c.Write(b); err != nil {
			f.t.Errorf("kvm write: %v", err)
		}
	}
	if !expect(EncodeClientNOP()) {
		return
	}
	hs := make([]byte, 33)
	hs[0] = msgServerHandshake
	copy(hs[1:], "2.0 kvms")
	send(hs)
	// the token of the most recent page load must be used
	if !expect(EncodeClientHandshake(SignatureEmbedded, "admin", token(f.tokens.Load()), RightAll)) {
		return
	}
	send([]byte{msgFirmwareVersion, 4, 0, 0, 0, '4', '.', '2', '6'})
	send([]byte{msgMultiUserState, 0, 0, 0, 0, 5, 0, 0x81, 0})
	send([]byte("admin"))
	if !expect(bytes.Join([][]byte{
		EncodeAbsoluteMode(true), EncodeRelativeMode(false, false),
		EncodeRequestVesaMode(), EncodeRequestKeyIndicators(),
	}, nil)) {
		return
	}
	// 16bpp 320x200, then one raw 2x2 tile at the top left
	send([]byte{msgInformVesaMode, 0x11, 0x01, 0x40, 0x01, 200, 0, 16, 0})
	if !expect(EncodeInvalidate(Region{W: 320, H: 200})) {
		return
	}
	e := make([]byte, 1+20+64*8)
	e[0] = msgEnhanceBitBlt
	e[1], e[2] = 0xF0, 0x01                                       // type 0x1F0
	e[3], e[4] = 2, 2                                             // tile 2x2
	e[17] = 8                                                     // payload size
	e[21] = 1                                                     // lo[0] bit 0
	e = append(e, 0x00, 0xF8, 0x00, 0xF8, 0x00, 0xF8, 0x00, 0xF8) // red
	send(e)
	// forward everything the console sends from now on
	buf := make([]byte, 256)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		f.gotKeys <- append([]byte(nil), buf[:n]...)
	}
}

func TestConsoleEndToEnd(t *testing.T) {
	f := newFakeILO(t)
	u, _ := url.Parse(f.web.URL)
	c := NewConsole(Config{
		URL: u, User: "admin", Password: "pw",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	updates := make(chan Update, 10)
	c.OnUpdate(func(u Update) { updates <- u })
	c.Attach()
	defer c.Close()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			if u.Dirty != image.Rect(0, 0, 2, 2) {
				continue
			}
			if w, h := c.Framebuffer().Size(); w != 320 || h != 200 {
				t.Fatalf("size %dx%d", w, h)
			}
			if p := pixel(c.Framebuffer(), 1, 1); p != 0xFF0000 {
				t.Fatalf("pixel %06X", p)
			}
			// input now flows to the iLO as HID usages
			c.Key('a', true)
			select {
			case b := <-f.gotKeys:
				if !bytes.Equal(b, EncodeKey(0x04, true)) {
					t.Fatalf("key message % X", b)
				}
			case <-deadline:
				t.Fatal("no key message")
			}
			return
		case <-deadline:
			t.Fatal("timeout waiting for the tile update")
		}
	}
}

func TestConsoleLoginFailure(t *testing.T) {
	f := newFakeILO(t)
	u, _ := url.Parse(f.web.URL)
	_, err := Login(t.Context(), http.DefaultClient, u, "admin", "nope")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err %v", err)
	}
}

// sentMessages runs input through f with a session that has full control
// and returns what was sent once want bytes arrived.
func sentMessages(t *testing.T, relative bool, want []byte, f func(*Console)) []byte {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	c := NewConsole(Config{RelativeMouse: relative, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	c.sess = &Session{conn: a, log: c.log, ready: true} // status 0: full control
	go f(c)
	got := make([]byte, len(want))
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func pointerMessages(t *testing.T, relative bool, want []byte, events ...[3]int) []byte {
	t.Helper()
	return sentMessages(t, relative, want, func(c *Console) {
		for _, e := range events {
			c.Pointer(uint8(e[0]), e[1], e[2])
		}
	})
}

func TestKeyRepeat(t *testing.T) {
	want := bytes.Join([][]byte{
		// auto-repeated presses are sent once
		EncodeKey(0x52, true),
		EncodeKey(0x52, false),
		// a release without press is still sent (clears a key stuck in the iLO)
		EncodeKey(0x51, false),
		EncodeKey(0x52, true),
	}, nil)
	got := sentMessages(t, false, want, func(c *Console) {
		c.Scancode(0xC8, true)
		c.Scancode(0xC8, true)
		c.Scancode(0xC8, true)
		c.Scancode(0xC8, false)
		c.Scancode(0xD0, false)
		c.Scancode(0xC8, true)
	})
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X\nwant % X", got, want)
	}
}

func TestPointerAbsolute(t *testing.T) {
	want := bytes.Join([][]byte{
		EncodeMouseMove(10, 20),
		// same position again: nothing
		EncodeButtonsAbsolute(10, 20, Buttons{Left: true}),
		EncodeButtonsAbsolute(12, 20, Buttons{Wheel: 1}),
	}, nil)
	got := pointerMessages(t, false, want, [3]int{0, 10, 20}, [3]int{0, 10, 20},
		[3]int{maskLeft, 10, 20}, [3]int{maskWheelDown, 12, 20})
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X\nwant % X", got, want)
	}
}

func TestPointerRelative(t *testing.T) {
	want := bytes.Join([][]byte{
		// first event only sets the reference position
		EncodeMouseMove(5, -2),
		EncodeButtonsRelative(1, 0, Buttons{Left: true}),
	}, nil)
	got := pointerMessages(t, true, want, [3]int{0, 10, 20}, [3]int{0, 15, 18},
		[3]int{0, 15, 18}, [3]int{maskLeft, 16, 18})
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X\nwant % X", got, want)
	}
}
