package slogor

import "strconv"

// sgrCode represents an SGR parameter.
type sgrCode int

// ANSI codes for text styling and formatting.
const (
	// https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_(Select_Graphic_Rendition)_parameters
	reset           = sgrCode(0)
	bold            = sgrCode(1)
	faint           = sgrCode(2)
	underline       = sgrCode(4)
	normalIntensity = sgrCode(22)
	// https://en.wikipedia.org/wiki/ANSI_escape_code#3-bit_and_4-bit
	fgRed     = sgrCode(31)
	fgGreen   = sgrCode(32)
	fgYellow  = sgrCode(33)
	fgBlue    = sgrCode(34)
	fgMagenta = sgrCode(35)
	fgCyan    = sgrCode(36)
)

func (c sgrCode) AppendTo(buf []byte) []byte {
	buf = append(buf, '\033', '[')
	buf = strconv.AppendInt(buf, int64(c), 10)

	return append(buf, 'm')
}
