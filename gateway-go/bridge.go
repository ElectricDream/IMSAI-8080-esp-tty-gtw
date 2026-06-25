package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Gateway holds shared state. One goroutine per accepted TCP client.
type Gateway struct {
	cfg      Config
	mu       sync.Mutex
	sessions int
}

// dialWS opens the outbound /tty WebSocket. No automatic keepalive Ping is sent (gorilla
// does not ping on its own); the ESP /tty server drops the connection at ~20 s if pinged.
func dialWS(host string, port int) (*websocket.Conn, error) {
	url := fmt.Sprintf("ws://%s:%d/tty", host, port)
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	c, _, err := d.Dial(url, nil)
	return c, err
}

// enableTCPKeepAlive reaps half-open clients that vanish silently (no FIN/RST).
func enableTCPKeepAlive(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(60 * time.Second)
	}
}

const tuiOutBufMax = 256 * 1024 // cap on console output buffered while the TUI is open

// bridgeSession is the shared, mutable state of one client session. It lives for the whole
// client connection (across /tty reconnects) and coordinates the two pump goroutines, the
// modal TUI, and the persistent /cpa connection used by the panel pane.
type bridgeSession struct {
	cfg  Config
	conn net.Conn
	term *TerminalInput

	mu      sync.Mutex
	ws      *websocket.Conn // current /tty connection (changes on reconnect)
	tui     bool            // true while the control TUI owns the screen
	outBuf  []byte          // console output buffered while tui is true (replayed on exit)
	hkMatch int             // bytes of the hotkey matched so far (bridge mode)
	tuiM    *tuiModel       // TUI state, non-nil while tui is true

	// Front panel: /cpa opened ONCE at session start (while idle) and kept open. Opening it
	// during live /tty traffic makes the device drop /tty, so we never (re)open it on demand.
	cpa       *cpaConn
	cpaStatus string // latest 6-char status, updated by readCPALoop

	capFile *os.File // diagnostic: raw device-output capture (nil if disabled)
}

// readCPALoop drains /cpa continuously: it keeps the latest status string and discards '$'
// disk-activity pings, so the connection's buffer never backs up. The conn is passed in (not
// read from s.cpa) so closing/replacing s.cpa cannot race this goroutine. Exits on error/close.
func (s *bridgeSession) readCPALoop(c *cpaConn) {
	for {
		msg, err := c.readMessage()
		if err != nil {
			return
		}
		if len(msg) > 0 && (msg[0] == 'U' || msg[0] == 'D' || msg[0] == 'E') {
			s.mu.Lock()
			s.cpaStatus = msg
			s.mu.Unlock()
		}
	}
}

// openCPA opens /cpa AFTER /tty (so /tty stays the device's primary socket) and starts the
// drain loop. Best-effort: on failure the panel pane just shows unavailable.
func (s *bridgeSession) openCPA() {
	if !s.cfg.TUI || s.cpa != nil {
		return
	}
	c, err := dialCPA(s.cfg)
	if err != nil {
		logf("WARNING", "cpa connect failed: %v", err)
		return
	}
	s.cpa = c
	go s.readCPALoop(c)
}

// cpaState is a decoded /cpa status snapshot for the connection banner.
type cpaState struct {
	known bool // false if no status could be read (no /cpa, TUI off, or no reply)
	power bool // status[0] == 'U'
	run   bool // status[2] == 'R'
}

// probeCPA does a one-shot /cpa poll for the connection banner. It REUSES the persistent /cpa
// (no extra connection; reconnecting /cpa would restart the device web server). /cpa status:
// [0] U=Up/powered D=Down/off E=offline, [2] R=RUN.
func (s *bridgeSession) probeCPA() cpaState {
	if s.cpa == nil {
		return cpaState{}
	}
	if err := s.cpa.send("P"); err != nil {
		return cpaState{}
	}
	time.Sleep(cpaSettle) // let readCPALoop capture the reply
	s.mu.Lock()
	st := s.cpaStatus
	s.mu.Unlock()
	if len(st) < 3 {
		return cpaState{} // device does not answer /cpa "P" while powered off (use /system)
	}
	return cpaState{known: true, power: st[0] == 'U', run: st[2] == 'R'}
}

// machineState returns the power/run state for the banner. It prefers /cpa (authoritative while
// the machine runs) and falls back to HTTP GET /system when /cpa gives no status: the device's
// front-panel task does not answer /cpa "P" while powered off, but the HTTP server still reports
// state.power (0 = off). When /cpa is silent the machine is off, so RUN is reported off.
// known is false only if neither source answered.
func (s *bridgeSession) machineState() (power, run, known bool) {
	if cs := s.probeCPA(); cs.known {
		return cs.power, cs.run, true
	}
	if si, err := getSystem(s.cfg); err == nil {
		return si.State.Power != 0, false, true
	}
	return false, false, false
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// closeCPA closes /cpa and clears the cached status. Called before reopening /tty so the next
// /tty is opened before a fresh /cpa.
func (s *bridgeSession) closeCPA() {
	if s.cpa != nil {
		s.cpa.close()
		s.cpa = nil
	}
	s.mu.Lock()
	s.cpaStatus = ""
	s.mu.Unlock()
}

// pumpWSToTCP forwards WS (binary console output) -> TCP. While the TUI is open the output is
// buffered instead of written, so the TUI owns the screen. Returns the cause.
func (s *bridgeSession) pumpWSToTCP() string {
	var proc *outProc
	if s.cfg.CRLFOut || s.cfg.Cols > 0 {
		proc = newOutProc(s.cfg.Cols, s.cfg.CRLFOut)
	}
	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			return "device WS closed"
		}
		if s.capFile != nil {
			_, _ = s.capFile.Write(data) // raw device output, before any transform
		}
		if proc != nil {
			data = proc.process(data)
		}
		s.mu.Lock()
		if s.tui {
			s.outBuf = append(s.outBuf, data...)
			if len(s.outBuf) > tuiOutBufMax {
				s.outBuf = s.outBuf[len(s.outBuf)-tuiOutBufMax:]
			}
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		if _, werr := s.conn.Write(data); werr != nil {
			return "client TCP write failed"
		}
	}
}

// sendToWS writes keystrokes to the WS: one char per frame with optional throttle, or the
// whole chunk in a single frame when throttle is off.
func sendToWS(ws *websocket.Conn, data []byte, cfg Config) error {
	mt := websocket.BinaryMessage
	if cfg.Framing == "text" {
		mt = websocket.TextMessage
	}
	if !cfg.Throttle {
		return ws.WriteMessage(mt, data)
	}
	crDelay := time.Duration(cfg.CRDelay * float64(time.Second))
	charDelay := time.Duration(cfg.CharDelay * float64(time.Second))
	for _, b := range data {
		if err := ws.WriteMessage(mt, []byte{b}); err != nil {
			return err
		}
		if b == 13 {
			time.Sleep(crDelay)
		} else {
			time.Sleep(charDelay)
		}
	}
	return nil
}

// pumpTCPToWS forwards TCP (keystrokes) -> WS with Telnet/EOL normalization. The cleaned
// stream is dispatched through handleInput, which intercepts the TUI hotkey. Returns the cause.
func (s *bridgeSession) pumpTCPToWS() string {
	buf := make([]byte, 4096)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			toWS, toClient := s.term.Process(buf[:n])
			if len(toClient) > 0 {
				if _, werr := s.conn.Write(toClient); werr != nil {
					return "client TCP write failed"
				}
			}
			if rc := s.handleInput(toWS); rc != "" {
				return rc
			}
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return "stopped" // read deadline set by runBridge; not a client disconnect
			}
			if err == io.EOF {
				return "client closed connection"
			}
			return "client TCP read failed"
		}
	}
}

// handleInput dispatches cleaned client bytes: in TUI mode they drive the TUI; in bridge mode
// they are forwarded to the WS except the hotkey sequence, which opens the TUI. Returns a
// non-empty disconnect cause only if a WS write fails.
func (s *bridgeSession) handleInput(data []byte) string {
	hk := s.cfg.HotkeyBytes
	i := 0
	for i < len(data) {
		s.mu.Lock()
		inTUI := s.tui
		s.mu.Unlock()
		if inTUI {
			consumed, exited := s.tuiFeed(data[i:])
			i += consumed
			if exited {
				s.exitTUI()
			}
			continue
		}
		if len(hk) == 0 { // TUI disabled: forward everything
			if err := sendToWS(s.ws, data[i:], s.cfg); err != nil {
				return "device WS closed"
			}
			return ""
		}
		if s.hkMatch == 0 {
			idx := bytes.IndexByte(data[i:], hk[0])
			if idx < 0 {
				if err := sendToWS(s.ws, data[i:], s.cfg); err != nil {
					return "device WS closed"
				}
				return ""
			}
			if idx > 0 {
				if err := sendToWS(s.ws, data[i:i+idx], s.cfg); err != nil {
					return "device WS closed"
				}
			}
			i += idx + 1
			s.hkMatch = 1
			if len(hk) == 1 {
				s.hkMatch = 0
				s.enterTUI()
			}
		} else if data[i] == hk[s.hkMatch] {
			s.hkMatch++
			i++
			if s.hkMatch == len(hk) {
				s.hkMatch = 0
				s.enterTUI()
			}
		} else {
			// Broken partial match: the held prefix was not the hotkey -> forward it as data,
			// then re-examine the current byte on the next iteration.
			if err := sendToWS(s.ws, hk[:s.hkMatch], s.cfg); err != nil {
				return "device WS closed"
			}
			s.hkMatch = 0
		}
	}
	return ""
}

// runBridge runs both pumps on the given /tty connection until one ends; returns the first
// pump's disconnect cause and closes that WebSocket. The TCP connection and the session
// (incl. /cpa) are kept for a possible reconnect.
func (s *bridgeSession) runBridge(ws *websocket.Conn) string {
	s.ws = ws
	reasonCh := make(chan string, 2)
	go func() { reasonCh <- s.pumpWSToTCP() }()
	go func() { reasonCh <- s.pumpTCPToWS() }()

	reason := <-reasonCh
	// Unblock the still-running pump: closing the WS ends a blocked ReadMessage/WriteMessage;
	// a momentary read deadline ends a blocked conn.Read. Then collect the second result.
	ws.Close()
	if tc, ok := s.conn.(*net.TCPConn); ok {
		_ = tc.SetReadDeadline(time.Now())
	}
	<-reasonCh
	if tc, ok := s.conn.(*net.TCPConn); ok {
		_ = tc.SetReadDeadline(time.Time{}) // clear, so the conn is reusable on reconnect
	}
	// If we tore down while the TUI was open, restore the normal screen so any reconnect
	// messages / teardown happen on the console screen.
	s.mu.Lock()
	inTUI := s.tui
	s.tui = false
	s.tuiM = nil
	s.mu.Unlock()
	if inTUI {
		_, _ = s.conn.Write([]byte("\x1b[?25h\x1b[?7h\x1b[?1049l")) // show cursor, restore wrap, leave alt
	}
	return reason
}

func (g *Gateway) handleClient(conn net.Conn) {
	peer := conn.RemoteAddr().String()

	g.mu.Lock()
	if g.sessions >= g.cfg.MaxSessions {
		inUse := g.sessions
		g.mu.Unlock()
		logf("WARNING", "rejecting %s: %d/%d sessions in use", peer, inUse, g.cfg.MaxSessions)
		_, _ = conn.Write([]byte("\r\nGateway busy: console session already in use.\r\n"))
		_ = conn.Close()
		return
	}
	g.sessions++
	cur := g.sessions
	g.mu.Unlock()

	enableTCPKeepAlive(conn)
	started := time.Now()
	reason := "unknown"
	url := fmt.Sprintf("ws://%s:%d/tty", g.cfg.Host, g.cfg.WSPort)
	logf("INFO", "session up: %s <-> %s (%d/%d, mode=%s eol=%s)",
		peer, url, cur, g.cfg.MaxSessions, g.cfg.Mode, g.cfg.EOL)

	// One session for the whole client connection. Telnet/EOL state persists across /tty
	// reconnects (the client never renegotiates).
	s := &bridgeSession{cfg: g.cfg, conn: conn, term: NewTerminalInput(g.cfg.Mode, g.cfg.EOL)}
	defer s.closeCPA() // safety net if we break out while /cpa is open

	if g.cfg.Capture != "" {
		if f, err := os.OpenFile(g.cfg.Capture, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err != nil {
			logf("WARNING", "cannot open capture file %s: %v", g.cfg.Capture, err)
		} else {
			s.capFile = f
			defer f.Close()
			logf("INFO", "capturing device output to %s", g.cfg.Capture)
		}
	}

	attempt := 0
	announcedInit := false
	for {
		ws, err := dialWS(g.cfg.Host, g.cfg.WSPort)
		if err != nil {
			reason = "device unreachable"
			logf("WARNING", "WS connect failed for %s: %v", peer, err)
		} else {
			attempt = 0 // a successful connect resets the backoff
			if !announcedInit {
				if init := s.term.Initial(); len(init) > 0 {
					if _, e := conn.Write(init); e != nil {
						reason = "client TCP write failed"
						ws.Close()
						break
					}
				}
				// Best-effort: request the client window geometry (VT-100 is 80x24, the size the
				// CP/M console and the WebUI's xterm assume). Without this, a client narrower than
				// 80 columns wraps full-width output (e.g. Zork's reverse-video status line bleeds
				// onto the next row). XTWINOPS (ESC[8;rows;cols t) is honored by xterm-class
				// terminals; one that does not support it parses the CSI and ignores it silently,
				// so there is no garbage and no regression. Disable with --resize off.
				if g.cfg.ResizeCols > 0 {
					resize := fmt.Sprintf("\x1b[8;%d;%dt", g.cfg.ResizeRows, g.cfg.ResizeCols)
					if _, e := conn.Write([]byte(resize)); e != nil {
						reason = "client TCP write failed"
						ws.Close()
						break
					}
				}
				// Open /cpa ONCE, right after the first /tty connect (while idle), and KEEP it
				// for the whole session. The device allows one client per channel and RECONNECTING
				// /cpa restarts its web server, so we must never reopen it. Done before the banner
				// so we can probe the power state for it (reusing this same connection).
				s.openCPA()
				// Connection banner: confirms the terminal reached the gateway and the /tty
				// WebSocket is up, which helps when the IMSAI is OFF (ESP answers but the emulation
				// is not running, so no console output ever arrives and the screen stays blank).
				// When the TUI is enabled, also advertise the hotkey that opens it.
				banner := fmt.Sprintf("\r\nConnected to %s v%s\r\n", productName, version)
				if g.cfg.TUI {
					banner += fmt.Sprintf("Press %s for special functions (disks, panel, system, library).\r\n",
						g.cfg.Hotkey)
				}
				// Probe the machine state once (power + RUN) to tailor the console hint and the
				// status line. The /tty WS is active on connect, but CP/M only (re)prints its A>
				// prompt in response to input AND only while the CPU is running; a stopped or
				// powered-off machine produces nothing, so the hint must reflect that. (A known
				// state implies /cpa is open, which implies the TUI is enabled, so referencing the
				// Panel pane below is safe.) M0 spike: one CR -> "\r\r\nA>"; we suggest Enter
				// rather than injecting a CR (auto-input could disturb a running program).
				power, run, known := s.machineState()
				switch {
				case !known, power && run:
					banner += "If the screen is blank, press Enter to display the CP/M prompt.\r\n"
				case power && !run:
					banner += fmt.Sprintf("The CPU is stopped (RUN off). Set RUN from the front panel (%s -> Panel) to start it.\r\n", g.cfg.Hotkey)
				default: // powered off
					banner += "The machine is powered off; no CP/M console until it is powered on.\r\n"
				}
				// Machine state snapshot, shown last per the chosen banner layout.
				if known {
					banner += fmt.Sprintf("POWER is %s and RUN is %s\r\n", onOff(power), onOff(run))
				}
				if _, e := conn.Write([]byte(banner)); e != nil {
					reason = "client TCP write failed"
					ws.Close()
					break
				}
				announcedInit = true
			} else {
				_, _ = conn.Write([]byte("\r\n[gateway] reconnected.\r\n"))
			}
			reason = s.runBridge(ws)
		}

		// A client-side end (or reconnect disabled) stops the session.
		if strings.HasPrefix(reason, "client") || !g.cfg.Reconnect {
			break
		}
		attempt++
		if g.cfg.ReconnectMax > 0 && attempt > g.cfg.ReconnectMax {
			reason = fmt.Sprintf("%s; gave up after %d retries", reason, g.cfg.ReconnectMax)
			_, _ = conn.Write([]byte("\r\n[gateway] giving up reconnecting.\r\n"))
			break
		}
		msg := fmt.Sprintf("\r\n[gateway] console link lost (%s); reconnecting in %.0fs (attempt %d)...\r\n",
			reason, g.cfg.ReconnectDelay, attempt)
		if _, e := conn.Write([]byte(msg)); e != nil {
			reason = "client gone during reconnect"
			break
		}
		logf("INFO", "reconnecting %s: attempt %d after '%s'", peer, attempt, reason)
		time.Sleep(time.Duration(g.cfg.ReconnectDelay * float64(time.Second)))
	}

	g.mu.Lock()
	g.sessions--
	cur = g.sessions
	g.mu.Unlock()
	logf("INFO", "session down: %s after %.1fs (cause: %s) (%d/%d)",
		peer, time.Since(started).Seconds(), reason, cur, g.cfg.MaxSessions)
	_ = conn.Close()
}

func (g *Gateway) serve() error {
	addr := fmt.Sprintf("%s:%d", g.cfg.ListenHost, g.cfg.ListenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	tuiInfo := "off"
	if g.cfg.TUI {
		tuiInfo = "on hotkey=" + g.cfg.Hotkey
	}
	logf("INFO", "listening on %s -> ws://%s:%d/tty (mode=%s eol=%s framing=%s throttle=%v reconnect=%v tui=%s)",
		addr, g.cfg.Host, g.cfg.WSPort, g.cfg.Mode, g.cfg.EOL, g.cfg.Framing, g.cfg.Throttle, g.cfg.Reconnect, tuiInfo)
	for {
		conn, err := ln.Accept()
		if err != nil {
			logf("WARNING", "accept: %v", err)
			continue
		}
		go g.handleClient(conn)
	}
}
