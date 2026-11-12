package main

// Optional RTSP broadcast of the captured screen frames.
//
// Architecture: snoopy runs one supervised ffmpeg child that reads the
// latest JPEG frame from stdin once per second (image2pipe), scales it and
// encodes it as an MPEG-4 Part 2 (or H.264) elementary stream on stdout.
// snoopy splits that stream into access units at start codes and
// packetises them itself (gortsplib's RTP encoders) onto a ServerStream of
// its own RTSP server. Nothing ever publishes to the server: ANNOUNCE and
// RECORD are refused for everyone, so there is no publish path to protect,
// and the stream (and its readers) survives an encoder restart as long as
// the codec parameters stay the same.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig for captured frames
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/auth"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpfragmented"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"
)

const (
	rtspCodecAuto  = "auto"
	rtspCodecMPEG4 = "mpeg4"
	rtspCodecH264  = "h264"

	// rtspFrameInterval is how often the latest frame is fed to the
	// encoder (the stream's frame rate), and rtspGOP the keyframe interval
	// in frames: a new reader gets a keyframe within rtspGOP seconds.
	rtspFrameInterval = time.Second
	rtspGOP           = 2

	// rtspVAAPIDevice is the render node used by the h264_vaapi encoder.
	rtspVAAPIDevice = "/dev/dri/renderD128"

	rtspMinMaxHeight = 120
)

// h264Encoders are the ffmpeg H.264 encoders snoopy can use, in order of
// preference. libx264 is absent from Fedora's ffmpeg-free; libopenh264
// needs Cisco's openh264 (not the noopenh264 stub); h264_vaapi needs a GPU
// driver with H.264 encoding.
var h264Encoders = []string{"libx264", "libopenh264", "h264_vaapi"}

// rtspConfig is the RTSP part of options.
type rtspConfig struct {
	enabled   bool
	addr      string
	port      int
	codec     string // auto, mpeg4 or h264
	user      string
	password  string
	maxHeight int // 0: keep the source height
}

// validate checks the settings. Errors never contain the password.
func (c *rtspConfig) validate() error {
	c.codec = strings.ToLower(strings.TrimSpace(c.codec))
	switch c.codec {
	case rtspCodecAuto, rtspCodecMPEG4, rtspCodecH264:
	default:
		return fmt.Errorf("-rtsp-codec %q: must be auto, mpeg4 or h264", c.codec)
	}
	if c.port < 1 || c.port > 65535 {
		return fmt.Errorf("-rtsp-port %d: must be between 1 and 65535", c.port)
	}
	if (c.user == "") != (c.password == "") {
		return errors.New("-rtsp-user and -rtsp-password must be set together (or neither)")
	}
	if c.maxHeight != 0 && c.maxHeight < rtspMinMaxHeight {
		return fmt.Errorf("-rtsp-max-height %d: must be 0 (keep source size) or at least %d", c.maxHeight, rtspMinMaxHeight)
	}
	return nil
}

// frameHub fans captured JPEG frames out to subscribers. Each subscriber
// has a one-slot channel that always holds the newest frame: a slow
// subscriber skips frames and never blocks the capture loop.
type frameHub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newFrameHub() *frameHub {
	return &frameHub{subs: make(map[chan []byte]struct{})}
}

// Publish hands frame to every subscriber, replacing any frame they have
// not taken yet. Subscribers must not modify it.
func (h *frameHub) Publish(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case <-ch: // drop the stale frame
		default:
		}
		ch <- frame
	}
}

// Subscribe returns a channel of new frames and a function that ends the
// subscription.
func (h *frameHub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// encoderChoice is one ffmpeg encoder snoopy can stream with.
type encoderChoice struct {
	codec   string // rtspCodecMPEG4 or rtspCodecH264
	encoder string // ffmpeg encoder name
}

func (e encoderChoice) String() string {
	if e.codec == rtspCodecMPEG4 {
		return "MPEG-4 Part 2 (" + e.encoder + ")"
	}
	return "H.264 (" + e.encoder + ")"
}

// parseEncoders returns the names of the video encoders listed by
// `ffmpeg -hide_banner -encoders`.
func parseEncoders(out string) map[string]bool {
	avail := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || len(f[0]) != 6 || f[0][0] != 'V' || f[0] == "V....." && f[1] == "=" {
			continue
		}
		avail[f[1]] = true
	}
	return avail
}

// encoderCandidates lists the encoders to try for mode, in order. In auto
// mode the native MPEG-4 Part 2 encoder comes first (it is in every ffmpeg
// build, Fedora's ffmpeg-free included), then the H.264 encoders.
func encoderCandidates(mode string, avail map[string]bool) ([]encoderChoice, error) {
	var mpeg4, h264 []encoderChoice
	if avail["mpeg4"] {
		mpeg4 = append(mpeg4, encoderChoice{rtspCodecMPEG4, "mpeg4"})
	}
	for _, e := range h264Encoders {
		if avail[e] {
			h264 = append(h264, encoderChoice{rtspCodecH264, e})
		}
	}
	switch mode {
	case rtspCodecMPEG4:
		if len(mpeg4) == 0 {
			return nil, errors.New("ffmpeg has no mpeg4 (MPEG-4 Part 2) encoder")
		}
		return mpeg4, nil
	case rtspCodecH264:
		if len(h264) == 0 {
			return nil, fmt.Errorf("ffmpeg has no H.264 encoder (looked for %s)", strings.Join(h264Encoders, ", "))
		}
		return h264, nil
	default:
		all := append(mpeg4, h264...)
		if len(all) == 0 {
			return nil, fmt.Errorf("ffmpeg has neither an mpeg4 nor an H.264 encoder (%s)", strings.Join(h264Encoders, ", "))
		}
		return all, nil
	}
}

// outputDims returns the encoded size for a srcW x srcH frame: the source
// size, scaled down to maxHeight (0: no limit) keeping the aspect ratio,
// rounded down to even numbers as 4:2:0 encoders require.
func outputDims(srcW, srcH, maxHeight int) (int, int) {
	w, h := srcW, srcH
	if maxHeight > 0 && h > maxHeight {
		w = int(float64(srcW)*float64(maxHeight)/float64(srcH) + 0.5)
		h = maxHeight
	}
	w, h = w&^1, h&^1
	return max(w, 2), max(h, 2)
}

// ffmpegArgs builds the encoder command line: JPEG frames on stdin, an
// elementary stream on stdout, a keyframe every rtspGOP frames and no
// B-frames (so each input frame comes out as one access unit).
func ffmpegArgs(enc encoderChoice, w, h int) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats"}
	if enc.encoder == "h264_vaapi" {
		args = append(args, "-vaapi_device", rtspVAAPIDevice)
	}
	args = append(args,
		"-probesize", "32", "-analyzeduration", "0", "-fflags", "nobuffer",
		"-f", "image2pipe", "-c:v", "mjpeg", "-framerate", "1", "-i", "pipe:0",
		"-an", "-sn",
	)
	scale := fmt.Sprintf("scale=%d:%d:flags=bicubic", w, h)
	switch enc.encoder {
	case "h264_vaapi":
		args = append(args, "-vf", scale+",format=nv12,hwupload", "-c:v", "h264_vaapi", "-qp", "26")
	case "libx264":
		args = append(args, "-vf", scale+",format=yuv420p", "-c:v", "libx264",
			"-preset", "ultrafast", "-tune", "zerolatency", "-crf", "26", "-sc_threshold", "0", "-threads", "1")
	case "libopenh264":
		args = append(args, "-vf", scale+",format=yuv420p", "-c:v", "libopenh264", "-b:v", "2M", "-threads", "1")
	default: // mpeg4
		args = append(args, "-vf", scale+",format=yuv420p", "-c:v", "mpeg4", "-q:v", "4", "-threads", "1")
	}
	muxer := "m4v"
	if enc.codec == rtspCodecH264 {
		muxer = "h264"
	}
	args = append(args, "-g", strconv.Itoa(rtspGOP), "-bf", "0", "-flush_packets", "1", "-f", muxer, "pipe:1")
	return args
}

// accessUnit is one encoded frame.
type accessUnit struct {
	units  [][]byte // MPEG-4: start-code units incl. 00 00 01; H.264: NAL units
	key    bool
	params [][]byte // latest MPEG-4 config, or H.264 SPS and PPS; nil if not seen yet
	inband bool     // the parameters are already inside units
	plid   int      // MPEG-4 profile-level-id
}

// startCodeScanner splits a byte stream at 00 00 01 start codes. A unit is
// returned once the next start code shows where it ends.
type startCodeScanner struct {
	buf      []byte
	scanFrom int
	inUnit   bool // buf starts with a start code
}

// pending returns the bytes received so far of the unit after the last
// start code (nil if none), so that a new access unit can be recognised as
// soon as its first start code arrives rather than when it is complete.
func (s *startCodeScanner) pending() []byte {
	if !s.inUnit {
		return nil
	}
	return s.buf[3:]
}

// push appends data and returns the complete units (without the prefix,
// trailing zero bytes removed, except the first). Bytes before the first start code are
// discarded.
func (s *startCodeScanner) push(data []byte) [][]byte {
	s.buf = append(s.buf, data...)
	first := bytes.Index(s.buf, []byte{0, 0, 1})
	if first < 0 {
		if len(s.buf) > 2 {
			s.buf = append(s.buf[:0], s.buf[len(s.buf)-2:]...)
		}
		s.scanFrom = 0
		s.inUnit = false
		return nil
	}
	s.inUnit = true
	var units [][]byte
	start := first
	from := max(s.scanFrom, start+3)
	for {
		i := bytes.Index(s.buf[from:], []byte{0, 0, 1})
		if i < 0 {
			break
		}
		next := from + i
		// Trailing zeros are stuffing or the first byte of a 4-byte start
		// code; the first byte (start code value / NAL header) always stays,
		// since MPEG-4's video_object_start_code is 00 00 01 00.
		u := s.buf[start+3 : next]
		if len(u) > 0 {
			n := 1 + len(bytes.TrimRight(u[1:], "\x00"))
			units = append(units, bytes.Clone(u[:n]))
		}
		start = next
		from = next + 3
	}
	rest := len(s.buf) - start
	copy(s.buf, s.buf[start:])
	s.buf = s.buf[:rest]
	// Resume the search where it stopped, allowing a start code that
	// straddles the next push.
	s.scanFrom = max(3, rest-2)
	return units
}

// auSplitter turns an encoder's output into access units.
type auSplitter interface {
	push(data []byte) []accessUnit
}

func newSplitter(codec string) auSplitter {
	if codec == rtspCodecH264 {
		return &h264Splitter{}
	}
	return &mpeg4Splitter{}
}

// mpeg4Splitter groups MPEG-4 Part 2 start-code units into frames: the
// headers (VOS, VO, VOL, user data, GOV) and the VOP that follows them.
type mpeg4Splitter struct {
	sc     startCodeScanner
	cur    [][]byte
	hasVOP bool
	config []byte
	plid   int
}

func (m *mpeg4Splitter) push(data []byte) []accessUnit {
	var out []accessUnit
	for _, u := range m.sc.push(data) {
		if m.startsAU(u) {
			out = append(out, m.finish())
		}
		m.cur = append(m.cur, u)
		if u[0] == 0xB6 {
			m.hasVOP = true
		}
	}
	if p := m.sc.pending(); len(p) > 0 && m.startsAU(p) {
		out = append(out, m.finish())
	}
	return out
}

// startsAU reports whether a unit beginning with u ends the current frame.
func (m *mpeg4Splitter) startsAU(u []byte) bool {
	code := u[0]
	return m.hasVOP && (code == 0xB6 || code <= 0x2F || code == 0xB0 || code == 0xB3 || code == 0xB5)
}

func (m *mpeg4Splitter) finish() accessUnit {
	au := accessUnit{plid: 1}
	var config []byte
	headers := true
	hasVOL := false
	for _, u := range m.cur {
		withPrefix := append([]byte{0, 0, 1}, u...)
		au.units = append(au.units, withPrefix)
		switch {
		case u[0] == 0xB6:
			au.key = len(u) > 1 && u[1]>>6 == 0 // vop_coding_type I
			headers = false
		case u[0] == 0xB3:
			headers = false
		case headers:
			config = append(config, withPrefix...)
			if u[0] >= 0x20 && u[0] <= 0x2F {
				hasVOL = true
			}
			if u[0] == 0xB0 && len(u) > 1 {
				m.plid = int(u[1])
			}
		}
	}
	if hasVOL {
		m.config = config
		au.inband = true
	}
	if m.config != nil {
		au.params = [][]byte{m.config}
	}
	if m.plid != 0 {
		au.plid = m.plid
	}
	m.cur, m.hasVOP = nil, false
	return au
}

// h264Splitter groups H.264 NAL units into access units (ITU-T H.264
// 7.4.1.2.3: an AUD, SPS, PPS or SEI, or a slice with first_mb_in_slice 0,
// after a slice starts a new access unit). AUDs are dropped.
type h264Splitter struct {
	sc       startCodeScanner
	cur      [][]byte
	hasVCL   bool
	sps, pps []byte
}

func (s *h264Splitter) push(data []byte) []accessUnit {
	var out []accessUnit
	for _, n := range s.sc.push(data) {
		if s.startsAU(n) {
			out = append(out, s.finish())
		}
		typ := n[0] & 0x1F
		if typ == 9 {
			continue
		}
		s.cur = append(s.cur, n)
		if typ == 1 || typ == 5 {
			s.hasVCL = true
		}
	}
	if p := s.sc.pending(); len(p) >= 2 && s.startsAU(p) {
		out = append(out, s.finish())
	}
	return out
}

// startsAU reports whether a NAL unit beginning with n ends the current
// access unit (n needs 2 bytes for slices).
func (s *h264Splitter) startsAU(n []byte) bool {
	if !s.hasVCL {
		return false
	}
	switch n[0] & 0x1F {
	case 6, 7, 8, 9:
		return true
	case 1, 5:
		return len(n) > 1 && n[1]&0x80 != 0
	}
	return false
}

func (s *h264Splitter) finish() accessUnit {
	var au accessUnit
	hasSPS, hasPPS := false, false
	for _, n := range s.cur {
		switch n[0] & 0x1F {
		case 5:
			au.key = true
		case 7:
			s.sps, hasSPS = n, true
		case 8:
			s.pps, hasPPS = n, true
		}
	}
	au.units = s.cur
	au.inband = hasSPS && hasPPS
	if s.sps != nil && s.pps != nil {
		au.params = [][]byte{s.sps, s.pps}
	}
	s.cur, s.hasVCL = nil, false
	return au
}

// rtspOutput is the stream readers see, for one codec and one set of
// codec parameters.
type rtspOutput struct {
	codec    string
	key      string
	params   [][]byte
	stream   *gortsplib.ServerStream
	media    *description.Media
	mpeg4    *rtpfragmented.Encoder
	h264     *rtph264.Encoder
	t0       time.Time
	tsOffset uint32
}

func paramsKey(codec string, params [][]byte) string {
	var b strings.Builder
	b.WriteString(codec)
	for _, p := range params {
		b.WriteByte(':')
		b.WriteString(hex.EncodeToString(p))
	}
	return b.String()
}

func newRTSPOutput(server *gortsplib.Server, codec string, au accessUnit) (*rtspOutput, error) {
	o := &rtspOutput{codec: codec, key: paramsKey(codec, au.params), params: au.params, t0: time.Now()}
	var forma format.Format
	switch codec {
	case rtspCodecMPEG4:
		f := &format.MPEG4Video{PayloadTyp: 96, ProfileLevelID: au.plid, Config: au.params[0]}
		enc, err := f.CreateEncoder()
		if err != nil {
			return nil, err
		}
		forma, o.mpeg4 = f, enc
	default:
		f := &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: au.params[0], PPS: au.params[1]}
		enc, err := f.CreateEncoder()
		if err != nil {
			return nil, err
		}
		forma, o.h264 = f, enc
	}
	o.media = &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{forma}}
	o.stream = &gortsplib.ServerStream{
		Server: server,
		Desc:   &description.Session{Title: "snoopy", Medias: []*description.Media{o.media}},
	}
	if err := o.stream.Initialize(); err != nil {
		return nil, err
	}
	var r [4]byte
	_, _ = rand.Read(r[:])
	o.tsOffset = binary.BigEndian.Uint32(r[:])
	return o, nil
}

// write packetises au and sends it to every reader. Keyframes always carry
// the parameter sets in band for decoders that ignore the SDP.
func (o *rtspOutput) write(au accessUnit) error {
	units := au.units
	if au.key && !au.inband {
		units = append(append([][]byte{}, o.params...), units...)
	}
	var pkts []*rtp.Packet
	var err error
	if o.mpeg4 != nil {
		pkts, err = o.mpeg4.Encode(bytes.Join(units, nil))
	} else {
		pkts, err = o.h264.Encode(units)
	}
	if err != nil {
		return err
	}
	now := time.Now()
	ts := o.tsOffset + uint32(now.Sub(o.t0)*90000/time.Second)
	for _, p := range pkts {
		p.Timestamp = ts
		if err := o.stream.WritePacketRTPWithNTP(o.media, p, now); err != nil {
			return err
		}
	}
	return nil
}

// rtspStreamer is the RTSP server plus its supervised encoder.
type rtspStreamer struct {
	cfg        rtspConfig
	ffmpeg     string
	candidates []encoderChoice
	server     *gortsplib.Server
	frames     <-chan []byte
	unsub      func()

	minBackoff, maxBackoff time.Duration

	mu  sync.Mutex
	out *rtspOutput
	pid int

	latest  []byte
	latestW int // source size of latest
	latestH int

	cancel context.CancelFunc
	done   chan struct{}
}

// startRTSPFromCache starts the RTSP stream with the image cache's
// "waiting" placeholder as the first frame and logs how it can be reached.
func startRTSPFromCache(ctx context.Context, cfg rtspConfig, hub *frameHub, cache *ImageCache) (*rtspStreamer, error) {
	placeholder, err := os.ReadFile(cache.waitingImg)
	if err != nil {
		return nil, fmt.Errorf("read placeholder: %w", err)
	}
	s, err := startRTSP(ctx, cfg, hub, placeholder)
	if err != nil {
		return nil, err
	}
	log.Printf("RTSP: serving the screen on rtsp://%s/ (any path, TCP transport, %s)",
		net.JoinHostPort(cfg.addr, strconv.Itoa(cfg.port)), s.candidates[0])
	switch {
	case cfg.user != "":
		log.Printf("RTSP: readers must authenticate as %q (digest)", cfg.user)
	case shouldAdvertise(cfg.addr):
		log.Printf("WARNING: RTSP has no credentials: anyone on the network who can reach port %d can watch this screen (set %s and %s)",
			cfg.port, envName("rtsp-user"), envName("rtsp-password"))
	default:
		log.Printf("RTSP: no credentials; bound to loopback, so only local users can watch")
	}
	return s, nil
}

// startRTSP probes ffmpeg, starts the RTSP server and the encoder
// supervisor. placeholder is streamed until hub delivers the first frame.
// The caller must Close the result.
func startRTSP(ctx context.Context, cfg rtspConfig, hub *frameHub, placeholder []byte) (*rtspStreamer, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.New("ffmpeg not found in PATH")
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	out, err := exec.CommandContext(probeCtx, ffmpeg, "-hide_banner", "-encoders").Output()
	cancelProbe()
	if err != nil {
		return nil, fmt.Errorf("list ffmpeg encoders: %w", err)
	}
	cands, err := encoderCandidates(cfg.codec, parseEncoders(string(out)))
	if err != nil {
		return nil, err
	}
	return newRTSPStreamer(ctx, cfg, ffmpeg, cands, hub, placeholder)
}

func newRTSPStreamer(ctx context.Context, cfg rtspConfig, ffmpeg string, cands []encoderChoice, hub *frameHub, placeholder []byte) (*rtspStreamer, error) {
	w, h, err := jpegSize(placeholder)
	if err != nil {
		return nil, fmt.Errorf("placeholder image: %w", err)
	}
	s := &rtspStreamer{
		cfg:        cfg,
		ffmpeg:     ffmpeg,
		candidates: cands,
		minBackoff: time.Second,
		maxBackoff: 30 * time.Second,
		latest:     placeholder,
		latestW:    w,
		latestH:    h,
		done:       make(chan struct{}),
	}
	s.server = &gortsplib.Server{
		Handler:        s,
		RTSPAddress:    net.JoinHostPort(cfg.addr, strconv.Itoa(cfg.port)),
		WriteQueueSize: 1024,
		// Digest only, so the password never crosses the network in
		// clear. UDP is not configured: TCP-interleaved transport only.
		AuthMethods: []auth.VerifyMethod{auth.VerifyMethodDigestMD5},
	}
	if err := s.server.Start(); err != nil {
		return nil, fmt.Errorf("RTSP server on %s: %w", s.server.RTSPAddress, err)
	}
	s.frames, s.unsub = hub.Subscribe()
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.supervise(runCtx)
	return s, nil
}

// Close stops the encoder, disconnects readers and closes the server.
func (s *rtspStreamer) Close() {
	s.cancel()
	<-s.done
	s.unsub()
	s.server.Close()
	s.mu.Lock()
	if s.out != nil {
		s.out.stream.Close()
		s.out = nil
	}
	s.mu.Unlock()
}

// encoderPID returns the running encoder's process ID (0 if none).
func (s *rtspStreamer) encoderPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

func jpegSize(b []byte) (int, int, error) {
	cfg, name, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0, err
	}
	if name != "jpeg" || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, fmt.Errorf("not a usable JPEG (%s %dx%d)", name, cfg.Width, cfg.Height)
	}
	return cfg.Width, cfg.Height, nil
}

// takeFrame records a new captured frame. It reports whether the encoded
// size changes, which needs an encoder restart.
func (s *rtspStreamer) takeFrame(b []byte) (resize bool) {
	w, h, err := jpegSize(b)
	if err != nil {
		log.Printf("RTSP: ignoring unreadable frame: %v", err)
		return false
	}
	ow, oh := outputDims(s.latestW, s.latestH, s.cfg.maxHeight)
	nw, nh := outputDims(w, h, s.cfg.maxHeight)
	s.latest, s.latestW, s.latestH = b, w, h
	return ow != nw || oh != nh
}

// supervise keeps an encoder running until ctx ends, restarting it with
// backoff. In auto mode an encoder that exits without producing a frame
// makes way for the next candidate.
func (s *rtspStreamer) supervise(ctx context.Context) {
	defer close(s.done)
	idx := 0
	delay := s.minBackoff
	for {
		select {
		case b := <-s.frames:
			s.takeFrame(b)
		default:
		}
		enc := s.candidates[idx]
		w, h := outputDims(s.latestW, s.latestH, s.cfg.maxHeight)
		log.Printf("RTSP: starting encoder %s at %dx%d", enc, w, h)
		produced, resize, err := s.runEncoder(ctx, enc, w, h)
		if ctx.Err() != nil {
			return
		}
		if resize {
			log.Printf("RTSP: frame size changed, restarting encoder")
			delay = s.minBackoff
			continue
		}
		log.Printf("RTSP: encoder %s stopped: %v; restarting in %s", enc, err, delay)
		if produced {
			delay = s.minBackoff
		} else if len(s.candidates) > 1 {
			idx = (idx + 1) % len(s.candidates)
			log.Printf("RTSP: encoder produced no output, trying %s next", s.candidates[idx])
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if !produced {
			delay = min(delay*2, s.maxBackoff)
		}
	}
}

// tailBuffer keeps the last bytes written to it (ffmpeg's stderr).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2048 {
		t.buf = t.buf[len(t.buf)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// runEncoder runs one ffmpeg process until it exits, ctx ends or the frame
// size changes. It reports whether any access unit was produced.
func (s *rtspStreamer) runEncoder(ctx context.Context, enc encoderChoice, w, h int) (produced, resize bool, err error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.ffmpeg, ffmpegArgs(enc, w, h)...) //nolint:gosec // fixed binary, generated arguments
	cmd.WaitDelay = 3 * time.Second
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, false, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, false, err
	}
	if err := cmd.Start(); err != nil {
		return false, false, err
	}
	s.mu.Lock()
	s.pid = cmd.Process.Pid
	s.mu.Unlock()

	// Writer: never blocks the loop below on a stuck encoder.
	feed := make(chan []byte, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for b := range feed {
			if _, err := stdin.Write(b); err != nil {
				for range feed {
					// drain until closed
				}
				return
			}
		}
		_ = stdin.Close()
	}()

	// Reader: split the elementary stream and publish each frame.
	readDone := make(chan int, 1)
	go func() {
		readDone <- s.pump(stdout, enc.codec)
	}()

	send := func() {
		select {
		case feed <- s.latest:
		default: // encoder busy; the next tick sends the newest frame
		}
	}
	send()
	ticker := time.NewTicker(rtspFrameInterval)
	defer ticker.Stop()

	count := 0
loop:
	for {
		select {
		case <-runCtx.Done():
			// ctx ended: CommandContext kills the process, stdout hits EOF.
			count = <-readDone
			break loop
		case n := <-readDone:
			count = n
			break loop
		case b := <-s.frames:
			if s.takeFrame(b) {
				resize = true
				cancel()
			}
		case <-ticker.C:
			send()
		}
	}
	close(feed)
	waitErr := cmd.Wait()
	wg.Wait()
	s.mu.Lock()
	s.pid = 0
	s.mu.Unlock()
	if waitErr == nil {
		waitErr = errors.New("exited")
	}
	if tail := stderr.String(); tail != "" {
		waitErr = fmt.Errorf("%w: %s", waitErr, lastLine(tail))
	}
	return count > 0, resize, waitErr
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// pump reads the encoder output until EOF and publishes every access unit.
// It returns how many were published.
func (s *rtspStreamer) pump(r io.Reader, codec string) int {
	sp := newSplitter(codec)
	buf := make([]byte, 64*1024)
	n := 0
	for {
		k, err := r.Read(buf)
		if k > 0 {
			for _, au := range sp.push(buf[:k]) {
				if s.publish(codec, au) {
					n++
				}
			}
		}
		if err != nil {
			return n
		}
	}
}

// publish sends au to readers, creating the stream on the first keyframe
// and replacing it (disconnecting readers) only if the codec parameters
// change, e.g. after a resolution change.
func (s *rtspStreamer) publish(codec string, au accessUnit) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if au.key && au.params != nil && (s.out == nil || s.out.key != paramsKey(codec, au.params)) {
		if s.out != nil {
			log.Printf("RTSP: stream parameters changed; readers must reconnect")
			s.out.stream.Close()
			s.out = nil
		}
		out, err := newRTSPOutput(s.server, codec, au)
		if err != nil {
			log.Printf("RTSP: cannot create stream: %v", err)
			return false
		}
		s.out = out
	}
	if s.out == nil || s.out.codec != codec {
		return false // wait for a keyframe with parameters
	}
	if err := s.out.write(au); err != nil {
		log.Printf("RTSP: write: %v", err)
		return false
	}
	return true
}

func (s *rtspStreamer) currentStream() *gortsplib.ServerStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		return nil
	}
	return s.out.stream
}

// authorized checks reader credentials when they are configured. Digest
// responses cover the URL the client sent, so verification uses the
// original URL saved by OnRequest.
func (s *rtspStreamer) authorized(conn *gortsplib.ServerConn, req *base.Request) bool {
	if s.cfg.user == "" {
		return true
	}
	r := *req
	if orig, ok := req.Header[rtspOrigURLHeader]; ok && len(orig) == 1 {
		if u, err := base.ParseURL(orig[0]); err == nil {
			r.URL = u
		}
	}
	return conn.VerifyCredentials(&r, s.cfg.user, s.cfg.password)
}

// rtspStreamPath is the canonical path every request is mapped to.
const rtspStreamPath = "/screen"

// OnRequest maps every request URL onto one canonical path (keeping a
// SETUP's /trackID=N), so the stream is served at any path, including the
// empty one: VLC's mDNS discovery on Windows, macOS and Android opens the
// bare rtsp://host:port, while its Avahi module on Linux appends TXT
// path=/. Without this, gortsplib sees rtsp://host:port (path "") and the
// Content-Base it returns, rtsp://host:port/ (path "/"), as different
// paths and refuses the PLAY. gortsplib calls OnRequest before handling
// the request; TestRTSPIntegration covers both forms.
func (s *rtspStreamer) OnRequest(_ *gortsplib.ServerConn, req *base.Request) {
	if req.URL == nil {
		return
	}
	if req.Header == nil {
		req.Header = base.Header{}
	}
	req.Header[rtspOrigURLHeader] = base.HeaderValue{req.URL.String()}
	req.URL = canonicalRTSPURL(req.URL)
}

// rtspOrigURLHeader carries a request's original URL from OnRequest to the
// digest check; it is set (overwritten) by snoopy on every request.
const rtspOrigURLHeader = "X-Snoopy-Original-Url"

func canonicalRTSPURL(u *base.URL) *base.URL {
	c := u.Clone()
	track := ""
	for _, part := range []string{u.RawQuery, u.Path} {
		if i := strings.LastIndex(part, "/trackID="); i >= 0 {
			track = part[i:]
			break
		}
	}
	c.Path = rtspStreamPath + track
	c.RawPath = ""
	c.RawQuery = ""
	return c
}

// OnDescribe serves the stream (OnRequest has made the path irrelevant).
func (s *rtspStreamer) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !s.authorized(ctx.Conn, ctx.Request) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	st := s.currentStream()
	if st == nil {
		return &base.Response{StatusCode: base.StatusServiceUnavailable}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnAnnounce refuses every publisher: the stream comes from snoopy's own
// encoder, never from the network.
func (s *rtspStreamer) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	log.Printf("RTSP: refused publish attempt from %s", ctx.Conn.NetConn().RemoteAddr())
	return &base.Response{StatusCode: base.StatusForbidden}, errors.New("publishing is not allowed")
}

// OnSetup hands readers the current stream.
func (s *rtspStreamer) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if ctx.Session.State() == gortsplib.ServerSessionStatePreRecord {
		return &base.Response{StatusCode: base.StatusForbidden}, nil, errors.New("publishing is not allowed")
	}
	if !s.authorized(ctx.Conn, ctx.Request) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	st := s.currentStream()
	if st == nil {
		return &base.Response{StatusCode: base.StatusServiceUnavailable}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnPlay logs who started watching (the screen is sensitive).
func (s *rtspStreamer) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	log.Printf("RTSP: %s started watching", ctx.Conn.NetConn().RemoteAddr())
	return &base.Response{StatusCode: base.StatusOK}, nil
}
