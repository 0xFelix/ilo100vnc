package ilo

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// page mirrors the applet markup of a real firmware 4.26 kvms.html.
const page = `<APPLET CODE="com.serverengines.mahogany.MahoganyViewer.class" mayscript="true"ARCHIVE = "M2.JAR" WIDTH="400" HEIGHT="250"alt="x"><PARAM NAME="NonSecure_KVMPort" VALUE="80"> <PARAM NAME="sessiontype" VALUE="kvm" ><PARAM NAME="port" VALUE="5901" > <PARAM NAME="ipaddress" VALUE="bmc.example" > <PARAM NAME="sessiontype" VALUE="kvm" ><PARAM NAME="httpdata" VALUE="TOKEN" > <PARAM NAME="username" VALUE="admin" ></APPLET>`

func TestParseParams(t *testing.T) {
	p, err := ParseParams(page)
	if err != nil {
		t.Fatal(err)
	}
	want := Params{Port: 80, Username: "admin", Token: "TOKEN", PageHost: "bmc.example"}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
	p, err = ParseParams(strings.Replace(page, `VALUE="kvm"`, `VALUE="KVMSSL"`, 1))
	if err != nil || !p.SSL {
		t.Fatalf("kvmssl not detected: %+v %v", p, err)
	}
	if _, err := Dial(context.Background(), p, nil, SessionOptions{}); !errors.Is(err, ErrKVMSSL) {
		t.Fatalf("Dial with kvmssl: got %v, want ErrKVMSSL", err)
	}
	if _, err := ParseParams("<html>login</html>"); err == nil {
		t.Fatal("expected error without httpdata")
	}
}

func TestLoginBasic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, pw, ok := r.BasicAuth()
		if !ok || u != "admin" || pw != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="BMC"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != KVMPage {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, page)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	p, err := Login(context.Background(), srv.Client(), base, "admin", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if p.Token != "TOKEN" || p.Host != "127.0.0.1" || p.Port != 80 {
		t.Fatalf("%+v", p)
	}
	if _, err := Login(context.Background(), srv.Client(), base, "admin", "wrong"); err == nil {
		t.Fatal("expected failure with wrong password")
	}
}

func TestLoginDigest(t *testing.T) {
	const realm, nonce = "BMC", "1a2b3c"
	h := func(s string) string { x := md5.Sum([]byte(s)); return hex.EncodeToString(x[:]) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		scheme, rest, _ := strings.Cut(auth, " ")
		if scheme != "Digest" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s"`, realm, nonce))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c := parseAuthParams(rest)
		ha1 := h("admin:" + realm + ":secret")
		ha2 := h("GET:" + c["uri"])
		want := h(ha1 + ":" + nonce + ":" + c["nc"] + ":" + c["cnonce"] + ":" + c["qop"] + ":" + ha2)
		if c["response"] != want || c["uri"] != KVMPage || c["username"] != "admin" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, page)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	if _, err := Login(context.Background(), srv.Client(), base, "admin", "secret"); err != nil {
		t.Fatal(err)
	}
}

func TestParseAuthParams(t *testing.T) {
	got := parseAuthParams(`realm="BMC, Inc", qop="auth,auth-int", nonce=abc, algorithm=MD5`)
	if got["realm"] != "BMC, Inc" || got["qop"] != "auth,auth-int" || got["nonce"] != "abc" || got["algorithm"] != "MD5" {
		t.Fatalf("%v", got)
	}
}
