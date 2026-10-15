package pgarchive

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SystemIdentifier reads a data directory's system identifier: the first eight bytes of
// global/pg_control, little-endian on every platform Postgres runs on here. It names the
// cluster's folder in the bucket, so two clusters never share segments.
func SystemIdentifier(dataDir string) (string, error) {
	f, err := os.Open(filepath.Join(dataDir, "global", "pg_control"))
	if err != nil {
		return "", fmt.Errorf("reading the system identifier: %w", err)
	}
	defer f.Close()
	var b [8]byte
	if _, err := f.Read(b[:]); err != nil {
		return "", fmt.Errorf("reading the system identifier: %w", err)
	}
	id := binary.LittleEndian.Uint64(b[:])
	if id == 0 {
		return "", errors.New("pg_control has no system identifier")
	}
	return strconv.FormatUint(id, 10), nil
}

// isSegmentName reports a WAL segment file name: 24 upper-case hex digits.
func isSegmentName(s string) bool {
	if len(s) != 24 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// validArchiveName accepts what Postgres asks to archive: segments, .partial segments,
// .backup labels, and timeline .history files. Anything else (a path, a dot-dot) is refused,
// because the name becomes part of an object key and of a file path on restore.
func validArchiveName(name string) bool {
	switch {
	case isSegmentName(name):
		return true
	case strings.HasSuffix(name, ".partial"):
		return isSegmentName(strings.TrimSuffix(name, ".partial"))
	case strings.HasSuffix(name, ".history"):
		h := strings.TrimSuffix(name, ".history")
		return len(h) == 8 && isSegmentName(h+"0000000000000000")
	case strings.HasSuffix(name, ".backup"):
		parts := strings.SplitN(name, ".", 3)
		return len(parts) == 3 && isSegmentName(parts[0]) && len(parts[1]) == 8 && isSegmentName("0000000000000000"+parts[1])
	}
	return false
}

// segmentPosition is the (log, segment) part of a segment name, ignoring the timeline: the
// order pg_archivecleanup uses to decide what is older than a backup.
func segmentPosition(name string) (string, bool) {
	if len(name) < 24 || !isSegmentName(name[:24]) {
		return "", false
	}
	return name[8:24], true
}

// SegmentForLSN names the WAL segment holding lsn ("0/2000028") on timeline tli.
func SegmentForLSN(tli uint32, lsn string, segSize uint64) (string, error) {
	hi, lo, ok := strings.Cut(lsn, "/")
	if !ok {
		return "", fmt.Errorf("bad LSN %q", lsn)
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil || segSize == 0 {
		return "", fmt.Errorf("bad LSN %q", lsn)
	}
	pos := h<<32 | l
	seg := pos / segSize
	perLog := (uint64(1) << 32) / segSize
	return fmt.Sprintf("%08X%08X%08X", tli, seg/perLog, seg%perLog), nil
}
