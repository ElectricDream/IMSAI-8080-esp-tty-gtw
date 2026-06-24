package main

import (
	"fmt"
	"sort"
	"strings"
)

// formatSysInfo turns GET /system into a list of display lines (section headers + "key: value"),
// for the scrollable SYS pane. The Wi-Fi password is masked.

func formatSysInfo(si *sysInfo) []string {
	var L []string
	kv := func(k, v string) { L = append(L, fmt.Sprintf("  %-13s %s", k+":", v)) }
	hdr := func(h string) {
		if len(L) > 0 {
			L = append(L, "")
		}
		L = append(L, ansiBold+h+ansiReset)
	}

	hdr("About")
	kv("Firmware", si.About.AppVer+"  ("+si.About.UsrRel+")")
	kv("Description", si.About.UsrCom)
	kv("CPU", si.About.CPU)
	kv("Copyright", si.About.UsrCpr)

	hdr("Machine")
	kv("Machine", si.Machine+" / "+si.Platform)
	power := "OFF"
	if si.State.Power != 0 {
		power = "ON"
	}
	kv("Power", power)
	kv("Errors", fmt.Sprintf("last=%d  cpu=%d  reset=%d", si.State.LastError, si.State.CPUError, si.State.Reset))

	hdr("Network")
	kv("Hostname", si.Network.Hostname)
	kv("IP", si.Network.IP+"  ("+si.Network.Interface+")")
	kv("MAC", si.Network.MAC)
	kv("Wi-Fi", fmt.Sprintf("%s  ch%d  %d dBm  %s  %s",
		si.Network.AP.SSID, si.Network.AP.Channel, si.Network.AP.Signal,
		si.Network.AP.Bandwidth, strings.TrimSpace(si.Network.AP.Country)))

	hdr("System")
	kv("IDF", si.System.IDFVer)
	kv("Free memory", fmt.Sprintf("%d bytes", si.System.FreeMem))
	kv("Uptime", fmtUptime(si.System.Uptime))

	hdr("Paths")
	for _, k := range sortedKeys(si.Paths) {
		kv(k, si.Paths[k])
	}

	hdr("Partitions")
	for _, k := range []string{"run", "boot", "next"} {
		if p, ok := si.Partitions[k]; ok {
			kv(k, p.Label)
		}
	}

	if len(si.UARTs) > 0 {
		hdr("UARTs")
		for _, u := range si.UARTs {
			kv(u.Name, fmt.Sprintf("%d baud  %d%s%d", u.Baudrate, u.Word+5, parityStr(u.Parity), u.Stopbits))
		}
	}

	if len(si.HALPorts) > 0 {
		hdr("HAL ports")
		for _, p := range si.HALPorts {
			kv(p.Name, strings.Join(p.Devices, ", "))
		}
	}

	hdr("Memory")
	for _, mm := range si.MemMap {
		line := fmt.Sprintf("%s %04X-%04X", mm.Type, mm.From, mm.To)
		if mm.File != "" {
			line += "  " + mm.File
		}
		L = append(L, "  "+line)
	}
	for _, e := range si.MemExtra {
		if strings.TrimSpace(e) != "" {
			L = append(L, "  "+e)
		}
	}

	hdr("Config (env)")
	for _, k := range sortedKeys(si.Env) {
		v := si.Env[k]
		if strings.Contains(strings.ToUpper(k), "PASSWORD") {
			v = "********"
		}
		kv(k, v)
	}
	return L
}

func fmtUptime(sec int64) string {
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm %ds", d, h, m, s)
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	default:
		return fmt.Sprintf("%dm %ds", m, s)
	}
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func parityStr(p int) string {
	switch p {
	case 1:
		return "O"
	case 2:
		return "E"
	default:
		return "N"
	}
}
