package driver

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// parseFuseMinor scans /proc/self/mountinfo content and returns the device
// minor of the fuse mount at mountPoint, or false if none matches.
//
// mountinfo format (proc(5)):
//
//	36 35 98:0 / /mnt rw,... - fuse.hf-mount source super_opts
//	          ^maj:min        ^sep ^fstype
//
// The optional fields between the root (field 4) and the " - " separator vary
// in count, so we locate the separator dynamically rather than by fixed index.
func parseFuseMinor(r io.Reader, mountPoint string) (int, bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[4] != mountPoint {
			continue
		}
		sep := -1
		for i, fld := range fields {
			if fld == "-" {
				sep = i
				break
			}
		}
		if sep == -1 || sep+1 >= len(fields) || !strings.HasPrefix(fields[sep+1], "fuse") {
			continue
		}
		majMin := strings.SplitN(fields[2], ":", 2)
		if len(majMin) != 2 {
			continue
		}
		minor, err := strconv.Atoi(majMin[1])
		if err != nil {
			continue
		}
		return minor, true
	}
	return 0, false
}

// fuseMount is one fuse mount record from mountinfo.
type fuseMount struct {
	minor      int
	mountpoint string
	fstype     string // e.g. "fuse" or "fuse.hf-mount"
	source     string // mount source, e.g. "hf-mount"
}

// parseFuseMounts scans mountinfo content and returns every fuse mount record.
// Unlike parseFuseMinor it does not filter by mountpoint — the sweep needs the
// full set so it can scope to our connections and resolve a minor that is
// referenced by several mountpoints (a source plus its bind targets).
//
// The mountinfo tail after the " - " separator is `<fstype> <source> <opts>`,
// so fstype is fields[sep+1] and source is fields[sep+2].
//
// A scanner error (short read, line too long) returns a non-nil error so the
// caller skips the sweep rather than acting on a truncated mount table — a
// partial table could hide the source mount that proves a connection is ours.
func parseFuseMounts(r io.Reader) ([]fuseMount, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out []fuseMount
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		sep := -1
		for i, fld := range fields {
			if fld == "-" {
				sep = i
				break
			}
		}
		if sep == -1 || sep+2 >= len(fields) {
			continue
		}
		fstype := fields[sep+1]
		if !strings.HasPrefix(fstype, "fuse") {
			continue
		}
		majMin := strings.SplitN(fields[2], ":", 2)
		if len(majMin) != 2 {
			continue
		}
		minor, err := strconv.Atoi(majMin[1])
		if err != nil {
			continue
		}
		out = append(out, fuseMount{
			minor:      minor,
			mountpoint: fields[4],
			fstype:     fstype,
			source:     fields[sep+2],
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
