package web

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Minimal RFC 6455 server, enough to carry RFB as binary messages.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA

	maxControlPayload = 125
	maxFramePayload   = 16 << 20
)

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// sameOrigin rejects cross-site WebSocket hijacking: browsers always send
// Origin, which must match the Host the page was served from.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // not a browser
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// upgrade performs the WebSocket handshake and returns the connection.
func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if r.Method != http.MethodGet ||
		!headerHasToken(r.Header, "Connection", "upgrade") ||
		!headerHasToken(r.Header, "Upgrade", "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("not a websocket request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("unsupported websocket version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if k, err := base64.StdEncoding.DecodeString(key); err != nil || len(k) != 16 {
		http.Error(w, "bad Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("bad websocket key")
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin websocket rejected", http.StatusForbidden)
		return nil, fmt.Errorf("origin %q does not match host %q", r.Header.Get("Origin"), r.Host)
	}
	proto := ""
	if headerHasToken(r.Header, "Sec-WebSocket-Protocol", "binary") {
		proto = "binary"
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return nil, errors.New("hijacking not supported")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n"
	if proto != "" {
		resp += "Sec-WebSocket-Protocol: " + proto + "\r\n"
	}
	resp += "\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{Conn: conn, r: brw.Reader}, nil
}

// wsConn exposes the binary message stream as a net.Conn.
type wsConn struct {
	net.Conn
	r *bufio.Reader

	wmu sync.Mutex

	remaining uint64 // payload bytes left in the current data frame
	mask      [4]byte
	maskPos   int
	closed    bool
}

func (c *wsConn) readHeader() (op byte, fin bool, n uint64, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.r, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	if h[0]&0x70 != 0 {
		return 0, false, 0, errors.New("websocket: reserved bits set")
	}
	if h[1]&0x80 == 0 {
		return 0, false, 0, errors.New("websocket: client frame not masked")
	}
	n = uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if _, err = io.ReadFull(c.r, c.mask[:]); err != nil {
		return
	}
	c.maskPos = 0
	return op, fin, n, nil
}

func (c *wsConn) unmask(b []byte) {
	for i := range b {
		b[i] ^= c.mask[c.maskPos&3]
		c.maskPos++
	}
}

// Read returns payload bytes of binary/text/continuation frames and handles
// control frames transparently.
func (c *wsConn) Read(p []byte) (int, error) {
	for c.remaining == 0 {
		if c.closed {
			return 0, io.EOF
		}
		op, _, n, err := c.readHeader()
		if err != nil {
			return 0, err
		}
		switch op {
		case opBinary, opText, opContinuation:
			if n > maxFramePayload {
				return 0, errors.New("websocket: frame too large")
			}
			c.remaining = n
		case opClose, opPing, opPong:
			if n > maxControlPayload {
				return 0, errors.New("websocket: control frame too large")
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(c.r, payload); err != nil {
				return 0, err
			}
			c.unmask(payload)
			switch op {
			case opPing:
				if err := c.writeFrame(opPong, payload); err != nil {
					return 0, err
				}
			case opClose:
				c.closed = true
				_ = c.writeFrame(opClose, payload[:min(len(payload), 2)])
				return 0, io.EOF
			}
		default:
			return 0, fmt.Errorf("websocket: unknown opcode %d", op)
		}
	}
	if uint64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.unmask(p[:n])
	c.remaining -= uint64(n)
	return n, err
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	h := make([]byte, 0, 10)
	h = append(h, 0x80|op)
	switch n := len(payload); {
	case n < 126:
		h = append(h, byte(n))
	case n <= 0xFFFF:
		h = append(h, 126)
		h = binary.BigEndian.AppendUint16(h, uint16(n))
	default:
		h = append(h, 127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	bufs := net.Buffers{h, payload}
	_, err := bufs.WriteTo(c.Conn)
	return err
}

// Write sends p as one binary message.
func (c *wsConn) Write(p []byte) (int, error) {
	if err := c.writeFrame(opBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close sends a normal closure and closes the socket.
func (c *wsConn) Close() error {
	_ = c.writeFrame(opClose, []byte{0x03, 0xE8})
	return c.Conn.Close()
}
