package web

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func dialWS(t *testing.T, srv *httptest.Server, origin string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET /websockify HTTP/1.1\r\nHost: " + host + "\r\n" +
		"Upgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary\r\n"
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return c, br, resp.Status + "|" + resp.Header.Get("Sec-WebSocket-Accept") + "|" + resp.Header.Get("Sec-WebSocket-Protocol")
}

func frame(op byte, payload []byte) []byte {
	mask := [4]byte{1, 2, 3, 4}
	b := []byte{0x80 | op}
	if len(payload) < 126 {
		b = append(b, 0x80|byte(len(payload)))
	} else {
		b = append(b, 0x80|126)
		b = binary.BigEndian.AppendUint16(b, uint16(len(payload)))
	}
	b = append(b, mask[:]...)
	for i, c := range payload {
		b = append(b, c^mask[i%4])
	}
	return b
}

func write(t *testing.T, c net.Conn, b []byte) {
	t.Helper()
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, r *bufio.Reader) (byte, []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		t.Fatal(err)
	}
	n := int(h[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			t.Fatal(err)
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		t.Fatal(err)
	}
	return h[0] & 0x0F, p
}

func TestWebSocketEcho(t *testing.T) {
	srv := echoServer(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	c, r, status := dialWS(t, srv, "http://"+host)
	defer c.Close()
	// RFC 6455 section 1.3 sample key/accept pair
	if status != "101 Switching Protocols|s3pPLMBiTxaQ9kYGzzhZRbK+xOo=|binary" {
		t.Fatalf("status %q", status)
	}

	payload := bytes.Repeat([]byte("rfb"), 100) // 300 bytes, 16-bit length
	write(t, c, frame(opBinary, payload))
	var got []byte
	for len(got) < len(payload) {
		op, p := readFrame(t, r)
		if op != opBinary {
			t.Fatalf("opcode %d", op)
		}
		got = append(got, p...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("echo mismatch")
	}

	write(t, c, frame(opPing, []byte("hi")))
	if op, p := readFrame(t, r); op != opPong || string(p) != "hi" {
		t.Fatalf("pong %d %q", op, p)
	}

	write(t, c, frame(opClose, []byte{0x03, 0xE8}))
	if op, _ := readFrame(t, r); op != opClose {
		t.Fatalf("close reply %d", op)
	}
}

func TestWebSocketRejectsCrossOrigin(t *testing.T) {
	srv := echoServer(t)
	c, _, status := dialWS(t, srv, "https://evil.example")
	defer c.Close()
	if !strings.HasPrefix(status, "403") {
		t.Fatalf("status %q", status)
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
