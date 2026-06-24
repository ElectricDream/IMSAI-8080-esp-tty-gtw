package main

import "sync"

// TerminalInput is the per-session inbound processor: Telnet IAC handling plus
// line-ending normalization.
//
// Process(data) returns (toWS, toClient):
//   - toWS     : cleaned console bytes to forward to the /tty WebSocket.
//   - toClient : Telnet negotiation replies to write back to the TCP socket.
//
// Initial() returns negotiation bytes to send on connect (telnet mode only).

// Telnet (RFC 854/855) constants.
const (
	tIAC  = 255
	tDONT = 254
	tDO   = 253
	tWONT = 252
	tWILL = 251
	tSB   = 250
	tSE   = 240

	optECHO = 1
	optSGA  = 3
	optNAWS = 31
)

// IAC parser states.
const (
	stData = iota
	stIAC
	stOpt
	stSB
	stSBIAC
)

type TerminalInput struct {
	mode string
	eol  string

	// telnet parser
	state     int
	cmd       byte
	sb        []byte
	announced bool
	will      map[byte]bool
	do        map[byte]bool

	// eol normalization
	lastCR bool

	// client terminal size from NAWS (RFC 1073), guarded by mu for cross-goroutine reads.
	mu      sync.Mutex
	cols    int
	rows    int
	hasSize bool
}

// Size returns the last terminal size reported by the client via Telnet NAWS.
// ok is false until a NAWS subnegotiation has been received.
func (t *TerminalInput) Size() (cols, rows int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows, t.hasSize
}

func NewTerminalInput(mode, eol string) *TerminalInput {
	return &TerminalInput{
		mode:  mode,
		eol:   eol,
		state: stData,
		will:  map[byte]bool{},
		do:    map[byte]bool{},
	}
}

func (t *TerminalInput) Initial() []byte {
	// Announce WILL ECHO / WILL SGA proactively on connect in BOTH telnet and auto mode, so the
	// client switches to character/no-echo mode BEFORE the user's first keystroke. Some telnet
	// clients negotiate lazily (only on first input); without a proactive announce that first
	// keypress is handled in line mode (echoed locally as `^\` and buffered, not delivered), so
	// the hotkey appeared to need two presses at startup. Only "raw" sends no negotiation.
	if t.mode == "telnet" || t.mode == "auto" {
		return t.announce()
	}
	return nil
}

// announce offers server-side echo + char mode so the client drops local echo/line mode.
func (t *TerminalInput) announce() []byte {
	if t.announced {
		return nil
	}
	t.announced = true
	t.will[optECHO] = true
	t.will[optSGA] = true
	return []byte{tIAC, tWILL, optECHO, tIAC, tWILL, optSGA}
}

// negotiate replies to a client DO/DONT/WILL/WONT, avoiding negotiation loops.
func (t *TerminalInput) negotiate(cmd, opt byte) []byte {
	switch cmd {
	case tDO:
		if opt == optECHO || opt == optSGA {
			if t.will[opt] {
				return nil
			}
			t.will[opt] = true
			return []byte{tIAC, tWILL, opt}
		}
		return []byte{tIAC, tWONT, opt}
	case tDONT:
		if t.will[opt] {
			delete(t.will, opt)
			return []byte{tIAC, tWONT, opt}
		}
		return nil
	case tWILL:
		if opt == optSGA || opt == optNAWS {
			if t.do[opt] {
				return nil
			}
			t.do[opt] = true
			return []byte{tIAC, tDO, opt}
		}
		return []byte{tIAC, tDONT, opt}
	case tWONT:
		if t.do[opt] {
			delete(t.do, opt)
			return []byte{tIAC, tDONT, opt}
		}
		return nil
	}
	return nil
}

func (t *TerminalInput) Process(data []byte) (toWS, toClient []byte) {
	raw := make([]byte, 0, len(data))
	rep := make([]byte, 0)
	if t.mode == "raw" {
		raw = append(raw, data...)
	} else {
		for _, b := range data {
			if r := t.feedTelnet(b, &raw); len(r) > 0 {
				rep = append(rep, r...)
			}
		}
	}
	return t.normalizeEOL(raw), rep
}

func (t *TerminalInput) feedTelnet(b byte, raw *[]byte) []byte {
	switch t.state {
	case stData:
		if b == tIAC {
			t.state = stIAC
			if t.mode == "auto" {
				return t.announce() // client is telnet; engage on first IAC
			}
		} else {
			*raw = append(*raw, b)
		}
	case stIAC:
		switch {
		case b == tIAC:
			*raw = append(*raw, tIAC) // escaped literal 0xFF data byte
			t.state = stData
		case b == tWILL || b == tWONT || b == tDO || b == tDONT:
			t.cmd = b
			t.state = stOpt
		case b == tSB:
			t.sb = t.sb[:0]
			t.state = stSB
		default:
			t.state = stData // standalone command (GA/NOP/...) - ignore
		}
	case stOpt:
		r := t.negotiate(t.cmd, b)
		t.state = stData
		return r
	case stSB:
		if b == tIAC {
			t.state = stSBIAC
		} else {
			t.sb = append(t.sb, b)
		}
	case stSBIAC:
		if b == tSE {
			t.handleSubneg() // end of subnegotiation (e.g. NAWS)
			t.state = stData
		} else {
			t.sb = append(t.sb, b) // escaped IAC IAC inside the subnegotiation
			t.state = stSB
		}
	}
	return nil
}

// handleSubneg processes a completed IAC SB ... IAC SE payload (t.sb starts with the option
// code). Currently only NAWS (window size) is decoded; other subnegotiations are ignored.
func (t *TerminalInput) handleSubneg() {
	// NAWS payload: optNAWS, width_hi, width_lo, height_hi, height_lo (16-bit big-endian).
	if len(t.sb) >= 5 && t.sb[0] == optNAWS {
		cols := int(t.sb[1])<<8 | int(t.sb[2])
		rows := int(t.sb[3])<<8 | int(t.sb[4])
		if cols <= 0 || rows <= 0 {
			return
		}
		t.mu.Lock()
		changed := !t.hasSize || cols != t.cols || rows != t.rows
		t.cols, t.rows, t.hasSize = cols, rows, true
		t.mu.Unlock()
		if changed {
			logf("DEBUG", "client terminal size: %dx%d", cols, rows)
		}
	}
}

func (t *TerminalInput) normalizeEOL(data []byte) []byte {
	if t.eol == "raw" {
		return data
	}
	out := make([]byte, 0, len(data))
	for _, b := range data {
		switch b {
		case 0x0D: // CR
			out = t.emitEOL(out)
			t.lastCR = true
		case 0x0A: // LF
			if !t.lastCR {
				out = t.emitEOL(out) // lone LF == Enter (Unix terminals)
			}
			t.lastCR = false // swallow the LF of a CR LF pair
		case 0x00: // NUL (telnet CR-NUL padding) - drop
			t.lastCR = false
		default:
			out = append(out, b)
			t.lastCR = false
		}
	}
	return out
}

func (t *TerminalInput) emitEOL(out []byte) []byte {
	switch t.eol {
	case "cr":
		return append(out, 0x0D)
	case "crlf":
		return append(out, 0x0D, 0x0A)
	case "lf":
		return append(out, 0x0A)
	}
	return out
}
