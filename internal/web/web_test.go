package web

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(func(c net.Conn) {
		defer c.Close()
		_, _ = io.Copy(c, c)
	}, nil))
	t.Cleanup(srv.Close)
	return srv
}

func dialWS(t *testing.T, srv *httptest.Server, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/websockify",
		&websocket.DialOptions{HTTPHeader: h, Subprotocols: []string{"binary"}})
}

func TestWebSocketEcho(t *testing.T) {
	srv := echoServer(t)
	c, _, err := dialWS(t, srv, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if c.Subprotocol() != "binary" {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload := bytes.Repeat([]byte("rfb"), 100)
	if err := c.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for len(got) < len(payload) {
		typ, p, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("message type %v", typ)
		}
		got = append(got, p...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("echo mismatch")
	}
	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketRejectsCrossOrigin(t *testing.T) {
	srv := echoServer(t)
	_, resp, err := dialWS(t, srv, "https://evil.example")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("resp %v, err %v", resp, err)
	}
}

func TestStaticWithoutNoVNC(t *testing.T) {
	srv := echoServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, err = http.Get(srv.URL + "/.gitkeep")
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf(".gitkeep: %v %v", resp.Status, err)
	}
	resp.Body.Close()
}

func TestInjectScript(t *testing.T) {
	got, ok := injectScript([]byte("<html><body>x</body></html>"))
	if want := "<html><body>x" + iloScript + "\n</body></html>"; !ok || string(got) != want {
		t.Fatalf("got %q", got)
	}
	if _, ok := injectScript([]byte("<html>")); ok {
		t.Fatal("injected without </body>")
	}
}

func TestILOJS(t *testing.T) {
	srv := echoServer(t)
	resp, err := http.Get(srv.URL + "/ilo.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") ||
		!bytes.Contains(b, []byte(`import UI from "./app/ui.js"`)) {
		t.Fatalf("%s %s %q", resp.Status, resp.Header.Get("Content-Type"), b)
	}
}
