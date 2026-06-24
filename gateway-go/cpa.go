package main

import (
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// Client for the front-panel WebSocket (ws://<host>/cpa). Used by the TUI panel pane to read
// the LED/status string and actuate the momentary command switches.
//
// IMPORTANT (learned live): opening /cpa WHILE /tty has active console traffic makes the
// device drop the /tty connection. So /cpa is opened ONCE at session start (while idle) and
// kept open for the whole session, never opened on demand mid-session. A background reader
// drains it continuously (status updates + '$' disk pings) so its buffer never backs up.
//
// Protocol: send "P" to request the 6-char status "U/D/E I R W H X" (letter = ON). Actuating
// a command switch sends a 2-char code (<switch><dir>), e.g. "eu". '$' = disk-activity ping.

type cpaConn struct{ ws *websocket.Conn }

func dialCPA(cfg Config) (*cpaConn, error) {
	url := fmt.Sprintf("ws://%s:%d/cpa", cfg.Host, cfg.WSPort)
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	c, _, err := d.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	return &cpaConn{ws: c}, nil
}

func (c *cpaConn) close() {
	if c != nil && c.ws != nil {
		_ = c.ws.Close()
	}
}

// send writes a text message ("P" to poll, or a 2-char actuation code).
func (c *cpaConn) send(msg string) error {
	return c.ws.WriteMessage(websocket.TextMessage, []byte(msg))
}

// readMessage blocks for the next /cpa text message.
func (c *cpaConn) readMessage() (string, error) {
	_, data, err := c.ws.ReadMessage()
	return string(data), err
}
