// Package web serves the embedded noVNC client and bridges its WebSocket
// connection to the RFB server.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
)

// noVNC is not committed; `make novnc` downloads it into ./novnc.
//
//go:embed all:novnc
var assets embed.FS

// iloJS extends the noVNC UI (power panel), see ilo.js.
//
//go:embed ilo.js
var iloJS []byte

const iloScript = `<script type="module" src="ilo.js"></script>`

// injectScript adds ilo.js to vnc.html, right before </body>.
func injectScript(html []byte) ([]byte, bool) {
	i := bytes.LastIndex(html, []byte("</body>"))
	if i < 0 {
		return html, false
	}
	out := make([]byte, 0, len(html)+len(iloScript)+1)
	out = append(out, html[:i]...)
	out = append(out, iloScript+"\n"...)
	return append(out, html[i:]...), true
}

// StartPath is where "/" redirects to: noVNC with sensible defaults.
const StartPath = "/vnc.html?autoconnect=true&reconnect=true&resize=scale&path=websockify"

// Handler returns the HTTP handler. serve runs the RFB protocol on an
// upgraded WebSocket connection and blocks until it ends.
func Handler(serve func(net.Conn), log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	static, _ := fs.Sub(assets, "novnc")
	_, err := fs.Stat(static, "vnc.html")
	haveNoVNC := err == nil
	if !haveNoVNC {
		log.Warn("noVNC is not embedded in this build (run `make novnc`), only the WebSocket endpoint is available")
	}
	files := http.FileServerFS(static)
	var vncHTML []byte
	if haveNoVNC {
		b, _ := fs.ReadFile(static, "vnc.html")
		var ok bool
		if vncHTML, ok = injectScript(b); !ok {
			log.Warn("vnc.html has no </body>, power buttons are not added")
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /websockify", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrade(w, r)
		if err != nil {
			log.Info("websocket upgrade failed", "remote", r.RemoteAddr, "err", err)
			return
		}
		serve(c)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !haveNoVNC {
			http.Error(w, "noVNC is not embedded in this build; connect a VNC client instead", http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, StartPath, http.StatusFound)
	})
	mux.HandleFunc("GET /vnc.html", func(w http.ResponseWriter, r *http.Request) {
		if !haveNoVNC {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(vncHTML)
	})
	mux.HandleFunc("GET /ilo.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(iloJS)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.gitkeep" {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		mux.ServeHTTP(w, r)
	})
}
