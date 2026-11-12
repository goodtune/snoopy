package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"
)

// --- configuration -------------------------------------------------------

func TestEnvDefaultsBool(t *testing.T) {
	vars := map[string]string{
		"SNOOPY_A": "1", "SNOOPY_B": "TRUE", "SNOOPY_C": " yes ",
		"SNOOPY_D": "0", "SNOOPY_E": "false", "SNOOPY_F": "No",
		"SNOOPY_G": "", "SNOOPY_H": "maybe",
	}
	env := newEnvDefaults(lookupMap(vars))
	for name, want := range map[string]bool{"a": true, "b": true, "c": true, "d": false, "e": false, "f": false} {
		if got := env.bool(name, !want); got != want {
			t.Errorf("bool(%q) = %v, want %v", name, got, want)
		}
	}
	if !env.bool("g", true) || env.bool("g", false) {
		t.Error("empty value must fall back to the default")
	}
	if !env.bool("unset", true) {
		t.Error("unset value must fall back to the default")
	}
	if err := env.err(); err != nil {
		t.Fatalf("err() = %v, want nil", err)
	}
	if env.bool("h", true) != true {
		t.Error("invalid value must fall back to the default")
	}
	if err := env.err(); err == nil || !strings.Contains(err.Error(), `SNOOPY_H="maybe"`) {
		t.Errorf("err() = %v, want it to name SNOOPY_H", err)
	}
}

func TestResolveConfigRTSPDefaults(t *testing.T) {
	res, err := resolveConfig(managedFile{}, lookupMap(nil), nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := rtspConfig{enabled: false, addr: "0.0.0.0", port: 8554, codec: "auto", maxHeight: 1080}
	if res.opts.rtsp != want {
		t.Errorf("rtsp = %+v, want %+v", res.opts.rtsp, want)
	}
	if res.opts.addr != "127.0.0.1" {
		t.Errorf("addr = %q: the HTTP loopback default must not change", res.opts.addr)
	}
}

func TestResolveConfigRTSPFromEnvAndFlags(t *testing.T) {
	env := map[string]string{
		"SNOOPY_RTSP":            "yes",
		"SNOOPY_RTSP_ADDR":       "192.168.1.5",
		"SNOOPY_RTSP_PORT":       "9554",
		"SNOOPY_RTSP_CODEC":      "H264",
		"SNOOPY_RTSP_USER":       "viewer",
		"SNOOPY_RTSP_PASSWORD":   "env-secret",
		"SNOOPY_RTSP_MAX_HEIGHT": "720",
	}
	res, err := resolveConfig(managedFile{}, lookupMap(env), nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := rtspConfig{enabled: true, addr: "192.168.1.5", port: 9554, codec: "h264", user: "viewer", password: "env-secret", maxHeight: 720}
	if res.opts.rtsp != want {
		t.Errorf("rtsp = %+v, want %+v", res.opts.rtsp, want)
	}

	res, err = resolveConfig(managedFile{}, lookupMap(env), []string{"-rtsp=false", "-rtsp-password", "flag-secret", "-rtsp-max-height", "0"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if r := res.opts.rtsp; r.enabled || r.password != "flag-secret" || r.maxHeight != 0 {
		t.Errorf("flags must win over env: %+v", r)
	}
}

// TestResolveConfigRTSPManaged: an administrator enables the stream per
// desktop with SNOOPY_RTSP=1 in the managed file; the user cannot turn it
// off or change its credentials.
func TestResolveConfigRTSPManaged(t *testing.T) {
	mf := rootFile(map[string]string{
		"SNOOPY_MANAGED":       "1",
		"SNOOPY_RTSP":          "1",
		"SNOOPY_RTSP_USER":     "admin",
		"SNOOPY_RTSP_PASSWORD": "file-secret",
	})
	env := map[string]string{
		"SNOOPY_RTSP":          "0",
		"SNOOPY_RTSP_PASSWORD": "user-secret",
		"SNOOPY_RTSP_PORT":     "1234",
	}
	res, err := resolveConfig(mf, lookupMap(env), []string{"-rtsp=false", "-rtsp-password", "flag-secret"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := rtspConfig{enabled: true, addr: "0.0.0.0", port: 8554, codec: "auto", user: "admin", password: "file-secret", maxHeight: 1080}
	if res.opts.rtsp != want {
		t.Errorf("rtsp = %+v, want %+v", res.opts.rtsp, want)
	}
	wantIgnored := []string{"-rtsp", "-rtsp-password", "SNOOPY_RTSP", "SNOOPY_RTSP_PASSWORD", "SNOOPY_RTSP_PORT"}
	if !reflect.DeepEqual(res.ignored, wantIgnored) {
		t.Errorf("ignored = %v, want %v", res.ignored, wantIgnored)
	}
	line := strings.Join(res.ignored, ", ")
	for _, secret := range []string{"file-secret", "user-secret", "flag-secret"} {
		if strings.Contains(line, secret) {
			t.Errorf("ignored-settings line %q leaks a password", line)
		}
	}
}

func TestResolveConfigRTSPErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"user without password", map[string]string{"SNOOPY_RTSP_USER": "viewer"}, "-rtsp-user and -rtsp-password"},
		{"password without user", map[string]string{"SNOOPY_RTSP_PASSWORD": "hunter2"}, "-rtsp-user and -rtsp-password"},
		{"bad codec", map[string]string{"SNOOPY_RTSP_CODEC": "vp8"}, "-rtsp-codec"},
		{"port zero", map[string]string{"SNOOPY_RTSP_PORT": "0"}, "-rtsp-port"},
		{"port too big", map[string]string{"SNOOPY_RTSP_PORT": "70000"}, "-rtsp-port"},
		{"port not a number", map[string]string{"SNOOPY_RTSP_PORT": "rtsp"}, "SNOOPY_RTSP_PORT"},
		{"tiny max height", map[string]string{"SNOOPY_RTSP_MAX_HEIGHT": "50"}, "-rtsp-max-height"},
		{"negative max height", map[string]string{"SNOOPY_RTSP_MAX_HEIGHT": "-1"}, "-rtsp-max-height"},
		{"bad bool", map[string]string{"SNOOPY_RTSP": "on"}, `SNOOPY_RTSP="on"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveConfig(managedFile{}, lookupMap(tt.env), nil, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q leaks the password", err)
			}
		})
	}
}

func TestHelpNeverShowsPassword(t *testing.T) {
	env := map[string]string{"SNOOPY_RTSP_USER": "viewer", "SNOOPY_RTSP_PASSWORD": "env-secret"}
	for name, mf := range map[string]managedFile{
		"unmanaged": {},
		"managed":   rootFile(map[string]string{"SNOOPY_MANAGED": "1", "SNOOPY_RTSP_USER": "a", "SNOOPY_RTSP_PASSWORD": "file-secret"}),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := resolveConfig(mf, lookupMap(env), []string{"-help"}, &out)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("err = %v, want flag.ErrHelp", err)
			}
			help := out.String()
			for _, want := range []string{"-rtsp-password", "SNOOPY_RTSP_PASSWORD", "-rtsp-max-height", "SNOOPY_RTSP"} {
				if !strings.Contains(help, want) {
					t.Errorf("help lacks %q", want)
				}
			}
			for _, secret := range []string{"env-secret", "file-secret"} {
				if strings.Contains(help, secret) {
					t.Errorf("help leaks the password:\n%s", help)
				}
			}
		})
	}
}

// --- encoder selection and arguments -------------------------------------

// encodersOutput imitates `ffmpeg -hide_banner -encoders`.
func encodersOutput(names ...string) string {
	var b strings.Builder
	b.WriteString("Encoders:\n V..... = Video\n A..... = Audio\n S..... = Subtitle\n .F.... = Frame-level multithreading\n ------\n")
	for _, n := range names {
		fmt.Fprintf(&b, " V....D %-20s some description (codec x)\n", n)
	}
	b.WriteString(" A....D aac                  AAC (Advanced Audio Coding)\n")
	return b.String()
}

func TestParseEncoders(t *testing.T) {
	got := parseEncoders(encodersOutput("mpeg4", "libx264", "h264_vaapi"))
	for _, n := range []string{"mpeg4", "libx264", "h264_vaapi"} {
		if !got[n] {
			t.Errorf("%s missing from %v", n, got)
		}
	}
	for _, n := range []string{"aac", "=", "Video", "------"} {
		if got[n] {
			t.Errorf("%q must not be listed as a video encoder", n)
		}
	}
}

func TestEncoderCandidates(t *testing.T) {
	// Fedora's ffmpeg-free (enable_encoders in src.fedoraproject.org/rpms/ffmpeg):
	// mpeg4, libopenh264 and the hardware h264_* encoders; no libx264.
	fedora := encodersOutput("mjpeg", "mpeg1video", "mpeg2video", "mpeg4", "libopenh264",
		"h264_amf", "h264_nvenc", "h264_qsv", "h264_v4l2m2m", "h264_vaapi", "h264_vulkan", "libvpx")
	ubuntu := encodersOutput("mjpeg", "mpeg4", "libx264", "libx264rgb", "h264_vaapi", "h264_v4l2m2m")
	noMPEG4 := encodersOutput("mjpeg", "libopenh264", "h264_vaapi")
	bare := encodersOutput("mjpeg", "libvpx")

	tests := []struct {
		name, mode, list string
		want             []string
		err              string
	}{
		{"fedora auto", "auto", fedora, []string{"mpeg4", "libopenh264", "h264_vaapi"}, ""},
		{"fedora mpeg4", "mpeg4", fedora, []string{"mpeg4"}, ""},
		{"fedora h264", "h264", fedora, []string{"libopenh264", "h264_vaapi"}, ""},
		{"ubuntu auto", "auto", ubuntu, []string{"mpeg4", "libx264", "h264_vaapi"}, ""},
		{"ubuntu h264", "h264", ubuntu, []string{"libx264", "h264_vaapi"}, ""},
		{"no mpeg4 auto", "auto", noMPEG4, []string{"libopenh264", "h264_vaapi"}, ""},
		{"no mpeg4 forced", "mpeg4", noMPEG4, nil, "no mpeg4"},
		{"no h264 forced", "h264", bare, nil, "no H.264 encoder"},
		{"nothing", "auto", bare, nil, "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encoderCandidates(tt.mode, parseEncoders(tt.list))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, c := range got {
				names = append(names, c.encoder)
				wantCodec := rtspCodecH264
				if c.encoder == "mpeg4" {
					wantCodec = rtspCodecMPEG4
				}
				if c.codec != wantCodec {
					t.Errorf("%s: codec %s, want %s", c.encoder, c.codec, wantCodec)
				}
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Errorf("candidates = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestOutputDims(t *testing.T) {
	tests := []struct{ w, h, max, ww, wh int }{
		{1920, 1080, 1080, 1920, 1080},
		{3840, 2160, 1080, 1920, 1080},
		{2560, 1600, 1080, 1728, 1080},
		{1366, 768, 1080, 1366, 768},
		{801, 601, 1080, 800, 600},  // odd source: rounded down to even
		{5120, 2880, 0, 5120, 2880}, // 0: no limit
		{3000, 2001, 1001, 1500, 1000},
	}
	for _, tt := range tests {
		w, h := outputDims(tt.w, tt.h, tt.max)
		if w != tt.ww || h != tt.wh {
			t.Errorf("outputDims(%d, %d, %d) = %dx%d, want %dx%d", tt.w, tt.h, tt.max, w, h, tt.ww, tt.wh)
		}
		if w%2 != 0 || h%2 != 0 {
			t.Errorf("outputDims(%d, %d, %d) = %dx%d: not even", tt.w, tt.h, tt.max, w, h)
		}
	}
}

func TestFFmpegArgs(t *testing.T) {
	tests := []struct {
		enc      encoderChoice
		contains []string
	}{
		{encoderChoice{rtspCodecMPEG4, "mpeg4"}, []string{"-c:v mpeg4", "-f m4v pipe:1", "scale=1280:720:flags=bicubic,format=yuv420p"}},
		{encoderChoice{rtspCodecH264, "libx264"}, []string{"-c:v libx264", "-tune zerolatency", "-f h264 pipe:1"}},
		{encoderChoice{rtspCodecH264, "libopenh264"}, []string{"-c:v libopenh264", "-f h264 pipe:1"}},
		{encoderChoice{rtspCodecH264, "h264_vaapi"}, []string{"-vaapi_device " + rtspVAAPIDevice, "format=nv12,hwupload", "-c:v h264_vaapi"}},
	}
	for _, tt := range tests {
		t.Run(tt.enc.encoder, func(t *testing.T) {
			line := strings.Join(ffmpegArgs(tt.enc, 1280, 720), " ")
			common := []string{"-f image2pipe -c:v mjpeg", "-i pipe:0", "-g 2", "-bf 0", "-flush_packets 1", "-loglevel error"}
			for _, want := range append(common, tt.contains...) {
				if !strings.Contains(line, want) {
					t.Errorf("args %q lack %q", line, want)
				}
			}
			if strings.Index(line, "-i pipe:0") > strings.Index(line, "-c:v "+tt.enc.encoder) {
				t.Errorf("encoder must be an output option: %q", line)
			}
		})
	}
}

// --- access unit splitting -----------------------------------------------

// pushChunked feeds data to sp in chunks of n bytes.
func pushChunked(sp auSplitter, data []byte, n int) []accessUnit {
	var out []accessUnit
	for len(data) > 0 {
		k := min(n, len(data))
		out = append(out, sp.push(data[:k])...)
		data = data[k:]
	}
	return out
}

func TestMPEG4Splitter(t *testing.T) {
	vos := []byte{0, 0, 1, 0xB0, 0x01}
	vo := []byte{0, 0, 1, 0xB5, 0x89, 0x13}
	vobj := []byte{0, 0, 1, 0x00}
	vol := []byte{0, 0, 1, 0x20, 0x00, 0xC8, 0x88}
	ud := []byte{0, 0, 1, 0xB2, 'L', 'a', 'v', 'c'}
	gov := []byte{0, 0, 1, 0xB3, 0x00, 0x10}
	iVOP := []byte{0, 0, 1, 0xB6, 0x10, 0xAA, 0xBB}
	pVOP := []byte{0, 0, 1, 0xB6, 0x50, 0xCC}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	config := cat(vos, vo, vobj, vol, ud)
	frame1 := cat(config, gov, iVOP)
	frame2 := pVOP
	frame3 := cat(config, gov, iVOP)
	stream := cat(frame1, frame2, []byte{0}, frame3, pVOP) // a zero stuffing byte, then the next frame

	for _, chunk := range []int{1, 2, 3, 7, len(stream)} {
		t.Run(strconv.Itoa(chunk), func(t *testing.T) {
			aus := pushChunked(&mpeg4Splitter{}, stream, chunk)
			if len(aus) != 3 { // the last frame waits for the next start code
				t.Fatalf("got %d access units, want 3", len(aus))
			}
			for i, want := range [][]byte{frame1, frame2, frame3} {
				if got := bytes.Join(aus[i].units, nil); !bytes.Equal(got, want) {
					t.Errorf("au %d = % x, want % x", i, got, want)
				}
			}
			if !aus[0].key || aus[1].key || !aus[2].key {
				t.Errorf("key flags = %v %v %v, want true false true", aus[0].key, aus[1].key, aus[2].key)
			}
			if !aus[0].inband || aus[1].inband {
				t.Error("inband must be set only when the VOL is in the frame")
			}
			for i, au := range aus {
				if len(au.params) != 1 || !bytes.Equal(au.params[0], config) {
					t.Errorf("au %d params = % x, want % x", i, au.params, config)
				}
				if au.plid != 1 {
					t.Errorf("au %d plid = %d, want 1", i, au.plid)
				}
			}
		})
	}
}

func TestH264Splitter(t *testing.T) {
	sc := []byte{0, 0, 0, 1}
	aud := []byte{0x09, 0xF0}
	sps := []byte{0x67, 0x42, 0xC0, 0x1F}
	pps := []byte{0x68, 0xCE, 0x3C, 0x80}
	sei := []byte{0x06, 0x05, 0x01}
	idr := []byte{0x65, 0x88, 0x84}    // first_mb_in_slice 0
	idr2 := []byte{0x65, 0x40, 0x11}   // second slice of the same picture
	pslice := []byte{0x41, 0x9A, 0x02} // first_mb_in_slice 0
	var stream []byte
	add := func(nalus ...[]byte) {
		for _, n := range nalus {
			stream = append(append(stream, sc...), n...)
		}
	}
	add(aud, sps, pps, sei, idr, idr2) // AU 1: keyframe, two slices
	add(aud, pslice)                   // AU 2
	add(pslice)                        // AU 3: no AUD
	add(idr)                           // AU 4: keyframe without SPS/PPS
	add(aud)                           // starts AU 5, completing AU 4

	for _, chunk := range []int{1, 3, 5, len(stream)} {
		t.Run(strconv.Itoa(chunk), func(t *testing.T) {
			aus := pushChunked(&h264Splitter{}, stream, chunk)
			want := [][][]byte{{sps, pps, sei, idr, idr2}, {pslice}, {pslice}, {idr}}
			if len(aus) != len(want) {
				t.Fatalf("got %d access units, want %d", len(aus), len(want))
			}
			for i := range want {
				if !reflect.DeepEqual(aus[i].units, want[i]) {
					t.Errorf("au %d = % x, want % x", i, aus[i].units, want[i])
				}
				if !reflect.DeepEqual(aus[i].params, [][]byte{sps, pps}) {
					t.Errorf("au %d params = % x", i, aus[i].params)
				}
			}
			if !aus[0].key || aus[1].key || aus[2].key || !aus[3].key {
				t.Error("wrong key flags")
			}
			if !aus[0].inband || aus[3].inband {
				t.Error("inband must be set only when SPS and PPS are in the access unit")
			}
		})
	}
}

func TestFrameHubKeepsNewest(t *testing.T) {
	hub := newFrameHub()
	ch, unsub := hub.Subscribe()
	hub.Publish([]byte("a"))
	hub.Publish([]byte("b")) // replaces a: never blocks the capture loop
	if got := string(<-ch); got != "b" {
		t.Errorf("got %q, want b", got)
	}
	unsub()
	hub.Publish([]byte("c"))
	select {
	case b := <-ch:
		t.Errorf("received %q after unsubscribing", b)
	default:
	}
}

// --- integration: real ffmpeg, real RTSP ---------------------------------

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
}

func solidJPEG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	red  = color.RGBA{220, 20, 20, 255}
	blue = color.RGBA{20, 20, 220, 255}
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// testCodecs returns the encoder to test for each codec ffmpeg supports.
func testCodecs(t *testing.T) map[string]encoderChoice {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatal(err)
	}
	avail := parseEncoders(string(out))
	codecs := map[string]encoderChoice{}
	for _, mode := range []string{rtspCodecMPEG4, rtspCodecH264} {
		cands, err := encoderCandidates(mode, avail)
		if err != nil {
			t.Logf("%s: %v (not tested)", mode, err)
			continue
		}
		// h264_vaapi needs a GPU; prefer a software encoder.
		codecs[mode] = cands[0]
	}
	return codecs
}

type testStream struct {
	s    *rtspStreamer
	hub  *frameHub
	port int
}

func startTestStream(t *testing.T, enc encoderChoice, cfg rtspConfig, placeholder []byte) *testStream {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	cfg.enabled = true
	cfg.addr = "127.0.0.1"
	cfg.port = freePort(t)
	cfg.codec = enc.codec
	hub := newFrameHub()
	s, err := newRTSPStreamer(context.Background(), cfg, ffmpeg, []encoderChoice{enc}, hub, placeholder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return &testStream{s: s, hub: hub, port: cfg.port}
}

func (ts *testStream) url(userinfo, path string) string {
	if userinfo != "" {
		userinfo += "@"
	}
	return fmt.Sprintf("rtsp://%s127.0.0.1:%d%s", userinfo, ts.port, path)
}

// rtspRead connects to rawURL and waits for a keyframe. It returns the
// described format, or the first error.
func rtspRead(rawURL string, timeout time.Duration) (format.Format, error) {
	u, err := base.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: new(gortsplib.ProtocolTCP)}
	if err := c.Start(); err != nil {
		return nil, err
	}
	defer c.Close()

	var desc *description.Session
	deadline := time.Now().Add(timeout)
	for {
		desc, _, err = c.Describe(u)
		var bad liberrors.ErrClientBadStatusCode
		if err == nil || !errors.As(err, &bad) || bad.Code != base.StatusServiceUnavailable || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond) // encoder still starting
	}
	if err != nil {
		return nil, err
	}
	if len(desc.Medias) != 1 || len(desc.Medias[0].Formats) != 1 {
		return nil, fmt.Errorf("want one video track, got %+v", desc.Medias)
	}
	medi := desc.Medias[0]
	forma := medi.Formats[0]

	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return nil, err
	}
	keyframe := make(chan struct{})
	var once sync.Once
	switch f := forma.(type) {
	case *format.MPEG4Video:
		dec, err := f.CreateDecoder()
		if err != nil {
			return nil, err
		}
		c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
			frame, err := dec.Decode(pkt)
			if err != nil {
				return
			}
			// A VOP of type I preceded by a VOL header.
			if i := bytes.Index(frame, []byte{0, 0, 1, 0xB6}); i >= 0 && i+4 < len(frame) && frame[i+4]>>6 == 0 &&
				bytes.Contains(frame[:i], []byte{0, 0, 1, 0x20}) {
				once.Do(func() { close(keyframe) })
			}
		})
	case *format.H264:
		dec, err := f.CreateDecoder()
		if err != nil {
			return nil, err
		}
		c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
			au, err := dec.Decode(pkt)
			if err != nil {
				return
			}
			if h264.IsRandomAccess(au) && h264NALPresent(au, h264.NALUTypeSPS) {
				once.Do(func() { close(keyframe) })
			}
		})
	default:
		return nil, fmt.Errorf("unexpected format %T", forma)
	}
	if _, err := c.Play(nil); err != nil {
		return nil, err
	}
	select {
	case <-keyframe:
		return forma, nil
	case <-time.After(time.Until(deadline) + 2*rtspGOP*time.Second):
		return forma, errors.New("no keyframe received")
	}
}

func h264NALPresent(au [][]byte, typ h264.NALUType) bool {
	for _, n := range au {
		if h264.NALUType(n[0]&0x1F) == typ {
			return true
		}
	}
	return false
}

func ffprobe(t *testing.T, rawURL string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-rtsp_transport", "tcp", "-analyzeduration", "1000000", "-select_streams", "v",
		"-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", rawURL).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe %s: %v\n%s", rawURL, err, out)
	}
	return strings.TrimSpace(string(out))
}

// grabColour decodes one frame from rawURL and returns its average colour.
func grabColour(t *testing.T, rawURL string) color.RGBA {
	t.Helper()
	// Decode keyframes only: a reader joining on a P-frame would get grey.
	out, err := exec.Command("ffmpeg", "-v", "error", "-rtsp_transport", "tcp", "-analyzeduration", "1000000", "-skip_frame", "nokey", "-i", rawURL,
		"-frames:v", "1", "-vf", "scale=1:1", "-f", "rawvideo", "-pix_fmt", "rgb24", "-").Output()
	if err != nil || len(out) < 3 {
		t.Fatalf("grab frame from %s: %v (%d bytes)", rawURL, err, len(out))
	}
	return color.RGBA{out[0], out[1], out[2], 255}
}

func near(a, b color.RGBA) bool {
	d := func(x, y uint8) int { return max(int(x)-int(y), int(y)-int(x)) }
	return d(a.R, b.R) < 40 && d(a.G, b.G) < 40 && d(a.B, b.B) < 40
}

func TestRTSPIntegration(t *testing.T) {
	requireFFmpeg(t)
	codecs := testCodecs(t)
	if len(codecs) == 0 {
		t.Skip("ffmpeg has no usable encoder")
	}
	wantName := map[string]string{rtspCodecMPEG4: "mpeg4", rtspCodecH264: "h264"}

	for _, codec := range []string{rtspCodecMPEG4, rtspCodecH264} {
		enc, ok := codecs[codec]
		if !ok {
			continue
		}
		t.Run(codec, func(t *testing.T) {
			t.Parallel()
			placeholder := solidJPEG(t, 640, 480, color.RGBA{40, 40, 40, 255})
			ts := startTestStream(t, enc, rtspConfig{maxHeight: 1080}, placeholder)

			t.Run("gortsplib reads any path", func(t *testing.T) {
				for _, path := range []string{"", "/", "/anything/else"} {
					forma, err := rtspRead(ts.url("", path), 10*time.Second)
					if err != nil {
						t.Fatalf("path %q: %v", path, err)
					}
					switch forma.(type) {
					case *format.MPEG4Video:
						if codec != rtspCodecMPEG4 {
							t.Errorf("path %q: got MPEG-4 Part 2, want %s", path, codec)
						}
					case *format.H264:
						if codec != rtspCodecH264 {
							t.Errorf("path %q: got H.264, want %s", path, codec)
						}
					}
				}
			})

			t.Run("ffprobe sees the placeholder", func(t *testing.T) {
				want := wantName[codec] + ",640,480"
				if got := ffprobe(t, ts.url("", "/")); got != want {
					t.Errorf("ffprobe = %q, want %q", got, want)
				}
			})

			t.Run("frames change and are scaled", func(t *testing.T) {
				ts.hub.Publish(solidJPEG(t, 1920, 1200, red)) // downscaled to 1728x1080
				waitFor(t, 15*time.Second, "1728x1080 stream", func() bool {
					return ffprobe(t, ts.url("", "/")) == wantName[codec]+",1728,1080"
				})
				if c := grabColour(t, ts.url("", "")); !near(c, red) {
					t.Errorf("colour = %v, want red", c)
				}
				ts.hub.Publish(solidJPEG(t, 1920, 1200, blue)) // same size: same stream
				waitFor(t, 15*time.Second, "blue frame", func() bool {
					return near(grabColour(t, ts.url("", "/")), blue)
				})
			})

			t.Run("publishing is refused", func(t *testing.T) {
				c := gortsplib.Client{Protocol: new(gortsplib.ProtocolTCP)}
				desc := &description.Session{Medias: []*description.Media{{
					Type:    description.MediaTypeVideo,
					Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}},
				}}}
				for _, path := range []string{"/", "/publish", ""} {
					err := c.StartRecording(ts.url("", path), desc)
					if err == nil {
						c.Close()
						t.Fatalf("publish to %q accepted", path)
					}
					var bad liberrors.ErrClientBadStatusCode
					if !errors.As(err, &bad) || bad.Code != base.StatusForbidden {
						t.Errorf("publish to %q: err = %v, want 403", path, err)
					}
				}
			})

			t.Run("encoder restarts after being killed", func(t *testing.T) {
				// A reader connected across the restart keeps its stream.
				u, _ := base.ParseURL(ts.url("", "/"))
				c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: new(gortsplib.ProtocolTCP)}
				if err := c.Start(); err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				desc, _, err := c.Describe(u)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				var last time.Time
				c.OnPacketRTPAny(func(*description.Media, format.Format, *rtp.Packet) {
					mu.Lock()
					last = time.Now()
					mu.Unlock()
				})
				if _, err := c.Play(nil); err != nil {
					t.Fatal(err)
				}

				pid := ts.s.encoderPID()
				if pid == 0 {
					t.Fatal("no encoder running")
				}
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				killed := time.Now()
				waitFor(t, 15*time.Second, "a new encoder", func() bool {
					p := ts.s.encoderPID()
					return p != 0 && p != pid
				})
				waitFor(t, 15*time.Second, "packets after the restart", func() bool {
					mu.Lock()
					defer mu.Unlock()
					return last.After(killed.Add(time.Second))
				})
				if _, err := rtspRead(ts.url("", "/"), 10*time.Second); err != nil {
					t.Errorf("new reader after restart: %v", err)
				}
			})
		})
	}
}

func TestRTSPAuthentication(t *testing.T) {
	requireFFmpeg(t)
	codecs := testCodecs(t)
	enc, ok := codecs[rtspCodecMPEG4]
	if !ok {
		for _, e := range codecs {
			enc, ok = e, true
		}
	}
	if !ok {
		t.Skip("ffmpeg has no usable encoder")
	}
	ts := startTestStream(t, enc, rtspConfig{user: "viewer", password: "s3cret pass", maxHeight: 1080},
		solidJPEG(t, 320, 240, red))

	for _, userinfo := range []string{"", "viewer:wrong", "other:s3cret%20pass"} {
		_, err := rtspRead(ts.url(userinfo, "/"), 5*time.Second)
		var bad liberrors.ErrClientBadStatusCode
		if !errors.As(err, &bad) || bad.Code != base.StatusUnauthorized {
			t.Errorf("credentials %q: err = %v, want 401", userinfo, err)
		}
	}
	if _, err := rtspRead(ts.url("viewer:s3cret%20pass", "/"), 10*time.Second); err != nil {
		t.Errorf("authenticated read: %v", err)
	}
	if got := ffprobe(t, ts.url("viewer:s3cret%20pass", "")); !strings.HasSuffix(got, ",320,240") {
		t.Errorf("authenticated ffprobe = %q", got)
	}
}

// TestRTSPPortInUse: a busy port is reported, not fatal.
func TestRTSPPortInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	cfg := rtspConfig{enabled: true, addr: "127.0.0.1", port: l.Addr().(*net.TCPAddr).Port, codec: "mpeg4"}
	_, err = newRTSPStreamer(context.Background(), cfg, "/nonexistent/ffmpeg",
		[]encoderChoice{{rtspCodecMPEG4, "mpeg4"}}, newFrameHub(), solidJPEG(t, 64, 48, red))
	if err == nil || !strings.Contains(err.Error(), "RTSP server on") {
		t.Fatalf("err = %v, want a bind error", err)
	}
}

// TestRTSPEncoderMissingKeepsRetrying: an encoder that cannot start is
// retried with backoff and Close still returns promptly.
func TestRTSPEncoderMissingKeepsRetrying(t *testing.T) {
	cfg := rtspConfig{enabled: true, addr: "127.0.0.1", port: freePort(t), codec: "auto"}
	s, err := newRTSPStreamer(context.Background(), cfg, "/nonexistent/ffmpeg",
		[]encoderChoice{{rtspCodecMPEG4, "mpeg4"}, {rtspCodecH264, "libx264"}}, newFrameHub(), solidJPEG(t, 64, 48, red))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
}

func TestCanonicalRTSPURL(t *testing.T) {
	for in, want := range map[string]string{
		"rtsp://h:8554":                      "rtsp://h:8554/screen",
		"rtsp://h:8554/":                     "rtsp://h:8554/screen",
		"rtsp://h:8554//":                    "rtsp://h:8554/screen",
		"rtsp://h:8554/foo/bar?x=1":          "rtsp://h:8554/screen",
		"rtsp://h:8554/trackID=0":            "rtsp://h:8554/screen/trackID=0",
		"rtsp://h:8554//trackID=0":           "rtsp://h:8554/screen/trackID=0",
		"rtsp://h:8554/screen/trackID=0":     "rtsp://h:8554/screen/trackID=0",
		"rtsp://h:8554/path?query/trackID=1": "rtsp://h:8554/screen/trackID=1",
		"rtsp://u:p@h:8554/":                 "rtsp://u:p@h:8554/screen",
	} {
		u, err := base.ParseURL(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalRTSPURL(u).String(); got != want {
			t.Errorf("canonicalRTSPURL(%s) = %s, want %s", in, got, want)
		}
	}
}
