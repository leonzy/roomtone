package app

import (
	"encoding/binary"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type dlnaRangeWatch struct {
	rate, offset int64
	events       chan time.Duration
}
type dlnaRangeRegistry struct {
	mu      sync.Mutex
	watches map[string]*dlnaRangeWatch
}

func (s *dlnaStreams) watchRanges(query string, rate, offset int64) (<-chan time.Duration, func()) {
	q, _ := url.ParseQuery(query)
	ticket := q.Get("ticket")
	w := &dlnaRangeWatch{rate: rate, offset: offset, events: make(chan time.Duration, 4)}
	s.mu.Lock()
	if s.watches == nil {
		s.watches = make(map[string]*dlnaRangeWatch)
	}
	s.watches[ticket] = w
	s.mu.Unlock()
	return w.events, func() {
		s.mu.Lock()
		if s.watches[ticket] == w {
			delete(s.watches, ticket)
		}
		s.mu.Unlock()
	}
}

func (s *dlnaStreams) observeResponse(w http.ResponseWriter, ticket string) http.ResponseWriter {
	s.mu.Lock()
	watch := s.watches[ticket]
	s.mu.Unlock()
	if watch == nil {
		return w
	}
	return &dlnaRangeResponse{ResponseWriter: w, observe: func(status int) {
		if status != http.StatusPartialContent {
			return
		}
		value := strings.TrimPrefix(w.Header().Get("Content-Range"), "bytes ")
		start, err := strconv.ParseInt(strings.SplitN(value, "-", 2)[0], 10, 64)
		if err != nil || watch.rate <= 0 {
			return
		}
		position := time.Duration(float64(max(int64(0), start-watch.offset)) / float64(watch.rate) * float64(time.Second))
		select {
		case watch.events <- position:
		default:
			select {
			case <-watch.events:
			default:
			}
			select {
			case watch.events <- position:
			default:
			}
		}
	}}
}

type dlnaRangeResponse struct {
	http.ResponseWriter
	status  int
	observe func(int)
}

func (w *dlnaRangeResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *dlnaRangeResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
	w.observe(status)
}
func (w *dlnaRangeResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}
func (w *dlnaRangeResponse) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// PCM WAV byte ranges map exactly to media time, including files with metadata chunks.
func wavByteLayout(path string) (int64, int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var header [12]byte
	if _, err = io.ReadFull(f, header[:]); err != nil || string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" {
		return 0, 0, false
	}
	var rate int64
	for chunks := 0; chunks < 128; chunks++ {
		var chunk [8]byte
		if _, err = io.ReadFull(f, chunk[:]); err != nil {
			return 0, 0, false
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		start, _ := f.Seek(0, io.SeekCurrent)
		if string(chunk[:4]) == "fmt " && size >= 16 {
			var format [16]byte
			if _, err = io.ReadFull(f, format[:]); err != nil {
				return 0, 0, false
			}
			if binary.LittleEndian.Uint16(format[:2]) == 1 {
				rate = int64(binary.LittleEndian.Uint32(format[8:12]))
			}
		}
		if string(chunk[:4]) == "data" {
			return rate, start, rate > 0
		}
		if _, err = f.Seek(start+size+size%2, io.SeekStart); err != nil {
			return 0, 0, false
		}
	}
	return 0, 0, false
}
