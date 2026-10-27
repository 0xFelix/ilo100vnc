package ilo

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// pipeSession returns a Session whose sent bytes arrive on the channel.
func pipeSession(t *testing.T) (*Session, <-chan []byte) {
	c, peer := net.Pipe()
	t.Cleanup(func() { c.Close(); peer.Close() })
	sent := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 512)
		for {
			n, err := peer.Read(buf)
			if err != nil {
				return
			}
			sent <- append([]byte(nil), buf[:n]...)
		}
	}()
	s := &Session{
		conn: c,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		fb:   NewFramebuffer(),
	}
	return s, sent
}

// collect returns everything sent within d.
func collect(sent <-chan []byte, d time.Duration) []byte {
	var out []byte
	timeout := time.After(d)
	for {
		select {
		case b := <-sent:
			out = append(out, b...)
		case <-timeout:
			return out
		}
	}
}

func viewOnly(users ...User) MultiUserState {
	// as seen on firmware 4.26: may take control, status view-only
	m := MultiUserState{Flags: 0x00101EEF}
	copy(m.Users[:], users)
	return m
}

func TestRequestControlWhenAlone(t *testing.T) {
	s, sent := pipeSession(t)
	if err := s.handle(viewOnly(User{Flags: 0x81, Name: "admin"}), "admin", "x"); err != nil {
		t.Fatal(err)
	}
	want := bytes.Join([][]byte{
		EncodeAbsoluteMode(true), EncodeRelativeMode(false, false),
		EncodeRequestVesaMode(), EncodeRequestPrimaryControl(),
	}, nil)
	if got := collect(sent, 100*time.Millisecond); !bytes.Equal(got, want) {
		t.Fatalf("sent % X\nwant % X", got, want)
	}
	// repeated view-only state while waiting: no second request
	if err := s.handle(viewOnly(User{Flags: 0x81, Name: "admin"}), "admin", "x"); err != nil {
		t.Fatal(err)
	}
	if got := collect(sent, 50*time.Millisecond); len(got) != 0 {
		t.Fatalf("unexpected % X", got)
	}
	// granted
	full := MultiUserState{Flags: 0x00101EE7}
	full.Users[0] = User{Flags: 0x83, Name: "admin"}
	if err := s.handle(full, "admin", "x"); err != nil {
		t.Fatal(err)
	}
	if got := collect(sent, 50*time.Millisecond); !bytes.Equal(got, EncodeRequestKeyIndicators()) {
		t.Fatalf("sent % X", got)
	}
	if !s.HasControl() {
		t.Fatal("no control")
	}
}

func TestNoControlRequestWithOtherUser(t *testing.T) {
	s, sent := pipeSession(t)
	m := viewOnly(User{Flags: 0x81, Name: "admin"}, User{Flags: 0x03, Name: "bob"})
	if err := s.handle(m, "admin", "x"); err != nil {
		t.Fatal(err)
	}
	got := collect(sent, 100*time.Millisecond)
	if bytes.Contains(got, EncodeRequestPrimaryControl()) {
		t.Fatalf("requested control from another user: % X", got)
	}
}

func TestUnusableVesaMode(t *testing.T) {
	s, sent := pipeSession(t)
	if err := s.handle(VesaMode{Mode: 3, Width: 640, Height: 400}, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := collect(sent, 50*time.Millisecond); !bytes.Equal(got, EncodeInvalidate(Region{W: 80, H: 25})) {
		t.Fatalf("sent % X", got)
	}
	// seen during POST on firmware 4.26
	if err := s.handle(VesaMode{Mode: 0x200, Width: 8, Height: 1, BPP: 8}, "", ""); err != nil {
		t.Fatal(err)
	}
	if w, h := s.fb.Size(); w != 640 || h != 400 {
		t.Fatalf("framebuffer resized to %dx%d", w, h)
	}
	// like the original viewer: nothing is sent
	if got := collect(sent, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("unexpected % X", got)
	}
}

func TestVesaModeUsable(t *testing.T) {
	for _, tc := range []struct {
		m    VesaMode
		want bool
	}{
		{VesaMode{Mode: 3, Width: 640, Height: 400}, true},
		{VesaMode{Mode: 0x13, Width: 320, Height: 200, BPP: 8}, true},
		{VesaMode{Mode: 0x117, Width: 1024, Height: 768, BPP: 16}, true},
		{VesaMode{Mode: 0x200, Width: 8, Height: 1, BPP: 8}, false},
		{VesaMode{Mode: 3, Width: 0, Height: 400}, false},
		{VesaMode{Mode: 0x118, Width: 5000, Height: 768, BPP: 32}, false},
	} {
		if got := tc.m.Usable(); got != tc.want {
			t.Errorf("%+v: got %v", tc.m, got)
		}
	}
}
