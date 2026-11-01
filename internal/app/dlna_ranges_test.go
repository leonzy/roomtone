package app

import (
	"context"
	"encoding/binary"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDLNARangeObservationIsBoundToActualResponseAndTicket(t *testing.T) {
	s := newDLNAStreams()
	query := s.query(42, "original:wav", time.Minute)
	events, release := s.watchRanges(query, 176400, 44)
	defer release()
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		r := httptest.NewRequest(method, "/stream?"+query, nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", "42")
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, route))
		w := httptest.NewRecorder()
		s.protect(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes 5292044-5292143/10000000")
			w.WriteHeader(206)
		})(w, r)
		if method == http.MethodHead {
			select {
			case <-events:
				t.Fatal("HEAD probe claimed audio delivery")
			default:
			}
			continue
		}
		select {
		case pos := <-events:
			if pos != 30*time.Second {
				t.Fatal(pos)
			}
		default:
			t.Fatal("actual byte range was not observed")
		}
	}
	release()
	s.mu.Lock()
	count := len(s.watches)
	s.mu.Unlock()
	if count != 0 {
		t.Fatal("stream observation leaked after release")
	}
}

func TestWAVByteLayoutAccountsForMetadataChunks(t *testing.T) {
	for _, junk := range []bool{false, true} {
		data := []byte("RIFF\x00\x00\x00\x00WAVE")
		if junk {
			data = append(data, []byte("JUNK\x04\x00\x00\x00abcd")...)
		}
		data = append(data, []byte("fmt \x10\x00\x00\x00")...)
		format := make([]byte, 16)
		binary.LittleEndian.PutUint16(format, 1)
		binary.LittleEndian.PutUint16(format[2:], 2)
		binary.LittleEndian.PutUint32(format[4:], 44100)
		binary.LittleEndian.PutUint32(format[8:], 176400)
		binary.LittleEndian.PutUint16(format[12:], 4)
		binary.LittleEndian.PutUint16(format[14:], 16)
		data = append(data, format...)
		data = append(data, []byte("data\x00\x00\x00\x00")...)
		path := filepath.Join(t.TempDir(), "test.wav")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		rate, offset, ok := wavByteLayout(path)
		if !ok || rate != 176400 || offset != int64(len(data)) {
			t.Fatal(rate, offset, ok)
		}
	}
}
