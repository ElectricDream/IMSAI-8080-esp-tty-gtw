package main

import (
	"fmt"
	"strings"
	"time"
)

// cpaSettle is a short wait after sending "P"/a switch code so readCPALoop has captured the
// fresh status before the next draw (the panel redraws on key events, not continuously).
const cpaSettle = 200 * time.Millisecond

// Control TUI rendered over the terminal via the alternate screen buffer. While it is open,
// console output from the device is buffered (see bridgeSession.pumpWSToTCP); on exit the
// console screen is restored and the buffered output flushed.
//
// M6 step 4: Disks pane. List drive slots, eject, and mount an image picked from the
// on-device library (HTTP backend in device.go). Front panel pane comes next.

// ANSI helpers.
const (
	ansiEnterAlt = "\x1b[?1049h" // switch to the alternate screen buffer
	ansiLeaveAlt = "\x1b[?1049l" // back to the normal screen (restores prior content)
	ansiHideCur  = "\x1b[?25l"
	ansiShowCur  = "\x1b[?25h"
	ansiWrapOff  = "\x1b[?7l" // disable auto-wrap (DECAWM) while the TUI owns the screen
	ansiWrapOn   = "\x1b[?7h" // restore auto-wrap on exit
	ansiClear    = "\x1b[2J"
	ansiReverse  = "\x1b[7m"
	ansiBold     = "\x1b[1m"
	ansiYellow   = "\x1b[33m"
	ansiReset    = "\x1b[0m"
)

func moveTo(row, col int) string {
	if row < 1 {
		row = 1
	}
	if col < 1 {
		col = 1
	}
	return fmt.Sprintf("\x1b[%d;%dH", row, col)
}

func pad(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}

func truncate(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

// --- input decoding --------------------------------------------------------------------

type keyKind int

const (
	kChar keyKind = iota
	kEnter
	kEsc
	kUp
	kDown
	kLeft
	kRight
	kPageUp
	kPageDown
)

type key struct {
	kind keyKind
	ch   byte
}

// keyDecoder turns a byte stream into key events, handling CR/LF, lone Esc, the CSI/SS3 arrow
// sequences (ESC [ A.. / ESC O A..) and the tilde-terminated keys Page Up (ESC [ 5 ~) and Page
// Down (ESC [ 6 ~), any of which may span multiple reads.
type keyDecoder struct {
	esc   int    // 0 = none, 1 = saw ESC, 2 = saw ESC[ or ESC O
	param []byte // CSI numeric parameter collected in state 2 (e.g. "5" for Page Up)
}

func (d *keyDecoder) decode(data []byte) []key {
	keys := []key{}
	emitPlain := func(b byte) {
		switch {
		case b == 13 || b == 10:
			keys = append(keys, key{kind: kEnter})
		default:
			keys = append(keys, key{kind: kChar, ch: b})
		}
	}
	for _, b := range data {
		switch d.esc {
		case 0:
			if b == 0x1b {
				d.esc = 1
			} else {
				emitPlain(b)
			}
		case 1:
			if b == '[' || b == 'O' {
				d.esc = 2
				d.param = d.param[:0]
			} else {
				keys = append(keys, key{kind: kEsc})
				d.esc = 0
				if b == 0x1b {
					d.esc = 1
				} else {
					emitPlain(b)
				}
			}
		case 2:
			switch {
			case b == 'A':
				keys = append(keys, key{kind: kUp})
				d.esc = 0
			case b == 'B':
				keys = append(keys, key{kind: kDown})
				d.esc = 0
			case b == 'C':
				keys = append(keys, key{kind: kRight})
				d.esc = 0
			case b == 'D':
				keys = append(keys, key{kind: kLeft})
				d.esc = 0
			case b == '~':
				// tilde-terminated key: 5 = Page Up, 6 = Page Down (others ignored)
				switch string(d.param) {
				case "5":
					keys = append(keys, key{kind: kPageUp})
				case "6":
					keys = append(keys, key{kind: kPageDown})
				}
				d.esc = 0
			case b >= '0' && b <= '9':
				d.param = append(d.param, b) // CSI numeric parameter; keep collecting
			case b == ';':
				// multi-parameter separator; keep collecting (ignored for our keys)
			default:
				d.esc = 0 // end of an unhandled sequence -> ignore
			}
		}
	}
	if d.esc == 1 { // a lone trailing ESC means the Esc key
		keys = append(keys, key{kind: kEsc})
		d.esc = 0
	}
	return keys
}

// --- TUI model -------------------------------------------------------------------------

const (
	tmDisks = iota
	tmLibrary // mount picker (reached from the Disks pane)
	tmPanel
	tmSystem
	tmLib        // LIB tab: manage the on-device library (list / preview / delete)
	tmLibPreview // LIB: scrollable view of an image's CP/M directory (USER 0)
)

type tuiModel struct {
	mode      int
	disks     []driveEntry
	sel       int
	library   []string // all library filenames (loaded once)
	libLoaded bool
	libView   []string // filtered list shown in the picker (.dsk for floppies)
	libSel    int
	libFor    string // drive id being mounted
	// front panel (LED status lives on the session as s.cpaStatus, updated by readCPALoop)
	selSw int // selected command switch
	// system pane
	sysLines  []string
	sysScroll int
	// LIB pane (library management)
	machine        string // active machine id (for the download path), fetched lazily
	libItems       []libEntry
	libItemSel     int
	libItemsLoaded bool
	prevName       string             // image being previewed
	prevByUser     map[int][]cpmFile  // all USERs' files, parsed once per image
	prevUsers      []int              // navigable user numbers (USER 0 + any non-empty), sorted
	prevUserIdx    int                // index into prevUsers of the displayed user
	prevLines      []string           // formatted listing for the current user
	prevScroll     int
	// confirmation overlay (used by destructive actions, e.g. library delete)
	confirm    bool
	confirmMsg string
	confirmAct func() // run on 'y'
	status     string
	pageStep   int // visible content rows of the active pane, set at render; used by Page Up/Down
	dec        keyDecoder
}

// step returns the Page Up/Down jump size (a screenful), with a sane fallback before the first
// render has measured the window.
func (m *tuiModel) step() int {
	if m.pageStep > 0 {
		return m.pageStep
	}
	return 10
}

// clampIndex bounds i to a valid index in a list of n items: [0, n-1], or 0 when empty.
func clampIndex(i, n int) int {
	if n <= 0 {
		return 0
	}
	if i < 0 {
		return 0
	}
	if i > n-1 {
		return n - 1
	}
	return i
}

// panelSwitch is one front-panel command switch (momentary: up/down send a /cpa code, the
// switch springs back to center). upCode/downCode "" means display-only (not in the API).
type panelSwitch struct {
	upLabel, downLabel string
	upCode, downCode   string
}

// Left-to-right as on the real IMSAI 8080 front panel.
var panelSwitches = []panelSwitch{
	{"EXAMINE", "EX.NEXT", "eu", "ed"},
	{"DEPOSIT", "DEP.NEXT", "du", "dd"},
	{"RESET", "EXT.CLR", "cu", "cd"},
	{"RUN", "STOP", "ru", "rd"},
	{"S.STEP", "S.STEP", "su", "sd"},
	{"PWR ON", "PWR OFF", "", ""}, // power switch: not exposed by the API -> display-only
}

func ledOn(status string, idx int, ch byte) bool {
	return len(status) > idx && status[idx] == ch
}

func centerCell(s string, w int) string {
	if len(s) > w {
		s = s[:w]
	}
	total := w - len(s)
	left := total / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", total-left)
}

func filterDsk(all []string) []string {
	out := make([]string, 0, len(all))
	for _, f := range all {
		if strings.HasSuffix(strings.ToLower(f), ".dsk") {
			out = append(out, f)
		}
	}
	return out
}

// filterDiskImages keeps only disk images (.dsk floppies and .hdd hard disks, any case),
// dropping non-image library files such as disk.map.
func filterDiskImages(all []libEntry) []libEntry {
	out := make([]libEntry, 0, len(all))
	for _, e := range all {
		l := strings.ToLower(e.Filename)
		if strings.HasSuffix(l, ".dsk") || strings.HasSuffix(l, ".hdd") {
			out = append(out, e)
		}
	}
	return out
}

func (s *bridgeSession) writeConn(str string) { _, _ = s.conn.Write([]byte(str)) }

// enterTUI switches into TUI mode, loads disk state, and draws the screen.
func (s *bridgeSession) enterTUI() {
	s.mu.Lock()
	s.tui = true
	s.mu.Unlock()
	s.tuiM = &tuiModel{mode: tmDisks}
	s.refreshDisks()
	s.writeConn(ansiEnterAlt + ansiHideCur + ansiWrapOff)
	s.tuiDraw()
}

// exitTUI restores the console screen and flushes buffered console output (under lock so it
// cannot be interleaved by the WS pump).
func (s *bridgeSession) exitTUI() {
	s.mu.Lock()
	s.tui = false
	pending := s.outBuf
	s.outBuf = nil
	s.tuiM = nil
	_, _ = s.conn.Write([]byte(ansiShowCur + ansiWrapOn + ansiLeaveAlt))
	if len(pending) > 0 {
		_, _ = s.conn.Write(pending)
	}
	s.mu.Unlock()
	// /cpa is persistent for the whole session (NOT closed here): reconnecting it restarts
	// the device web server, so we keep it open until the session ends.
}

// tuiFeed consumes client input while the TUI is open; returns bytes consumed and whether
// the TUI should close.
func (s *bridgeSession) tuiFeed(data []byte) (consumed int, exited bool) {
	m := s.tuiM
	for _, k := range m.dec.decode(data) {
		if s.tuiKey(k) {
			return len(data), true
		}
	}
	s.tuiDraw()
	return len(data), false
}

// tuiKey applies one key event; returns true to close the TUI.
func (s *bridgeSession) tuiKey(k key) bool {
	m := s.tuiM
	// A pending confirmation overlay captures input until answered.
	if m.confirm {
		switch {
		case k.kind == kChar && (k.ch == 'y' || k.ch == 'Y'):
			act := m.confirmAct
			m.confirm = false
			m.confirmAct = nil
			m.confirmMsg = ""
			if act != nil {
				act()
			}
		case k.kind == kEsc || (k.kind == kChar && (k.ch == 'n' || k.ch == 'N')):
			m.confirm = false
			m.confirmAct = nil
			m.confirmMsg = ""
			m.status = "cancelled"
		}
		return false
	}
	// Navigation keys clear any lingering status message so the key hints reappear on the bottom
	// bar while browsing (the bar shows either the hint OR a status/confirm message, never both).
	switch k.kind {
	case kUp, kDown, kLeft, kRight, kPageUp, kPageDown:
		m.status = ""
	}
	switch m.mode {
	case tmDisks:
		switch k.kind {
		case kUp:
			if m.sel > 0 {
				m.sel--
			}
		case kDown:
			if m.sel < len(m.disks)-1 {
				m.sel++
			}
		case kPageUp:
			m.sel = clampIndex(m.sel-m.step(), len(m.disks))
		case kPageDown:
			m.sel = clampIndex(m.sel+m.step(), len(m.disks))
		case kEnter:
			s.openLibrary()
		case kEsc:
			return true
		case kChar:
			switch k.ch {
			case 'q', 'Q':
				return true
			case '\t':
				s.enterPanel()
			case 'r', 'R':
				s.refreshDisks()
			case 'e', 'E':
				s.doEject()
			case 'm', 'M':
				s.openLibrary()
			}
		}
	case tmPanel:
		switch k.kind {
		case kLeft:
			if m.selSw > 0 {
				m.selSw--
			}
		case kRight:
			if m.selSw < len(panelSwitches)-1 {
				m.selSw++
			}
		case kUp:
			s.doActuate(true)
		case kDown:
			s.doActuate(false)
		case kEsc:
			return true
		case kChar:
			switch k.ch {
			case 'q', 'Q':
				return true
			case '\t':
				s.enterSystem() // Disks -> Panel -> System -> Disks
			case 'r', 'R':
				s.refreshPanel()
			case 'u', 'U':
				s.doActuate(true)
			case 'd', 'D':
				s.doActuate(false)
			}
		}
	case tmSystem:
		switch k.kind {
		case kUp:
			if m.sysScroll > 0 {
				m.sysScroll--
			}
		case kDown:
			if m.sysScroll < len(m.sysLines)-1 {
				m.sysScroll++
			}
		case kPageUp:
			m.sysScroll = clampIndex(m.sysScroll-m.step(), len(m.sysLines))
		case kPageDown:
			m.sysScroll = clampIndex(m.sysScroll+m.step(), len(m.sysLines))
		case kEsc:
			return true
		case kChar:
			switch k.ch {
			case 'q', 'Q':
				return true
			case '\t':
				s.enterLib() // System -> LIB
			case 'r', 'R':
				s.refreshSystem()
			}
		}
	case tmLib:
		switch k.kind {
		case kUp:
			if m.libItemSel > 0 {
				m.libItemSel--
			}
		case kDown:
			if m.libItemSel < len(m.libItems)-1 {
				m.libItemSel++
			}
		case kPageUp:
			m.libItemSel = clampIndex(m.libItemSel-m.step(), len(m.libItems))
		case kPageDown:
			m.libItemSel = clampIndex(m.libItemSel+m.step(), len(m.libItems))
		case kEnter:
			s.doPreview()
		case kEsc:
			return true
		case kChar:
			switch k.ch {
			case 'q', 'Q':
				return true
			case '\t':
				m.mode = tmDisks // LIB -> Disks
				m.status = ""
			case 'r', 'R':
				s.refreshLib()
			case 'v', 'V':
				s.doPreview()
			case 'd', 'D':
				s.askDeleteLib()
			}
		}
	case tmLibPreview:
		switch k.kind {
		case kUp:
			if m.prevScroll > 0 {
				m.prevScroll--
			}
		case kDown:
			if m.prevScroll < len(m.prevLines)-1 {
				m.prevScroll++
			}
		case kPageUp:
			m.prevScroll = clampIndex(m.prevScroll-m.step(), len(m.prevLines))
		case kPageDown:
			m.prevScroll = clampIndex(m.prevScroll+m.step(), len(m.prevLines))
		case kLeft:
			s.previewSetUser(m.prevUserIdx - 1)
		case kRight:
			s.previewSetUser(m.prevUserIdx + 1)
		case kEsc:
			m.mode = tmLib
			m.status = ""
		case kChar:
			if k.ch == 'q' || k.ch == 'Q' {
				m.mode = tmLib
				m.status = ""
			}
		}
	case tmLibrary:
		switch k.kind {
		case kUp:
			if m.libSel > 0 {
				m.libSel--
			}
		case kDown:
			if m.libSel < len(m.libView)-1 {
				m.libSel++
			}
		case kPageUp:
			m.libSel = clampIndex(m.libSel-m.step(), len(m.libView))
		case kPageDown:
			m.libSel = clampIndex(m.libSel+m.step(), len(m.libView))
		case kEnter:
			s.doMount()
		case kEsc:
			m.mode = tmDisks
			m.status = ""
		case kChar:
			if k.ch == 'q' || k.ch == 'Q' {
				m.mode = tmDisks
				m.status = ""
			}
		}
	}
	return false
}

func (s *bridgeSession) refreshDisks() {
	m := s.tuiM
	d, err := getDisks(s.cfg)
	if err != nil {
		m.status = "disks: " + err.Error()
		return
	}
	m.disks = d
	if m.sel >= len(d) {
		m.sel = max(0, len(d)-1)
	}
	m.status = ""
}

func (s *bridgeSession) openLibrary() {
	m := s.tuiM
	if len(m.disks) == 0 {
		return
	}
	dr := m.disks[m.sel]
	if dr.HardDisk {
		m.status = dr.ID + ": hard disk, set its image in the IMSAI config while powered off"
		return
	}
	if dr.Image != "" {
		m.status = fmt.Sprintf("%s is occupied; eject first (e)", dr.ID)
		return
	}
	if !m.libLoaded {
		lib, err := getLibrary(s.cfg)
		if err != nil {
			m.status = "library: " + err.Error()
			return
		}
		m.library = lib
		m.libLoaded = true
	}
	m.libView = filterDsk(m.library)
	if len(m.libView) == 0 {
		m.status = "no .dsk images in the library"
		return
	}
	m.libFor = dr.ID
	m.libSel = 0
	m.mode = tmLibrary
	m.status = ""
}

func (s *bridgeSession) doEject() {
	m := s.tuiM
	if len(m.disks) == 0 {
		return
	}
	dr := m.disks[m.sel]
	if dr.HardDisk {
		m.status = dr.ID + ": hard disk, not removable"
		return
	}
	if dr.Image == "" {
		m.status = dr.ID + " is already empty"
		return
	}
	if err := ejectDisk(s.cfg, dr.ID); err != nil {
		m.status = err.Error()
		return
	}
	s.refreshDisks()
	m.status = "ejected " + dr.ID
}

func (s *bridgeSession) doMount() {
	m := s.tuiM
	if m.libSel < 0 || m.libSel >= len(m.libView) {
		return
	}
	file := m.libView[m.libSel]
	drive := m.libFor
	m.mode = tmDisks
	// The firmware refuses to mount the same image on a second drive (it returns HTTP 404,
	// the same code as a missing image). Catch the duplicate ourselves so the message is
	// clear and names the drive already using it.
	for _, dr := range m.disks {
		if dr.ID != drive && dr.Image == file {
			m.status = fmt.Sprintf("%s is already mounted in drive %s (one image cannot be mounted twice)", file, dr.ID)
			return
		}
	}
	if err := mountDisk(s.cfg, drive, file); err != nil {
		// 404 here (past the duplicate check above) means the device cannot find the image.
		if strings.Contains(err.Error(), "HTTP 404") {
			m.status = fmt.Sprintf("cannot mount %s: image not found on device", file)
		} else {
			m.status = err.Error()
		}
		return
	}
	s.refreshDisks()
	m.status = fmt.Sprintf("mounted %s into %s", file, drive)
}

// --- front panel -----------------------------------------------------------------------

func (s *bridgeSession) enterPanel() {
	m := s.tuiM
	m.mode = tmPanel
	m.selSw = 0
	m.status = ""
	if s.cpa == nil { // /cpa is opened once per session in handleClient; nil = connect failed
		m.status = "front panel unavailable (/cpa not connected)"
		return
	}
	_ = s.cpa.send("P") // request a fresh status; readCPALoop updates s.cpaStatus
	time.Sleep(cpaSettle)
}

func (s *bridgeSession) refreshPanel() {
	m := s.tuiM
	if s.cpa == nil {
		m.status = "front panel unavailable"
		return
	}
	if err := s.cpa.send("P"); err != nil {
		m.status = "cpa: " + err.Error()
		return
	}
	time.Sleep(cpaSettle)
}

func (s *bridgeSession) doActuate(up bool) {
	m := s.tuiM
	sw := panelSwitches[m.selSw]
	code, lbl := sw.downCode, sw.downLabel
	if up {
		code, lbl = sw.upCode, sw.upLabel
	}
	if code == "" {
		m.status = lbl + ": not available via the API (set in the device config while OFF)"
		return
	}
	if s.cpa == nil {
		m.status = "cpa not connected"
		return
	}
	if err := s.cpa.send(code); err != nil {
		m.status = "cpa: " + err.Error()
		return
	}
	_ = s.cpa.send("P") // request updated status
	time.Sleep(cpaSettle)
	m.status = fmt.Sprintf("actuated %s (%s)", lbl, code)
}

// --- system pane -----------------------------------------------------------------------

func (s *bridgeSession) enterSystem() {
	m := s.tuiM
	m.mode = tmSystem
	m.sysScroll = 0
	m.status = ""
	s.refreshSystem()
}

func (s *bridgeSession) refreshSystem() {
	m := s.tuiM
	si, err := getSystem(s.cfg)
	if err != nil {
		m.sysLines = []string{"  /system error: " + err.Error()}
		return
	}
	m.sysLines = formatSysInfo(si)
}

// --- LIB pane (library management) -----------------------------------------------------

func (s *bridgeSession) enterLib() {
	m := s.tuiM
	m.mode = tmLib
	m.status = ""
	s.refreshLib()
}

func (s *bridgeSession) refreshLib() {
	m := s.tuiM
	items, err := getLibraryDetailed(s.cfg)
	if err != nil {
		m.status = "library: " + err.Error()
		return
	}
	m.libItems = filterDiskImages(items)
	m.libItemsLoaded = true
	if m.libItemSel >= len(items) {
		m.libItemSel = max(0, len(items)-1)
	}
	m.status = ""
}

// doPreview downloads the selected image and shows its CP/M directory (USER 0). The download is
// synchronous (256 KB, a few seconds on Wi-Fi); an interim "downloading…" frame is drawn first.
func (s *bridgeSession) doPreview() {
	m := s.tuiM
	if m.libItemSel < 0 || m.libItemSel >= len(m.libItems) {
		return
	}
	name := m.libItems[m.libItemSel].Filename
	m.status = "downloading " + name + " …"
	s.tuiDraw() // show progress before the blocking call

	if m.machine == "" {
		mach, err := getMachine(s.cfg)
		if err != nil {
			m.status = "machine: " + err.Error()
			return
		}
		m.machine = mach
	}
	data, err := downloadImage(s.cfg, m.machine, name)
	if err != nil {
		m.status = err.Error()
		return
	}
	byUser, err := listCPMByUser(data)
	if err != nil {
		m.status = err.Error()
		return
	}
	// Navigable user list: always include USER 0 (the default), plus any user that has files.
	present := map[int]bool{0: true}
	for u := range byUser {
		present[u] = true
	}
	users := []int{}
	for u := 0; u <= 15; u++ {
		if present[u] {
			users = append(users, u)
		}
	}
	m.prevName = name
	m.prevByUser = byUser
	m.prevUsers = users
	m.mode = tmLibPreview
	m.status = ""
	// Open on the first user that actually has files (USER 0 when it is populated, the common
	// case; otherwise the lowest populated user, so the first screen is never misleadingly empty
	// while files exist elsewhere). USER 0 stays reachable via Left. If no user has any files,
	// start on USER 0 (shown empty).
	start := 0
	for i, u := range users {
		if len(byUser[u]) > 0 {
			start = i
			break
		}
	}
	s.previewSetUser(start)
}

// previewSetUser switches the previewed user (by index into prevUsers) and rebuilds the listing.
func (s *bridgeSession) previewSetUser(idx int) {
	m := s.tuiM
	if idx < 0 || idx >= len(m.prevUsers) {
		return
	}
	m.prevUserIdx = idx
	u := m.prevUsers[idx]
	m.prevLines = formatCPMListing(m.prevName, u, m.prevByUser[u])
	m.prevScroll = 0
}

// formatCPMListing builds the preview lines for one user's directory on an image.
func formatCPMListing(name string, user int, files []cpmFile) []string {
	lines := []string{
		fmt.Sprintf("%s, USER %d (%d file(s))", name, user, len(files)),
		"",
	}
	if len(files) == 0 {
		lines = append(lines, fmt.Sprintf("  (no files in USER %d)", user))
		return lines
	}
	for _, f := range files {
		lines = append(lines, fmt.Sprintf("  %-12s %6d bytes  (%d records)", f.Name, f.SizeBytes(), f.Records))
	}
	return lines
}

func (s *bridgeSession) askDeleteLib() {
	m := s.tuiM
	if m.libItemSel < 0 || m.libItemSel >= len(m.libItems) {
		return
	}
	name := m.libItems[m.libItemSel].Filename
	m.confirm = true
	m.confirmMsg = fmt.Sprintf("Delete '%s' from the library? (y/N)", name)
	m.confirmAct = func() {
		if err := deleteLibraryImage(s.cfg, name); err != nil {
			m.status = err.Error()
			return
		}
		s.refreshLib()
		m.status = "deleted " + name
	}
}

// --- rendering -------------------------------------------------------------------------

func (s *bridgeSession) tuiDraw() {
	m := s.tuiM
	cols, rows, ok := s.term.Size()
	if !ok || cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	w := cols - 1

	// Page Up/Down jump size: roughly one screenful (window height minus title, tab bar, a
	// header, and the bottom hint/status overhead). At least 1.
	if ps := rows - 6; ps > 0 {
		m.pageStep = ps
	} else {
		m.pageStep = 1
	}

	var b strings.Builder
	b.WriteString(ansiClear)
	b.WriteString(moveTo(1, 1))
	b.WriteString(ansiBold + " " + productName + " v" + version + " " + ansiReset)

	// Tab bar (row 2): Disks | Panel | SYS, current one highlighted.
	tab := func(label string, active bool) string {
		if active {
			return ansiReverse + " " + label + " " + ansiReset
		}
		return " " + label + " "
	}
	disksActive := m.mode == tmDisks || m.mode == tmLibrary
	libActive := m.mode == tmLib || m.mode == tmLibPreview
	b.WriteString(moveTo(2, 1) + tab("Disks", disksActive) + " " +
		tab("Panel", m.mode == tmPanel) + " " + tab("SYS", m.mode == tmSystem) + " " +
		tab("LIB", libActive) + "   (Tab to switch)")

	row := 4
	var hint string
	switch m.mode {
	case tmDisks:
		b.WriteString(moveTo(row, 1) + "Drives:")
		row += 2
		for i, d := range m.disks {
			img := d.Image
			if img == "" {
				img = "(empty)"
			}
			tag := ""
			if d.HardDisk {
				tag += "  [HDD]"
			}
			if d.Remote {
				tag += "  [remote]"
			}
			line := fmt.Sprintf(" %-3s %s%s", d.ID+":", img, tag)
			b.WriteString(moveTo(row, 1))
			if i == m.sel {
				b.WriteString(ansiReverse + pad(line, w) + ansiReset)
			} else {
				b.WriteString(truncate(line, w))
			}
			row++
		}
		hint = "Up/Dn select   e eject   Enter/m mount   r refresh   Tab panel   q quit"
	case tmLibrary:
		b.WriteString(moveTo(row, 1) + fmt.Sprintf("Mount into %s - pick an image:", m.libFor))
		row += 2
		maxRows := rows - row - 2
		if maxRows < 1 {
			maxRows = 1
		}
		start := 0
		if m.libSel >= maxRows {
			start = m.libSel - maxRows + 1
		}
		for i := start; i < len(m.libView) && i < start+maxRows; i++ {
			line := " " + m.libView[i]
			b.WriteString(moveTo(row, 1))
			if i == m.libSel {
				b.WriteString(ansiReverse + pad(line, w) + ansiReset)
			} else {
				b.WriteString(truncate(line, w))
			}
			row++
		}
		hint = "Up/Dn/PgUp/PgDn   Enter mount   q/Esc back"
	case tmLib:
		b.WriteString(moveTo(row, 1) + fmt.Sprintf("Library (%d images):", len(m.libItems)))
		row += 2
		maxRows := rows - row - 2
		if maxRows < 1 {
			maxRows = 1
		}
		// Size the filename column to the longest name so the byte counts line up.
		nameW := 0
		for _, it := range m.libItems {
			if len(it.Filename) > nameW {
				nameW = len(it.Filename)
			}
		}
		start := 0
		if m.libItemSel >= maxRows {
			start = m.libItemSel - maxRows + 1
		}
		for i := start; i < len(m.libItems) && i < start+maxRows; i++ {
			it := m.libItems[i]
			line := fmt.Sprintf(" %-*s  %9d bytes", nameW, it.Filename, it.Size)
			b.WriteString(moveTo(row, 1))
			if i == m.libItemSel {
				b.WriteString(ansiReverse + pad(line, w) + ansiReset)
			} else {
				b.WriteString(truncate(line, w))
			}
			row++
		}
		hint = "Up/Dn/PgUp/PgDn   Enter/v view   d delete   r refresh   Tab disks   q quit"
	case tmLibPreview:
		visible := rows - row - 2
		if visible < 1 {
			visible = 1
		}
		if m.prevScroll > len(m.prevLines)-1 {
			m.prevScroll = max(0, len(m.prevLines)-1)
		}
		for i := m.prevScroll; i < len(m.prevLines) && i < m.prevScroll+visible; i++ {
			b.WriteString(moveTo(row, 1) + truncate(m.prevLines[i], w))
			row++
		}
		more := ""
		if len(m.prevLines) > visible {
			more = fmt.Sprintf("   [%d-%d/%d]", m.prevScroll+1,
				min(m.prevScroll+visible, len(m.prevLines)), len(m.prevLines))
		}
		userNav := ""
		if len(m.prevUsers) > 1 {
			userNav = fmt.Sprintf("   Left/Right user (%d/%d)", m.prevUserIdx+1, len(m.prevUsers))
		}
		hint = "Up/Dn/PgUp/PgDn scroll" + more + userNav + "   q/Esc back"
	case tmPanel:
		row = s.drawPanel(&b, row)
		hint = "Left/Right select   Up/Dn actuate   r refresh   Tab SYS   q quit"
	case tmSystem:
		visible := rows - row - 2 // leave room for hint + status
		if visible < 1 {
			visible = 1
		}
		if m.sysScroll > len(m.sysLines)-1 {
			m.sysScroll = max(0, len(m.sysLines)-1)
		}
		for i := m.sysScroll; i < len(m.sysLines) && i < m.sysScroll+visible; i++ {
			b.WriteString(moveTo(row, 1) + truncate(m.sysLines[i], w))
			row++
		}
		more := ""
		if len(m.sysLines) > visible {
			more = fmt.Sprintf("   [%d-%d/%d]", m.sysScroll+1,
				min(m.sysScroll+visible, len(m.sysLines)), len(m.sysLines))
		}
		hint = "Up/Dn/PgUp/PgDn scroll" + more + "   r refresh   Tab LIB   q quit"
	}

	// Single bottom line: a confirm/status message takes over the bar, otherwise the key hints
	// are shown, so the two can never collide on the same line (which they did when the client
	// sends no NAWS and the assumed height clamps both lines to the same physical row). The bar
	// is floored just below the content so it stays on-screen even on short windows.
	bottom := rows - 1
	if bottom < row+1 {
		bottom = row + 1
	}
	b.WriteString(moveTo(bottom, 1))
	switch {
	case m.confirm:
		b.WriteString(ansiReverse + truncate(m.confirmMsg, w) + ansiReset)
	case m.status != "":
		b.WriteString(ansiYellow + truncate(m.status, w) + ansiReset)
	default:
		b.WriteString(truncate(hint, w))
	}
	s.writeConn(b.String())
}

// ledForCol maps a switch column index to the status LED drawn above it, mirroring the real
// panel: INTER over RESET, then RUN / WAIT / HOLD over RUN / SINGLE STEP / PWR.
var ledForCol = map[int]struct {
	idx  int
	ch   byte
	name string
}{
	2: {1, 'I', "INTER"},
	3: {2, 'R', "RUN"},
	4: {3, 'W', "WAIT"},
	5: {4, 'H', "HOLD"},
}

// drawPanel renders the front panel: a row of LEDs aligned above the row of command switches,
// each LED/switch sharing a fixed-width column. Returns the next free row.
func (s *bridgeSession) drawPanel(b *strings.Builder, row int) int {
	m := s.tuiM
	s.mu.Lock()
	status := s.cpaStatus
	s.mu.Unlock()

	const colW = 12
	const margin = " "

	// LED row: a lamp centered above each switch column that has one (reverse video = lit).
	var leds strings.Builder
	for i := range panelSwitches {
		l, ok := ledForCol[i]
		if !ok {
			leds.WriteString(centerCell("", colW))
			continue
		}
		lamp := "(" + l.name + ")"
		total := colW - len(lamp)
		if total < 0 {
			total = 0
		}
		left := total / 2
		if ledOn(status, l.idx, l.ch) {
			lamp = ansiReverse + lamp + ansiReset // lit -> highlighted
		}
		leds.WriteString(strings.Repeat(" ", left) + lamp + strings.Repeat(" ", total-left))
	}
	b.WriteString(moveTo(row, 1) + margin + leds.String())
	row += 2

	// Switch rows: up label, up cell, center cell, down cell, down label.
	var top, upc, mid, dnc, bot strings.Builder
	for i, sw := range panelSwitches {
		hi, off := "", ""
		if i == m.selSw {
			hi, off = ansiReverse, ansiReset
		}
		top.WriteString(centerCell(sw.upLabel, colW))
		upc.WriteString(hi + centerCell("[ ^ ]", colW) + off)
		mid.WriteString(hi + centerCell("[ = ]", colW) + off)
		dnc.WriteString(hi + centerCell("[ v ]", colW) + off)
		bot.WriteString(centerCell(sw.downLabel, colW))
	}
	for _, line := range []string{top.String(), upc.String(), mid.String(), dnc.String(), bot.String()} {
		b.WriteString(moveTo(row, 1) + margin + line)
		row++
	}
	return row
}
