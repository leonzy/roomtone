package dlna

import (
	"reflect"
	"songloft/internal/player/sink"
	"testing"
)

func TestRendererCodecSelection(t *testing.T) {
	for _, tc := range []struct {
		protocols, source string
		want              sink.Codec
	}{
		{"http-get:*:audio/mpeg:*", "flac", sink.CodecMP3},
		{"http-get:*:audio/x-flac:*,http-get:*:audio/mpeg:*", "mp3", sink.CodecMP3},
		{"http-get:*:audio/x-flac:*,http-get:*:audio/mpeg:*", "dsf", sink.CodecFLAC},
		{"http-get:*:*:*", "wav", sink.CodecWAV},
		{"http-get:*:audio/unsupported:*", "mp3", ""},
		{"", "ape", sink.Codec("ape")},
		{"http-get:*:*:*", "m4a", sink.Codec("m4a")},
		{"http-get:*:audio/mp4:*", "m4a", sink.Codec("m4a")},
		{"http-get:*:audio/L16;rate=44100;channels=2:*", "flac", sink.CodecL16},
		{"http-get:*:audio/aac:*", "flac", sink.Codec("adts")},
		{"http-get:*:audio/aac:*", "adts", sink.Codec("adts")},
	} {
		if got := SelectCodec(tc.source, ParseSinkCodecs(tc.protocols)); got != tc.want {
			t.Errorf("%q source %s: got %s want %s", tc.protocols, tc.source, got, tc.want)
		}
	}
	got := ParseSinkCodecs("http-get:*:audio/mpeg:*,http-get:*:audio/mp3:*,rtsp-rtp-udp:*:audio/flac:*")
	if !reflect.DeepEqual(got, []sink.Codec{sink.CodecMP3}) {
		t.Fatal(got)
	}
}
