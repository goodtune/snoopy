// Package webmtail extracts the end of a WebM (Matroska) file that is still
// being written, as a smaller stream that a decoder can read on its own.
//
// A recording in progress has no duration and no cues, so a decoder cannot
// seek from the end of it (ffmpeg ignores -sseof and decodes the first frame
// instead). Matroska clusters are self-contained, though: the file's header
// (EBML header, Segment, Info, Tracks: everything before the first Cluster)
// followed by any run of clusters is a valid stream, which is how live WebM
// streaming works. Read returns the header plus everything from the newest
// cluster that starts with a keyframe, so decoding it to the end yields the
// newest frame on disk.
package webmtail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// Element IDs (with their length marker bits, as they appear in the file).
var (
	ebmlMagic = []byte{0x1A, 0x45, 0xDF, 0xA3}
	clusterID = []byte{0x1F, 0x43, 0xB6, 0x75}
)

const (
	idSimpleBlock = 0xA3
	idTimecode    = 0xE7
	idPosition    = 0xA7
	idPrevSize    = 0xAB
	idCRC32       = 0xBF
	idVoid        = 0xEC

	// maxHeader bounds the search for the first cluster.
	maxHeader = 1 << 20
	// firstTail is the first amount of the file's end searched for a
	// keyframe cluster; it grows 4x per attempt up to the caller's limit.
	firstTail = 8 << 20
)

// ErrNoKeyframe means no cluster starting with a keyframe was found within
// the limit.
var ErrNoKeyframe = errors.New("webmtail: no keyframe cluster found")

// Read returns the header of the WebM file at path followed by its data
// from the newest cluster that begins with a keyframe, searching at most
// maxTail bytes back from the end.
func Read(path string, maxTail int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return ReadAt(f, fi.Size(), maxTail)
}

// ReadAt is Read for size bytes of r.
func ReadAt(r io.ReaderAt, size, maxTail int64) ([]byte, error) {
	head := make([]byte, min(size, maxHeader))
	if _, err := r.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if !bytes.HasPrefix(head, ebmlMagic) {
		return nil, errors.New("webmtail: not a Matroska/WebM file")
	}
	first := bytes.Index(head, clusterID)
	if first < 0 {
		return nil, errors.New("webmtail: no cluster yet")
	}
	header := head[:first]

	for tail := int64(firstTail); ; tail *= 4 {
		tail = min(tail, maxTail)
		start := max(int64(first), size-tail)
		buf := make([]byte, size-start)
		if _, err := r.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if p := lastKeyframeCluster(buf); p >= 0 {
			out := make([]byte, 0, len(header)+len(buf)-p)
			return append(append(out, header...), buf[p:]...), nil
		}
		if start == int64(first) || tail >= maxTail {
			return nil, fmt.Errorf("%w in the last %d bytes", ErrNoKeyframe, size-start)
		}
	}
}

// lastKeyframeCluster is the offset in b of the last Cluster whose first
// block is a keyframe, or -1.
func lastKeyframeCluster(b []byte) int {
	for end := len(b); end > 0; {
		p := bytes.LastIndex(b[:end], clusterID)
		if p < 0 {
			return -1
		}
		if keyframeCluster(b[p:]) {
			return p
		}
		end = p
	}
	return -1
}

// keyframeCluster reports whether b starts with a Cluster whose first
// SimpleBlock is a keyframe. Clusters starting with anything else
// (including a BlockGroup) are passed over.
func keyframeCluster(b []byte) bool {
	pos := len(clusterID)
	_, n, ok := vint(b[pos:])
	if !ok {
		return false
	}
	pos += n
	// The block comes after a few small elements (Timecode first).
	for range 8 {
		id, n, ok := elementID(b[pos:])
		if !ok {
			return false
		}
		pos += n
		size, n, ok := vint(b[pos:])
		if !ok {
			return false
		}
		pos += n
		switch id {
		case idSimpleBlock:
			// Track number (vint), 16-bit relative timecode, flags.
			_, n, ok := vint(b[pos:])
			if !ok || pos+n+2 >= len(b) {
				return false
			}
			return b[pos+n+2]&0x80 != 0
		case idTimecode, idPosition, idPrevSize, idCRC32, idVoid:
			if size > uint64(len(b)-pos) {
				return false
			}
			pos += int(size)
		default:
			return false
		}
	}
	return false
}

// vint decodes an EBML variable-length integer (marker bit removed); an
// all-ones value (unknown size) decodes as such.
func vint(b []byte) (v uint64, n int, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n = 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 || len(b) < n {
		return 0, 0, false
	}
	v = uint64(b[0] & (0xFF >> n))
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n, true
}

// elementID decodes an EBML element ID (1-4 bytes, marker bit kept).
func elementID(b []byte) (id uint32, n int, ok bool) {
	if len(b) == 0 || b[0] < 0x10 {
		return 0, 0, false
	}
	n = 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if len(b) < n {
		return 0, 0, false
	}
	for _, c := range b[:n] {
		id = id<<8 | uint32(c)
	}
	return id, n, true
}
