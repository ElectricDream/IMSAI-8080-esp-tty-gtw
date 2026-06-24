package main

import (
	"fmt"
	"sort"
	"strings"
)

// Minimal CP/M 2.2 directory reader for z80pack/imsaisim disk images, used by the LIB pane to
// preview the files on an image WITHOUT mounting it. Two image kinds are supported; all geometry
// facts are sourced, not guessed:
//   - 256256-byte floppy (IBM-3740 8" SSSD): docs/cpm-disk-and-boot.md (from BIOS.ASM DPBLK/TRANS).
//   - 4177920-byte hard disk: cpmtools diskdef "z80pack-hd"
//     (seclen 128, tracks 255, sectrk 128, blocksize 2048, maxdir 1024, skew 0, boottrk 0).
//
// Directory entries are identical across both: 32 bytes, [0]=user (0xE5=free; >15 = non-file
// entry), [1..8]=name, [9..11]=ext (high bit = R/O,Sys,Arc attrs), [15]=RC. With EXM=0 (both
// geometries) each entry is one <=16 KB extent, so a file's size in records is the sum of the RC
// fields of its extents.

const (
	cpmFloppySize = 256256  // 77 trk x 26 sec x 128 (IBM-3740 8" SSSD)
	cpmHDSize     = 4177920 // 255 trk x 128 sec x 128 (z80pack 4 MB hard disk)
	cpmSecSize    = 128
)

// IBM-3740 logical->physical sector skew (TRANS table, 1-based physical sectors).
var cpmTrans = []int{1, 7, 13, 19, 25, 5, 11, 17, 23, 3, 9, 15, 21, 2, 8, 14, 20, 26, 6, 12, 18, 24, 4, 10, 16, 22}

// cpmGeom describes an image's on-disk layout, enough to locate the directory.
type cpmGeom struct {
	sectrk     int   // sectors per track
	bootTrk    int   // reserved system tracks before the data area
	dirEntries int   // directory entries (maxdir)
	skew       []int // logical->physical map (1-based), len == sectrk; nil = no skew (identity)
}

var (
	cpmFloppyGeom = cpmGeom{sectrk: 26, bootTrk: 2, dirEntries: 64, skew: cpmTrans}
	cpmHDGeom     = cpmGeom{sectrk: 128, bootTrk: 0, dirEntries: 1024, skew: nil}
)

// sectorOffset returns the byte offset of data-area logical sector s (0-based).
func (g cpmGeom) sectorOffset(s int) int {
	track := g.bootTrk + s/g.sectrk
	var phys int // 1-based physical sector
	if g.skew != nil {
		phys = g.skew[s%g.sectrk]
	} else {
		phys = s%g.sectrk + 1
	}
	return track*g.sectrk*cpmSecSize + (phys-1)*cpmSecSize
}

func (g cpmGeom) dirSectors() int { return g.dirEntries * 32 / cpmSecSize }

// cpmFile is one directory file entry (folded across extents).
type cpmFile struct {
	User    int
	Name    string // "NAME.EXT"
	Records int    // total 128-byte records
}

// SizeBytes is the file size rounded up to whole records (CP/M granularity).
func (f cpmFile) SizeBytes() int { return f.Records * cpmSecSize }

// geomForSize selects the geometry from the image length.
func geomForSize(n int) (cpmGeom, error) {
	switch n {
	case cpmFloppySize:
		return cpmFloppyGeom, nil
	case cpmHDSize:
		return cpmHDGeom, nil
	default:
		return cpmGeom{}, fmt.Errorf("unsupported image size %d (not a 256K floppy or 4M hard disk)", n)
	}
}

// listCPMByUser parses the directory once and returns the files grouped by user number (0..15),
// each group sorted by name.
func listCPMByUser(img []byte) (map[int][]cpmFile, error) {
	g, err := geomForSize(len(img))
	if err != nil {
		return nil, err
	}

	// Read the directory sectors into a contiguous buffer.
	dir := make([]byte, 0, g.dirSectors()*cpmSecSize)
	for s := 0; s < g.dirSectors(); s++ {
		off := g.sectorOffset(s)
		if off+cpmSecSize > len(img) {
			return nil, fmt.Errorf("image truncated reading directory")
		}
		dir = append(dir, img[off:off+cpmSecSize]...)
	}

	type key struct {
		user int
		name string
	}
	type acc struct {
		records int
	}
	files := map[key]*acc{}
	for i := 0; i+32 <= len(dir); i += 32 {
		e := dir[i : i+32]
		u := int(e[0])
		if u > 15 { // 0xE5 = deleted/empty; CP/M3 label/timestamp entries (>=0x20) -> skip
			continue
		}
		k := key{user: u, name: cpmName(e[1:9], e[9:12])}
		rc := int(e[15])
		if a, ok := files[k]; ok {
			a.records += rc
		} else {
			files[k] = &acc{records: rc}
		}
	}

	out := map[int][]cpmFile{}
	for k, a := range files {
		out[k.user] = append(out[k.user], cpmFile{User: k.user, Name: k.name, Records: a.records})
	}
	for u := range out {
		sort.Slice(out[u], func(i, j int) bool { return out[u][i].Name < out[u][j].Name })
	}
	return out, nil
}

// cpmName builds "NAME.EXT" from the 8-byte name and 3-byte extension fields, stripping the
// high bit (used for file attributes) and trailing spaces.
func cpmName(name, ext []byte) string {
	clean := func(b []byte) string {
		var sb strings.Builder
		for _, c := range b {
			c &= 0x7f
			if c == ' ' {
				continue
			}
			sb.WriteByte(c)
		}
		return sb.String()
	}
	n := clean(name)
	x := clean(ext)
	if x == "" {
		return n
	}
	return n + "." + x
}
