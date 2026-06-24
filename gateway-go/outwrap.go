package main

import "strings"

// outProc is a small output-side (WS->TCP) filter that:
//   1. converts a bare LF to CR LF (ONLCR), and
//   2. emulates an N-column terminal's auto-wrap by tracking the cursor column and inserting a
//      newline when a printable character would pass column N.
//
// Why (2): some apps (notably Rogue) redraw the whole screen as a single N-column "blob" that
// relies on the terminal auto-wrapping at column N (the CP/M console is 80 wide). On a wider
// real terminal this blob does not wrap where expected -> the display "staircases" and runs
// past 80 columns. Forcing the wrap at N reproduces an N-column console.
//
// Column tracking follows CSI cursor-position (CUP "H"/"f"), horizontal-absolute ("G"/"`"),
// cursor forward/back ("C"/"D"), and CR/LF/BS/TAB/printables. SGR/erase/DSR are treated as
// no-move; ANY other escape sets the column to "unknown" (0), which safely disables wrap
// injection until the next CUP/CR/LF re-establishes a known column.
type outProc struct {
	cols int  // wrap width; 0 disables wrapping
	crlf bool // convert bare LF -> CR LF

	col   int  // 1-based current column; 0 = unknown
	inEsc bool // saw ESC, deciding the kind
	inCSI bool // inside an ESC[ ... sequence
	csi   []byte
}

func newOutProc(cols int, crlf bool) *outProc {
	return &outProc{cols: cols, crlf: crlf, col: 1}
}

func (p *outProc) process(data []byte) []byte {
	out := make([]byte, 0, len(data)+16)
	for _, b := range data {
		switch {
		case p.inCSI:
			p.csi = append(p.csi, b)
			out = append(out, b)
			if b >= 0x40 && b <= 0x7e { // CSI final byte
				p.endCSI(b)
				p.inCSI = false
			}
		case p.inEsc:
			out = append(out, b)
			if b == '[' {
				p.inCSI = true
				p.csi = p.csi[:0]
			} else {
				p.col = 0 // 2-char escape / OSC start: cursor position uncertain
			}
			p.inEsc = false
		case b == 0x1b: // ESC
			out = append(out, b)
			p.inEsc = true
		case b == 0x0d: // CR
			out = append(out, b)
			p.col = 1
		case b == 0x0a: // LF
			if p.crlf {
				out = append(out, 0x0d, 0x0a)
			} else {
				out = append(out, b)
			}
			p.col = 1
		case b == 0x08: // BS
			out = append(out, b)
			if p.col > 1 {
				p.col--
			}
		case b == 0x09: // TAB
			out = append(out, b)
			if p.col > 0 {
				p.col = ((p.col-1)/8+1)*8 + 1
			}
		case b >= 0x20: // printable (incl. high bytes)
			if p.cols > 0 && p.col > 0 && p.col > p.cols {
				if p.crlf {
					out = append(out, 0x0d)
				}
				out = append(out, 0x0a)
				p.col = 1
			}
			out = append(out, b)
			if p.col > 0 {
				p.col++
			}
		default: // other control bytes
			out = append(out, b)
		}
	}
	return out
}

func (p *outProc) endCSI(final byte) {
	switch final {
	case 'H', 'f': // CUP: ESC[row;colH
		_, col := csiParams2(p.csi)
		if col < 1 {
			col = 1
		}
		p.col = col
	case 'G', '`': // CHA: ESC[colG
		col := csiParam1(p.csi)
		if col < 1 {
			col = 1
		}
		p.col = col
	case 'C': // cursor forward
		if p.col > 0 {
			p.col += max(1, csiParam1(p.csi))
		}
	case 'D': // cursor back
		if p.col > 0 {
			p.col -= max(1, csiParam1(p.csi))
			if p.col < 1 {
				p.col = 1
			}
		}
	case 'm', 'K', 'J', 'n': // SGR / erase / status report: no horizontal move
	default:
		p.col = 0 // unknown effect -> disable wrap until a CUP/CR/LF re-syncs
	}
}

// csiParams2 returns the first two numeric params of a CSI sequence (csi includes the final
// byte). Missing params are 0.
func csiParams2(csi []byte) (int, int) {
	if len(csi) == 0 {
		return 0, 0
	}
	parts := strings.Split(string(csi[:len(csi)-1]), ";")
	a, b := 0, 0
	if len(parts) >= 1 {
		a = atoiDefault(parts[0], 0)
	}
	if len(parts) >= 2 {
		b = atoiDefault(parts[1], 0)
	}
	return a, b
}

func csiParam1(csi []byte) int {
	a, _ := csiParams2(csi)
	return a
}

func atoiDefault(s string, d int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return d
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return d
		}
		n = n*10 + int(c-'0')
	}
	return n
}
