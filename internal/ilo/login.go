package ilo

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// KVMPage is the path of the page that embeds the viewer applet.
const KVMPage = "/kvms.html"

// Params are the applet parameters from the KVM page (PROTOCOL.md 2.3).
type Params struct {
	Host     string // host to connect to (from the iLO URL, not the page)
	Port     int    // NonSecure_KVMPort
	SSL      bool   // sessiontype == "kvmssl"
	Username string
	Token    string // httpdata
	PageHost string // ipaddress PARAM, informational
}

var paramRe = regexp.MustCompile(`(?i)<PARAM\s+NAME\s*=\s*"([^"]*)"\s+VALUE\s*=\s*"([^"]*)"`)

// ParseParams extracts the applet parameters from the KVM page HTML.
func ParseParams(page string) (Params, error) {
	p := Params{}
	seen := map[string]string{}
	for _, m := range paramRe.FindAllStringSubmatch(page, -1) {
		// the page repeats sessiontype; like Applet.getParameter, first wins
		k := strings.ToLower(m[1])
		if _, dup := seen[k]; !dup {
			seen[k] = html.UnescapeString(m[2])
		}
	}
	tok, ok := seen["httpdata"]
	if !ok || tok == "" {
		return p, fmt.Errorf("no httpdata parameter in KVM page (KVM not licensed, or login failed?)")
	}
	p.Token = tok
	p.Username = seen["username"]
	p.PageHost = seen["ipaddress"]
	p.SSL = strings.EqualFold(seen["sessiontype"], "kvmssl")
	var err error
	if p.Port, err = strconv.Atoi(seen["nonsecure_kvmport"]); err != nil || p.Port <= 0 || p.Port > 65535 {
		return p, fmt.Errorf("invalid NonSecure_KVMPort %q", seen["nonsecure_kvmport"])
	}
	return p, nil
}

// Login fetches the KVM page with HTTP Basic or Digest authentication and
// returns the applet parameters. A fresh token is issued on every call.
func Login(ctx context.Context, client *http.Client, base *url.URL, user, password string) (Params, error) {
	u := base.ResolveReference(&url.URL{Path: KVMPage})
	get := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return client.Do(req)
	}

	resp, err := get("")
	if err != nil {
		return Params{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Values("WWW-Authenticate")
		drain(resp)
		auth, err := authorization(challenge, user, password, http.MethodGet, u.RequestURI())
		if err != nil {
			return Params{}, err
		}
		if resp, err = get(auth); err != nil {
			return Params{}, err
		}
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return Params{}, fmt.Errorf("GET %s: %s", KVMPage, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Params{}, err
	}
	p, err := ParseParams(string(body))
	if err != nil {
		return p, err
	}
	p.Host = base.Hostname()
	return p, nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// authorization answers the strongest supported challenge.
func authorization(challenges []string, user, password, method, uri string) (string, error) {
	var basic bool
	for _, c := range challenges {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(c), " ")
		switch strings.ToLower(scheme) {
		case "digest":
			return digestAuth(parseAuthParams(rest), user, password, method, uri)
		case "basic":
			basic = true
		}
	}
	if basic {
		req := http.Request{Header: http.Header{}}
		req.SetBasicAuth(user, password)
		return req.Header.Get("Authorization"), nil
	}
	return "", fmt.Errorf("unsupported authentication challenge %q", challenges)
}

// parseAuthParams parses key=value and key="value" pairs.
func parseAuthParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,\t")
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				v, s = rest[1:], ""
			} else {
				v, s = rest[1:1+end], rest[2+end:]
			}
		} else {
			v, s, _ = strings.Cut(rest, ",")
			v = strings.TrimSpace(v)
		}
		out[k] = v
	}
	return out
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// digestAuth implements RFC 2617 Digest with MD5 / MD5-sess and qop=auth.
func digestAuth(c map[string]string, user, password, method, uri string) (string, error) {
	realm, nonce := c["realm"], c["nonce"]
	if nonce == "" {
		return "", fmt.Errorf("digest challenge without nonce")
	}
	alg := c["algorithm"]
	if alg != "" && !strings.EqualFold(alg, "MD5") && !strings.EqualFold(alg, "MD5-sess") {
		return "", fmt.Errorf("unsupported digest algorithm %q", alg)
	}
	cb := make([]byte, 8)
	if _, err := rand.Read(cb); err != nil {
		return "", err
	}
	cnonce := hex.EncodeToString(cb)
	const nc = "00000001"

	ha1 := md5hex(user + ":" + realm + ":" + password)
	if strings.EqualFold(alg, "MD5-sess") {
		ha1 = md5hex(ha1 + ":" + nonce + ":" + cnonce)
	}
	ha2 := md5hex(method + ":" + uri)

	qop := ""
	for q := range strings.SplitSeq(c["qop"], ",") {
		if strings.TrimSpace(q) == "auth" {
			qop = "auth"
		}
	}
	var resp string
	if qop != "" {
		resp = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		resp = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
		user, realm, nonce, uri, resp)
	if alg != "" {
		fmt.Fprintf(&b, ", algorithm=%s", alg)
	}
	if op := c["opaque"]; op != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, op)
	}
	if qop != "" {
		fmt.Fprintf(&b, `, qop=%s, nc=%s, cnonce="%s"`, qop, nc, cnonce)
	}
	return b.String(), nil
}
