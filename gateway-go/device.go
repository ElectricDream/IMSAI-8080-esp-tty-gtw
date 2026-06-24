package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// HTTP client for the device's REST API (same host/port as the WebSocket, e.g. :80).
// Used by the control TUI for disk and library operations.

func httpClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func deviceBase(cfg Config) string {
	return fmt.Sprintf("http://%s:%d", cfg.Host, cfg.WSPort)
}

type driveEntry struct {
	ID       string // drive id, e.g. "A", "I"
	Image    string // mounted image filename; "" = empty
	Remote   bool   // value had a leading '>' (remote image)
	HardDisk bool   // hard-disk slot: not removable from the TUI (set in config while OFF)
}

func isHardDisk(id, image string) bool {
	// Slot "I" is the firmware's hard-disk slot; .hdd images are hard disks.
	return id == "I" || strings.HasSuffix(strings.ToLower(image), ".hdd")
}

// getDisks returns the drive slots sorted by id. GET /disks REQUIRES a query string
// (a bare /disks makes the device drop the connection), so a timestamp is appended.
func getDisks(cfg Config) ([]driveEntry, error) {
	url := fmt.Sprintf("%s/disks?%d", deviceBase(cfg), time.Now().UnixNano())
	resp, err := httpClient().Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /disks: HTTP %d", resp.StatusCode)
	}
	var m map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	out := make([]driveEntry, 0, len(ids))
	for _, id := range ids {
		img := m[id]
		out = append(out, driveEntry{
			ID:       id,
			Image:    img,
			Remote:   strings.HasPrefix(img, ">"),
			HardDisk: isHardDisk(id, img),
		})
	}
	return out, nil
}

// getLibrary returns the on-device disk image library filenames (GET /library?S), sorted.
// The endpoint returns a JSON array of objects: [{"filename":"x.dsk","size":N}, ...].
func getLibrary(cfg Config) ([]string, error) {
	resp, err := httpClient().Get(deviceBase(cfg) + "/library?S")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /library: HTTP %d", resp.StatusCode)
	}
	var arr []struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(arr))
	for _, e := range arr {
		if e.Filename != "" {
			names = append(names, e.Filename)
		}
	}
	sort.Strings(names)
	return names, nil
}

// libEntry is one stored library image (GET /library?S).
type libEntry struct {
	Filename string
	Size     int64
}

// getLibraryDetailed returns the library images with their sizes, sorted by filename.
func getLibraryDetailed(cfg Config) ([]libEntry, error) {
	resp, err := httpClient().Get(deviceBase(cfg) + "/library?S")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /library: HTTP %d", resp.StatusCode)
	}
	var arr []struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, err
	}
	out := make([]libEntry, 0, len(arr))
	for _, e := range arr {
		if e.Filename != "" {
			out = append(out, libEntry{Filename: e.Filename, Size: e.Size})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

// getMachine returns the active machine profile id (GET /system?machine), used to build the
// file-download path /<machine>/disks/<file>.
func getMachine(cfg Config) (string, error) {
	resp, err := httpClient().Get(deviceBase(cfg) + "/system?machine")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /system?machine: HTTP %d", resp.StatusCode)
	}
	var m struct {
		Machine string `json:"machine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return "", err
	}
	if m.Machine == "" {
		return "", fmt.Errorf("empty machine id")
	}
	return m.Machine, nil
}

// downloadImage fetches a stored disk image's raw bytes: GET /<machine>/disks/<file>.
// A generous timeout is used: floppies are 256 KB but a hard-disk image is 4 MB, and the link
// may be slow Wi-Fi.
func downloadImage(cfg Config, machine, file string) ([]byte, error) {
	u := fmt.Sprintf("%s/%s/disks/%s", deviceBase(cfg), url.PathEscape(machine), url.PathEscape(file))
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", file, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// deleteLibraryImage removes a stored image: DELETE /library with the filename in the body.
// NOTE: this endpoint is derived from the WebUI client code and not yet hardware-validated.
func deleteLibraryImage(cfg Config, file string) error {
	req, err := http.NewRequest(http.MethodDelete, deviceBase(cfg)+"/library", strings.NewReader(file))
	if err != nil {
		return err
	}
	return doStatusOnly(req, "delete "+file)
}

// mountDisk mounts a library image into a drive: PUT /disks?<drive>:DSK: with the filename
// as the body. The slot must be empty (eject first to replace).
func mountDisk(cfg Config, drive, file string) error {
	url := fmt.Sprintf("%s/disks?%s:DSK:", deviceBase(cfg), drive)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(file))
	if err != nil {
		return err
	}
	return doStatusOnly(req, "mount "+drive)
}

// ejectDisk unmounts a drive: DELETE /disks?<drive>:DSK:.
func ejectDisk(cfg Config, drive string) error {
	url := fmt.Sprintf("%s/disks?%s:DSK:", deviceBase(cfg), drive)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	return doStatusOnly(req, "eject "+drive)
}

// sysInfo mirrors the parts of GET /system we display.
type sysInfo struct {
	Machine  string `json:"machine"`
	Platform string `json:"platform"`
	Network  struct {
		Interface string `json:"interface"`
		MAC       string `json:"mac_address"`
		IP        string `json:"ip_address"`
		Hostname  string `json:"hostname"`
		AP        struct {
			SSID      string `json:"SSID"`
			Channel   int    `json:"channel"`
			Signal    int    `json:"signal_strength"`
			Bandwidth string `json:"bandwidth"`
			Country   string `json:"country"`
		} `json:"access_point"`
	} `json:"network"`
	Paths  map[string]string `json:"paths"`
	System struct {
		IDFVer  string `json:"IDF_VER"`
		FreeMem int64  `json:"free_mem"`
		Time    int64  `json:"time"`
		Uptime  int64  `json:"uptime"`
	} `json:"system"`
	State struct {
		LastError int `json:"last_error"`
		CPUError  int `json:"cpu_error"`
		Reset     int `json:"reset"`
		Power     int `json:"power"`
	} `json:"state"`
	About struct {
		AppVer string `json:"APP_VER"`
		UsrCom string `json:"USR_COM"`
		UsrRel string `json:"USR_REL"`
		UsrCpr string `json:"USR_CPR"`
		CPU    string `json:"cpu"`
	} `json:"about"`
	Partitions map[string]struct {
		Label string `json:"label"`
	} `json:"partitions"`
	UARTs []struct {
		Name     string `json:"name"`
		Baudrate int    `json:"baudrate"`
		Word     int    `json:"word"`
		Stopbits int    `json:"stopbits"`
		Parity   int    `json:"parity"`
	} `json:"uarts"`
	HALPorts []struct {
		Name    string   `json:"name"`
		Devices []string `json:"devices"`
	} `json:"hal_ports"`
	MemMap []struct {
		Type string `json:"type"`
		From int    `json:"from"`
		To   int    `json:"to"`
		File string `json:"file"`
	} `json:"memmap"`
	MemExtra []string          `json:"memextra"`
	Env      map[string]string `json:"env"`
}

// getSystem fetches and parses GET /system.
func getSystem(cfg Config) (*sysInfo, error) {
	resp, err := httpClient().Get(deviceBase(cfg) + "/system")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /system: HTTP %d", resp.StatusCode)
	}
	var si sysInfo
	if err := json.NewDecoder(resp.Body).Decode(&si); err != nil {
		return nil, err
	}
	return &si, nil
}

func doStatusOnly(req *http.Request, what string) error {
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", what, resp.StatusCode)
	}
	return nil
}
