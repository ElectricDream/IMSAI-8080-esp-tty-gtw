// Command imsai-tty-gateway bridges an inbound Telnet/TCP terminal connection to the IMSAI 8080esp
// console WebSocket (ws://<host>/tty): raw or Telnet on the TCP side, a WebSocket client on
// the device side, with line-ending normalization, throttling, single-session policy and
// automatic reconnect. See README.md for usage and the design notes (e.g. no WS keepalive ping:
// the ESP /tty drops a pinged connection at ~20 s).
package main

import (
	"fmt"
	"log"
	"os"
)

// Product identity, surfaced in the connection banner and the TUI title. Keep version in sync
// with the release tag used by CI (e.g. v0.1).
const (
	productName = "IMSAI 8080 esp Replica - TTY Gateway Control"
	version     = "0.3.7"
)

// Minimal leveled logging: lines are formatted as "<LEVEL> gateway <msg>".
var logOrder = map[string]int{"DEBUG": 0, "INFO": 1, "WARNING": 2, "ERROR": 3}
var curLogLevel = 1 // INFO

func setLogLevel(level string) {
	if v, ok := logOrder[level]; ok {
		curLogLevel = v
	}
}

func logf(level, format string, a ...any) {
	if logOrder[level] >= curLogLevel {
		log.Printf("%s gateway %s", level, fmt.Sprintf(format, a...))
	}
}

func main() {
	cfg := buildConfig()
	log.SetFlags(log.LstdFlags)
	setLogLevel(cfg.LogLevel)

	logf("INFO", "IMSAI TTY Gateway v%s started", version)

	g := &Gateway{cfg: cfg}
	if err := g.serve(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}
