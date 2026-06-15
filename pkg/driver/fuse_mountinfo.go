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
