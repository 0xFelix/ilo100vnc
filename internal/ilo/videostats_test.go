package ilo

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestVideoStatsSummary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	v := &videoStats{}
	mode := VesaMode{Mode: 3, Width: 640, Height: 400}

	v.count(BitBlt{Type: 0x107, FontHeight: 16, FontWidth: 8})
	v.count(TextCursor{Col: 2, Row: 1, Start: 13, Stop: 14})
	v.count(SetPalette{})
	v.flush(log, mode)
	line := buf.String()
	for _, want := range []string{"mode=0x3", "text_blits=1", "palettes=1", "font=8x16", `cursor="2,1 lines 13-14"`} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}

	// silence is reported once, then not again until traffic resumes
	buf.Reset()
	v.flush(log, mode)
	v.flush(log, mode)
	if n := strings.Count(buf.String(), "quiet"); n != 1 {
		t.Fatalf("quiet logged %d times: %s", n, buf.String())
	}
	buf.Reset()
	v.count(StandbyPower{})
	v.flush(log, mode)
	if !strings.Contains(buf.String(), "standby=true") || strings.Contains(buf.String(), "font=") {
		t.Fatalf("standby line: %s", buf.String())
	}
}
