package webmtail

import (
	"bytes"
	"errors"
	"testing"
)

// header is an EBML magic plus filler standing in for Segment/Info/Tracks.
var header = append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x84}, []byte("HDR!")...)

// cluster builds a Cluster with unknown size (as a live muxer writes it),
// a Timecode, and one SimpleBlock on track 1 with the given keyframe flag.
func cluster(key bool, payload string) []byte {
	b := append([]byte{}, clusterID...)
	b = append(b, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF) // unknown size
	b = append(b, idTimecode, 0x82, 0x12, 0x34)
	flags := byte(0)
	if key {
		flags = 0x80
	}
	block := append([]byte{0x81, 0x00, 0x00, flags}, payload...)
	b = append(b, idSimpleBlock, byte(0x80|len(block)))
	return append(b, block...)
}

func file(parts ...[]byte) []byte {
	return bytes.Join(append([][]byte{header}, parts...), nil)
}

func TestReadStartsAtNewestKeyframeCluster(t *testing.T) {
	k1, d1, k2, d2 := cluster(true, "k1"), cluster(false, "d1"), cluster(true, "k2"), cluster(false, "d2")
	// The last cluster is cut short, as while the muxer is writing it.
	f := file(k1, d1, k2, d2[:len(d2)-1])
	got, err := ReadAt(bytes.NewReader(f), int64(len(f)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := file(k2, d2[:len(d2)-1])
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestReadNoKeyframe(t *testing.T) {
	f := file(cluster(false, "d1"), cluster(false, "d2"))
	if _, err := ReadAt(bytes.NewReader(f), int64(len(f)), 1<<20); !errors.Is(err, ErrNoKeyframe) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRejectsNonWebMAndEmpty(t *testing.T) {
	for _, f := range [][]byte{[]byte("ftypisom...."), header, nil} {
		if _, err := ReadAt(bytes.NewReader(f), int64(len(f)), 1<<20); err == nil {
			t.Errorf("%q accepted", f)
		}
	}
}

func TestReadIgnoresClusterIDInsidePayload(t *testing.T) {
	// A keyframe whose payload happens to contain the Cluster ID bytes
	// followed by junk must not be taken for a cluster.
	k := cluster(true, string(clusterID)+"junk")
	f := file(k)
	got, err := ReadAt(bytes.NewReader(f), int64(len(f)), 1<<20)
	if err != nil || !bytes.Equal(got, f) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestVint(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		v    uint64
		n    int
		okay bool
	}{
		{[]byte{0x81}, 1, 1, true},
		{[]byte{0x40, 0x02}, 2, 2, true},
		{[]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, 1<<56 - 1, 8, true},
		{[]byte{0x00}, 0, 0, false},
		{[]byte{0x40}, 0, 0, false},
		{nil, 0, 0, false},
	} {
		v, n, ok := vint(tc.in)
		if v != tc.v || n != tc.n || ok != tc.okay {
			t.Errorf("vint(%x) = %d %d %v", tc.in, v, n, ok)
		}
	}
}
