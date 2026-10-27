package ilo

import (
	"fmt"
	"log/slog"
	"sync"
)

// videoStats counts video messages for the periodic debug summary, which
// shows at a glance what the iLO sends during each boot stage (the iLO
// only sends text for 8, 12 or 16 line fonts).
type videoStats struct {
	mu                                         sync.Mutex
	text, rect, tiles, palettes, cursors, bits int
	standby                                    bool
	cursor                                     TextCursor
	fontH, fontW                               uint8
	quiet                                      bool
}

func (v *videoStats) count(msg any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch m := msg.(type) {
	case BitBlt:
		if m.Type >= bltTextFirst && m.Type <= bltTextCombo {
			v.text++
			v.fontH, v.fontW = m.FontHeight, m.FontWidth
		} else {
			v.rect++
		}
	case EnhanceBitBlt:
		v.tiles++
	case SetPalette:
		v.palettes++
	case TextCursor:
		v.cursors++
		v.cursor = m
	case SpecialGraphicsBit:
		v.bits++
	case StandbyPower:
		v.standby = true
	}
}

// flush logs one line for the counts since the last call and resets them.
// An interval without video messages is logged once, as "quiet".
func (v *videoStats) flush(log *slog.Logger, mode VesaMode) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.text+v.rect+v.tiles+v.palettes+v.cursors+v.bits == 0 && !v.standby {
		if !v.quiet {
			log.Debug("video summary: quiet", modeAttrs(mode)...)
			v.quiet = true
		}
		return
	}
	attrs := append(modeAttrs(mode),
		"text_blits", v.text, "rect_blits", v.rect, "tiles", v.tiles,
		"palettes", v.palettes, "cursors", v.cursors, "special_bits", v.bits)
	if v.standby {
		attrs = append(attrs, "standby", true)
	}
	if v.text > 0 {
		attrs = append(attrs, "font", fmt.Sprintf("%dx%d", v.fontW, v.fontH))
	}
	if v.cursors > 0 {
		c := v.cursor
		if c.Start == c.Stop {
			attrs = append(attrs, "cursor", "hidden")
		} else {
			attrs = append(attrs, "cursor", fmt.Sprintf("%d,%d lines %d-%d", c.Col, c.Row, c.Start, c.Stop))
		}
	}
	log.Debug("video summary", attrs...)
	v.text, v.rect, v.tiles, v.palettes, v.cursors, v.bits = 0, 0, 0, 0, 0, 0
	v.standby, v.quiet = false, false
}
