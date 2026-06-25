package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const defaultConfigFile = "imsai-gw.toml"

// Config holds all resolved gateway settings.
type Config struct {
	Host           string
	WSPort         int
	ListenHost     string
	ListenPort     int
	Mode           string // raw | telnet | auto
	EOL            string // cr | crlf | lf | raw  (keystrokes TCP->WS)
	CRLFOut        bool   // WS->TCP: convert bare LF to CR LF (ONLCR; like xterm convertEol)
	Cols           int    // WS->TCP: emulate auto-wrap at this column (0=off; 80 = CP/M console)
	Framing        string // binary | text
	Throttle       bool
	CRDelay        float64
	CharDelay      float64
	MaxSessions    int
	Reconnect      bool
	ReconnectDelay float64
	ReconnectMax   int
	TUI            bool   // enable the modal control TUI
	Hotkey         string // hotkey spec, e.g. "Ctrl+\\"
	HotkeyBytes    []byte // parsed hotkey byte sequence (nil when TUI disabled)
	Capture        string // diagnostic: file to dump raw WS->TCP (device output) bytes to
	LogLevel       string
}

func defaultConfig() Config {
	return Config{
		Host:       "", // no default: must be set via --host / env / config (validated in buildConfig)
		WSPort:     80,
		ListenHost: "0.0.0.0",
		ListenPort: 2323,
		Mode:       "auto",
		EOL:        "cr",
		CRLFOut:    true,
		Cols:       80,
		Framing:    "binary",
		Throttle:   true,
		CRDelay:    0.100,
		CharDelay:  0.020,
		MaxSessions: 1,
		Reconnect:   true,
		ReconnectDelay: 2.0,
		ReconnectMax:   0,
		TUI:            true,
		Hotkey:         `Ctrl+\`,
		LogLevel:       "INFO",
	}
}

// parseHotkey turns a spec like "Ctrl+\\", "Ctrl+]", "Alt+P", "Ctrl+Alt+Z", "Shift+Alt+X"
// into the byte sequence the terminal sends for it:
//   - Ctrl+<key>      -> one control byte (key & 0x1F)
//   - Alt+<key>       -> ESC + <key>            (Meta = ESC prefix)
//   - Shift+Alt+<key> -> ESC + <upper key>
//   - Ctrl+Alt+<key>  -> ESC + control byte
// Only single-character keys are supported.
func parseHotkey(spec string) ([]byte, error) {
	parts := strings.Split(spec, "+")
	var ctrl, alt, shift bool
	key := ""
	for i, p := range parts {
		t := strings.TrimSpace(p)
		if i < len(parts)-1 {
			switch strings.ToUpper(t) {
			case "CTRL", "CONTROL", "C":
				ctrl = true
			case "ALT", "META", "OPTION":
				alt = true
			case "SHIFT":
				shift = true
			default:
				return nil, fmt.Errorf("unknown modifier %q", t)
			}
		} else {
			key = t
		}
	}
	if len(key) != 1 {
		return nil, fmt.Errorf("key must be a single character, got %q", key)
	}
	ch := key[0]
	if (shift || ctrl) && ch >= 'a' && ch <= 'z' {
		ch = ch - 'a' + 'A'
	}
	if ctrl {
		ch &= 0x1F // map to control byte (e.g. '\\'=0x5C -> 0x1C)
	}
	if alt {
		return []byte{0x1B, ch}, nil
	}
	return []byte{ch}, nil
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// loadConfigFile reads a TOML file into a flat map. Accepts either top-level keys or a
// [gateway] table. A missing required file (explicit --config / env) is fatal; a missing
// auto-discovered default is ignored; a malformed file is fatal.
func loadConfigFile(path string, required bool) map[string]any {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			if required {
				fatal("config file not found: " + path)
			}
			return nil
		}
		fatal(fmt.Sprintf("cannot read config file %s: %v", path, err))
	}
	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		fatal(fmt.Sprintf("cannot read config file %s: %v", path, err))
	}
	if g, ok := raw["gateway"].(map[string]any); ok {
		return g
	}
	return raw
}

func fileStr(m map[string]any, k string) (string, bool) {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

func fileInt(m map[string]any, k string) (int, bool) {
	if v, ok := m[k]; ok {
		switch n := v.(type) {
		case int64:
			return int(n), true
		case int:
			return n, true
		}
	}
	return 0, false
}

func fileFloat(m map[string]any, k string) (float64, bool) {
	if v, ok := m[k]; ok {
		switch n := v.(type) {
		case float64:
			return n, true
		case int64:
			return float64(n), true
		}
	}
	return 0, false
}

func fileBool(m map[string]any, k string) (bool, bool) {
	if v, ok := m[k]; ok {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return false, false
}

func envStr(name string, dst *string) {
	if v, ok := os.LookupEnv("IMSAI_GW_" + name); ok {
		*dst = v
	}
}

func envInt(name string, dst *int) {
	if v, ok := os.LookupEnv("IMSAI_GW_" + name); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			*dst = n
		} else {
			logf("WARNING", "ignoring invalid env IMSAI_GW_%s=%q", name, v)
		}
	}
}

func envFloat(name string, dst *float64) {
	if v, ok := os.LookupEnv("IMSAI_GW_" + name); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			*dst = f
		} else {
			logf("WARNING", "ignoring invalid env IMSAI_GW_%s=%q", name, v)
		}
	}
}

func envBool(name string, dst *bool) {
	if v, ok := os.LookupEnv("IMSAI_GW_" + name); ok {
		s := strings.ToLower(strings.TrimSpace(v))
		*dst = s == "1" || s == "true" || s == "yes" || s == "on"
	}
}

func mustChoice(name, val string, choices ...string) {
	for _, c := range choices {
		if val == c {
			return
		}
	}
	fatal(fmt.Sprintf("invalid %s=%q (allowed: %s)", name, val, strings.Join(choices, ", ")))
}

// buildConfig resolves config with precedence: CLI flags > env (IMSAI_GW_*) > file > defaults.
func buildConfig() Config {
	def := defaultConfig()

	fs := flag.NewFlagSet("imsai-tty-gateway", flag.ExitOnError)
	fConfig := fs.String("config", "", "TOML config file (env IMSAI_GW_CONFIG; default ./imsai-gw.toml if present)")
	fHost := fs.String("host", def.Host, "IMSAI host/IP (env IMSAI_GW_HOST)")
	fWSPort := fs.Int("ws-port", def.WSPort, "WebSocket port (env IMSAI_GW_WS_PORT)")
	fListenHost := fs.String("listen-host", def.ListenHost, "TCP bind address (env IMSAI_GW_LISTEN_HOST)")
	fListenPort := fs.Int("listen-port", def.ListenPort, "TCP listen port (env IMSAI_GW_LISTEN_PORT)")
	fMode := fs.String("mode", def.Mode, "raw|telnet|auto (env IMSAI_GW_MODE)")
	fEOL := fs.String("eol", def.EOL, "cr|crlf|lf|raw (env IMSAI_GW_EOL)")
	fNoCRLFOut := fs.Bool("no-crlf-out", false, "do NOT convert bare LF to CR LF on output "+
		"(disable for 8-bit binary transfers; env IMSAI_GW_CRLF_OUT=0)")
	fCols := fs.Int("cols", def.Cols, "emulate output auto-wrap at this column, 0=off "+
		"(80 = CP/M console; fixes Rogue on wide terminals; env IMSAI_GW_COLS)")
	fFraming := fs.String("framing", def.Framing, "binary|text (env IMSAI_GW_FRAMING)")
	fNoThrottle := fs.Bool("no-throttle", false, "disable TCP->WS pacing (env IMSAI_GW_THROTTLE=0)")
	fCRDelay := fs.Float64("cr-delay", def.CRDelay, "seconds after CR (env IMSAI_GW_CR_DELAY)")
	fCharDelay := fs.Float64("char-delay", def.CharDelay, "seconds after other chars (env IMSAI_GW_CHAR_DELAY)")
	fMaxSessions := fs.Int("max-sessions", def.MaxSessions, "concurrent client sessions (env IMSAI_GW_MAX_SESSIONS)")
	fNoReconnect := fs.Bool("no-reconnect", false, "disable WS auto-reconnect (env IMSAI_GW_RECONNECT=0)")
	fReconnectDelay := fs.Float64("reconnect-delay", def.ReconnectDelay, "seconds between reconnect attempts (env IMSAI_GW_RECONNECT_DELAY)")
	fReconnectMax := fs.Int("reconnect-max", def.ReconnectMax, "max reconnect attempts, 0=unlimited (env IMSAI_GW_RECONNECT_MAX)")
	fNoTUI := fs.Bool("no-tui", false, "disable the control TUI (env IMSAI_GW_TUI=0)")
	fHotkey := fs.String("hotkey", def.Hotkey, `TUI hotkey, e.g. Ctrl+\ (env IMSAI_GW_HOTKEY)`)
	fCapture := fs.String("capture", "", "diagnostic: dump raw device output (WS->TCP) to this file")
	fLogLevel := fs.String("log-level", def.LogLevel, "DEBUG|INFO|WARNING|ERROR (env IMSAI_GW_LOG_LEVEL)")
	_ = fs.Parse(os.Args[1:])

	cfg := def

	// --- config file (lowest override above defaults) ---
	path := *fConfig
	required := path != ""
	if path == "" {
		if e := os.Getenv("IMSAI_GW_CONFIG"); e != "" {
			path = e
			required = true
		}
	}
	if path == "" {
		path = defaultConfigFile
		required = false
	}
	if fc := loadConfigFile(path, required); fc != nil {
		if v, ok := fileStr(fc, "host"); ok {
			cfg.Host = v
		}
		if v, ok := fileInt(fc, "ws_port"); ok {
			cfg.WSPort = v
		}
		if v, ok := fileStr(fc, "listen_host"); ok {
			cfg.ListenHost = v
		}
		if v, ok := fileInt(fc, "listen_port"); ok {
			cfg.ListenPort = v
		}
		if v, ok := fileStr(fc, "mode"); ok {
			cfg.Mode = v
		}
		if v, ok := fileStr(fc, "eol"); ok {
			cfg.EOL = v
		}
		if v, ok := fileBool(fc, "crlf_out"); ok {
			cfg.CRLFOut = v
		}
		if v, ok := fileInt(fc, "cols"); ok {
			cfg.Cols = v
		}
		if v, ok := fileStr(fc, "framing"); ok {
			cfg.Framing = v
		}
		if v, ok := fileBool(fc, "throttle"); ok {
			cfg.Throttle = v
		}
		if v, ok := fileFloat(fc, "cr_delay"); ok {
			cfg.CRDelay = v
		}
		if v, ok := fileFloat(fc, "char_delay"); ok {
			cfg.CharDelay = v
		}
		if v, ok := fileInt(fc, "max_sessions"); ok {
			cfg.MaxSessions = v
		}
		if v, ok := fileBool(fc, "reconnect"); ok {
			cfg.Reconnect = v
		}
		if v, ok := fileFloat(fc, "reconnect_delay"); ok {
			cfg.ReconnectDelay = v
		}
		if v, ok := fileInt(fc, "reconnect_max"); ok {
			cfg.ReconnectMax = v
		}
		if v, ok := fileBool(fc, "tui"); ok {
			cfg.TUI = v
		}
		if v, ok := fileStr(fc, "hotkey"); ok {
			cfg.Hotkey = v
		}
		if v, ok := fileStr(fc, "log_level"); ok {
			cfg.LogLevel = v
		}
	}

	// --- environment (overrides file) ---
	envStr("HOST", &cfg.Host)
	envInt("WS_PORT", &cfg.WSPort)
	envStr("LISTEN_HOST", &cfg.ListenHost)
	envInt("LISTEN_PORT", &cfg.ListenPort)
	envStr("MODE", &cfg.Mode)
	envStr("EOL", &cfg.EOL)
	envBool("CRLF_OUT", &cfg.CRLFOut)
	envInt("COLS", &cfg.Cols)
	envStr("FRAMING", &cfg.Framing)
	envBool("THROTTLE", &cfg.Throttle)
	envFloat("CR_DELAY", &cfg.CRDelay)
	envFloat("CHAR_DELAY", &cfg.CharDelay)
	envInt("MAX_SESSIONS", &cfg.MaxSessions)
	envBool("RECONNECT", &cfg.Reconnect)
	envFloat("RECONNECT_DELAY", &cfg.ReconnectDelay)
	envInt("RECONNECT_MAX", &cfg.ReconnectMax)
	envBool("TUI", &cfg.TUI)
	envStr("HOTKEY", &cfg.Hotkey)
	envStr("LOG_LEVEL", &cfg.LogLevel)

	// --- CLI flags explicitly set (highest override) ---
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["host"] {
		cfg.Host = *fHost
	}
	if set["ws-port"] {
		cfg.WSPort = *fWSPort
	}
	if set["listen-host"] {
		cfg.ListenHost = *fListenHost
	}
	if set["listen-port"] {
		cfg.ListenPort = *fListenPort
	}
	if set["mode"] {
		cfg.Mode = *fMode
	}
	if set["eol"] {
		cfg.EOL = *fEOL
	}
	if set["no-crlf-out"] {
		cfg.CRLFOut = !*fNoCRLFOut
	}
	if set["cols"] {
		cfg.Cols = *fCols
	}
	if set["framing"] {
		cfg.Framing = *fFraming
	}
	if set["no-throttle"] {
		cfg.Throttle = !*fNoThrottle
	}
	if set["cr-delay"] {
		cfg.CRDelay = *fCRDelay
	}
	if set["char-delay"] {
		cfg.CharDelay = *fCharDelay
	}
	if set["max-sessions"] {
		cfg.MaxSessions = *fMaxSessions
	}
	if set["no-reconnect"] {
		cfg.Reconnect = !*fNoReconnect
	}
	if set["reconnect-delay"] {
		cfg.ReconnectDelay = *fReconnectDelay
	}
	if set["reconnect-max"] {
		cfg.ReconnectMax = *fReconnectMax
	}
	if set["no-tui"] {
		cfg.TUI = !*fNoTUI
	}
	if set["hotkey"] {
		cfg.Hotkey = *fHotkey
	}
	if set["capture"] {
		cfg.Capture = *fCapture
	}
	if set["log-level"] {
		cfg.LogLevel = *fLogLevel
	}

	if strings.TrimSpace(cfg.Host) == "" {
		fatal("host is required: set the IMSAI host name or IP via --host, the IMSAI_GW_HOST " +
			"env var, or 'host' in imsai-gw.toml (e.g. --host imsai8080 or --host 192.168.1.50)")
	}

	cfg.LogLevel = strings.ToUpper(cfg.LogLevel)
	mustChoice("mode", cfg.Mode, "raw", "telnet", "auto")
	mustChoice("eol", cfg.EOL, "cr", "crlf", "lf", "raw")
	mustChoice("framing", cfg.Framing, "binary", "text")
	mustChoice("log-level", cfg.LogLevel, "DEBUG", "INFO", "WARNING", "ERROR")

	if cfg.TUI {
		hb, err := parseHotkey(cfg.Hotkey)
		if err != nil {
			fatal(fmt.Sprintf("invalid hotkey %q: %v", cfg.Hotkey, err))
		}
		cfg.HotkeyBytes = hb
	}

	return cfg
}
