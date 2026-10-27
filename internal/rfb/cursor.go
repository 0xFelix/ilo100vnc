package rfb

// The iLO host draws its mouse pointer as a hardware cursor that is not part
// of the framebuffer, so viewers get this local arrow via the Cursor
// pseudo-encoding. With absolute mouse mode it matches the host pointer.

var arrow = []string{
	"X...........",
	"XX..........",
	"XOX.........",
	"XOOX........",
	"XOOOX.......",
	"XOOOOX......",
	"XOOOOOX.....",
	"XOOOOOOX....",
	"XOOOOOOOX...",
	"XOOOOOOOOX..",
	"XOOOOOOOOOX.",
	"XOOOOOOXXXXX",
	"XOOOXOOX....",
	"XOOXXOOX....",
	"XOX..XOOX...",
	"XX...XOOX...",
	"X.....XOOX..",
	"......XOOX..",
	".......XX...",
}

const (
	arrowW, arrowH       = 12, 19
	arrowHotX, arrowHotY = 0, 0
)

// arrowCursor returns the Cursor pseudo-encoding payload: pixels in pf,
// then a 1-bit mask, rows padded to whole bytes.
func arrowCursor(pf PixelFormat) []byte {
	var px []byte
	stride := (arrowW + 7) / 8
	mask := make([]byte, stride*arrowH)
	for y, row := range arrow {
		for x, c := range row {
			var v uint32
			if c == 'O' {
				v = 0xFFFFFF
			}
			px = pf.encode(px, v)
			if c != '.' {
				mask[y*stride+x/8] |= 0x80 >> (x % 8)
			}
		}
	}
	return append(px, mask...)
}
