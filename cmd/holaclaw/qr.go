package main

import (
	"io"
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the margin around a code, in modules. The standard asks for four;
// two still scan, and a terminal is short on rows.
const qrQuiet = 2

// writeQR prints text as a QR code, two module rows per line. Light modules
// are drawn and dark ones left blank, so the code reads on a dark terminal,
// the usual one. Text too long to encode prints nothing.
func writeQR(w io.Writer, text string) {
	c, err := qr.Encode(text, qr.L)
	if err != nil {
		return
	}
	light := func(x, y int) bool { return !c.Black(x, y) } // outside the code is the quiet zone
	var sb strings.Builder
	for y := -qrQuiet; y < c.Size+qrQuiet; y += 2 {
		sb.WriteString("  ")
		for x := -qrQuiet; x < c.Size+qrQuiet; x++ {
			top, bottom := light(x, y), light(x, y+1)
			switch {
			case top && bottom:
				sb.WriteString("█")
			case top:
				sb.WriteString("▀")
			case bottom:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\n")
	}
	_, _ = io.WriteString(w, sb.String())
}
