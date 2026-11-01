package dlna

import (
	"context"
	"fmt"
	"mime"
	"strings"

	"songloft/internal/player/sink"
)

// LoadProtocolInfo records formats explicitly advertised by the renderer.
// Missing services, wildcard replies and broken implementations leave capabilities
// unknown, so stream planning preserves the original rather than forcing conversion.
func LoadProtocolInfo(ctx context.Context, client *Client, dev *Device) {
	service, ok := dev.Services[SvcConnectionMgr]
	if !ok {
		return
	}
	values, err := client.Call(ctx, service, "GetProtocolInfo")
	if err != nil {
		return
	}
	dev.Codecs = ParseSinkCodecs(values["Sink"])
}

func ParseSinkCodecs(protocols string) []sink.Codec {
	var codecs []sink.Codec
	seen := map[sink.Codec]bool{}
	for _, protocol := range strings.Split(protocols, ",") {
		fields := strings.SplitN(strings.TrimSpace(protocol), ":", 4)
		if len(fields) != 4 || !strings.EqualFold(fields[0], "http-get") {
			continue
		}
		var codec sink.Codec
		switch strings.ToLower(strings.TrimSpace(strings.SplitN(fields[2], ";", 2)[0])) {
		case "audio/flac", "audio/x-flac":
			codec = sink.CodecFLAC
		case "audio/wav", "audio/x-wav", "audio/wave":
			codec = sink.CodecWAV
		case "audio/mp4", "audio/x-m4a":
			codec = sink.Codec("m4a")
		case "audio/aac":
			codec = sink.Codec("adts")
		case "audio/l16":
			codec = sink.CodecL16
			_, params, err := mime.ParseMediaType(fields[2])
			if err != nil || (params["rate"] != "" && params["rate"] != "44100") || (params["channels"] != "" && params["channels"] != "2") {
				codec = sink.Codec(fields[2]) // do not promise a rate/channel layout we cannot produce
			}
		case "audio/ogg", "application/ogg", "audio/vorbis":
			codec = sink.Codec("ogg")
		case "audio/aiff", "audio/x-aiff":
			codec = sink.Codec("aiff")
		case "audio/mpeg", "audio/mp3":
			codec = sink.CodecMP3
		case "*":
			return nil
		default:
			codec = sink.Codec(strings.ToLower(strings.TrimSpace(fields[2])))
		}
		if codec != "" && !seen[codec] {
			codecs = append(codecs, codec)
			seen[codec] = true
		}
	}
	return codecs
}

// ContentFeatures describes what our HTTP representation actually supports.
// We support byte ranges on complete files, not DLNA time-range requests.
func ContentFeatures(converted, live bool) string {
	op, ci := "01", 0
	if live {
		op = "00"
	}
	if converted {
		ci = 1
	}
	return fmt.Sprintf("DLNA.ORG_OP=%s;DLNA.ORG_CI=%d;DLNA.ORG_FLAGS=01500000000000000000000000000000", op, ci)
}

// SelectCodec preserves supported originals, then prefers lossless output.
// Unknown capabilities preserve the original format.
// An explicit list containing only unknown formats must not silently imply MP3.
func SelectCodec(source string, accepted []sink.Codec) sink.Codec {
	if len(accepted) == 0 {
		return sink.Codec(strings.ToLower(source))
	}
	for _, codec := range accepted {
		if string(codec) == strings.ToLower(source) {
			return codec
		}
	}
	for _, preferred := range []sink.Codec{sink.CodecFLAC, sink.CodecWAV, sink.CodecL16, sink.CodecMP3, sink.Codec("m4a"), sink.Codec("adts"), sink.Codec("ogg")} {
		for _, codec := range accepted {
			if codec == preferred {
				return codec
			}
		}
	}
	return ""
}
