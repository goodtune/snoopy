// Package hubtest provides a fake snoopy desktop for the hub's tests: an
// httptest server implementing snoopy's HTTP API (/sse/image, /images/*).
package hubtest

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// Fake is a fake snoopy desktop.
type Fake struct {
	Server *httptest.Server

	mu       sync.Mutex
	images   map[string][]byte
	latest   string
	streams  map[chan string]chan struct{} // event channel -> drop signal
	connects int
	requests []string
}

// WaitingJPEG is what the fake serves as /images/waiting.jpg.
var WaitingJPEG = []byte("\xff\xd8waiting\xff\xd9")

// NewFake starts a fake desktop.
func NewFake() *Fake {
	f := &Fake{images: map[string][]byte{}, streams: map[chan string]chan struct{}{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Close stops the server.
func (f *Fake) Close() {
	f.DropStreams()
	f.Server.Close()
}

// HostPort returns the server's host and port.
func (f *Fake) HostPort() (string, int) {
	h, p, _ := net.SplitHostPort(f.Server.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n
}

// Publish stores an image and announces it to every open stream.
func (f *Fake) Publish(file string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[file] = data
	f.latest = file
	for ch := range f.streams {
		select {
		case ch <- "/images/" + file:
		default:
		}
	}
}

// Send sends raw event data to every open stream.
func (f *Fake) Send(data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.streams {
		ch <- data
	}
}

// DropStreams ends every open SSE response.
func (f *Fake) DropStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch, drop := range f.streams {
		close(drop)
		delete(f.streams, ch)
	}
}

// Connects is the number of SSE connections made so far.
func (f *Fake) Connects() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connects
}

// OpenStreams is the number of SSE connections currently open.
func (f *Fake) OpenStreams() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

// Requests returns the paths requested so far.
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// CountRequests counts requests for path.
func (f *Fake) CountRequests(path string) int {
	n := 0
	for _, p := range f.Requests() {
		if p == path {
			n++
		}
	}
	return n
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/":
		_, _ = w.Write([]byte("<h1>fake snoopy</h1>"))
	case r.URL.Path == "/sse/image":
		f.sse(w, r)
	case r.URL.Path == "/images/waiting.jpg":
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(WaitingJPEG)
	case strings.HasPrefix(r.URL.Path, "/images/"):
		f.mu.Lock()
		b, ok := f.images[strings.TrimPrefix(r.URL.Path, "/images/")]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) sse(w http.ResponseWriter, r *http.Request) {
	ch := make(chan string, 16)
	drop := make(chan struct{})
	f.mu.Lock()
	f.connects++
	f.streams[ch] = drop
	latest := f.latest
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.streams, ch)
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: /images/%s\n\n", latest)
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-drop:
			return
		case m := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", m)
			w.(http.Flusher).Flush()
		}
	}
}
