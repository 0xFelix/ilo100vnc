package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigPrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(file, []byte(`{"host": "file", "user": "fileuser", "listen": ":1",
		"vnc": ":2", "debug": true, "vnc_password": "filevnc"}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envPrefix+"USER", "envuser")
	t.Setenv(envPrefix+"VNC", ":3")
	t.Setenv(envPrefix+"RELATIVE_MOUSE", "true")
	t.Setenv(envPrefix+"PASSWORD", "envpw")

	cfg, _, err := loadConfig([]string{"-config", file, "-vnc", ":4", "-debug=false"})
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		Host:          "file",    // file
		User:          "envuser", // env over file
		Password:      "envpw",   // env only
		Listen:        ":1",      // file over default
		VNC:           ":4",      // flag over env and file
		VNCPassword:   "filevnc", // file
		IdleTimeout:   "30s",     // default
		RelativeMouse: true,      // env
		Debug:         false,     // flag over file, even at its default value
	}
	if cfg != want {
		t.Fatalf("got  %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfigBadEnv(t *testing.T) {
	t.Setenv(envPrefix+"DEBUG", "maybe")
	_, _, err := loadConfig(nil)
	if err == nil || !strings.Contains(err.Error(), envPrefix+"DEBUG") {
		t.Fatalf("err %v", err)
	}
}

func TestLoadConfigUnknownKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{"bogus": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfig([]string{"-config", file}); err == nil {
		t.Fatal("no error for unknown key")
	}
}
