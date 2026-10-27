package main

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestKeyDecoderSplitArrow(t *testing.T) {
	var d keyDecoder
	if keys := d.decode([]byte{0x1b}); len(keys) != 0 || !d.pending() {
		t.Fatalf("trailing ESC: got %+v pending=%v, want no keys and pending", keys, d.pending())
	}
	keys := d.decode([]byte("[B"))
	if len(keys) != 1 || keys[0].kind != kDown {
		t.Fatalf("ESC | [B: got %+v, want a single Down", keys)
	}
}

func TestKeyDecoderFlushLoneEsc(t *testing.T) {
	var d keyDecoder
	d.decode([]byte{0x1b})
	keys := d.flush()
	if len(keys) != 1 || keys[0].kind != kEsc || d.pending() {
		t.Fatalf("flush: got %+v pending=%v, want a single Esc", keys, d.pending())
	}
	if keys := d.flush(); len(keys) != 0 {
		t.Fatalf("second flush: got %+v, want none", keys)
	}
}

// newTUIPumpSession runs pumpTCPToWS over a pipe with the TUI open on a three-drive Disks pane.
func newTUIPumpSession(t *testing.T) (s *bridgeSession, client net.Conn, stop func() string) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go func() { _, _ = io.Copy(io.Discard, client) }()

	s = &bridgeSession{conn: server, term: NewTerminalInput("auto", "cr")}
	s.tui = true
	s.tuiM = &tuiModel{mode: tmDisks, disks: []driveEntry{{ID: "A"}, {ID: "B"}, {ID: "C"}}}

	done := make(chan string, 1)
	go func() { done <- s.pumpTCPToWS() }()
	stop = func() string {
		s.stopping.Store(true)
		_ = server.SetReadDeadline(time.Now())
		select {
		case r := <-done:
			return r
		case <-time.After(2 * time.Second):
			t.Fatal("pump did not stop")
			return ""
		}
	}
	return s, client, stop
}

func TestPumpSplitArrowStaysInTUI(t *testing.T) {
	s, client, stop := newTUIPumpSession(t)
	_, _ = client.Write([]byte{0x1b})
	time.Sleep(escTimeout / 5)
	_, _ = client.Write([]byte("[B"))
	time.Sleep(escTimeout / 5)
	stop()
	if !s.tui || s.tuiM.sel != 1 {
		t.Fatalf("tui=%v sel=%d, want TUI open with selection moved to 1", s.tui, s.tuiM.sel)
	}
}

func TestPumpLoneEscClosesTUI(t *testing.T) {
	s, client, stop := newTUIPumpSession(t)
	_, _ = client.Write([]byte{0x1b})
	time.Sleep(escTimeout + 150*time.Millisecond)
	stop()
	if s.tui {
		t.Fatal("TUI still open after a lone Esc")
	}
}

func TestPumpStopDuringEscWait(t *testing.T) {
	s, client, stop := newTUIPumpSession(t)
	_, _ = client.Write([]byte{0x1b})
	if r := stop(); r != "stopped" {
		t.Fatalf("stop cause %q, want stopped", r)
	}
	if !s.tui {
		t.Fatal("a stop during the ESC wait was taken as the Esc key")
	}
}
