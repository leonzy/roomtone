package airplay

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestALACRoundTripFFmpeg(t *testing.T) {
	pcm := make([]byte, 352*4)
	for i := 0; i < len(pcm)/2; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(i*89-28000)))
	}
	payload := EncodeALAC(pcm, 2)
	assertDecodedALAC(t, [][]byte{payload}, pcm)
}

func assertDecodedALAC(t *testing.T, packets [][]byte, pcm []byte) {
	t.Helper()
	ffmpeg := os.Getenv("TEST_FFMPEG")
	if ffmpeg == "" {
		t.Skip("TEST_FFMPEG not configured")
	}
	var caf bytes.Buffer
	caf.Write([]byte{'c', 'a', 'f', 'f', 0, 1, 0, 0})
	chunk := func(name string, data []byte) {
		caf.WriteString(name)
		_ = binary.Write(&caf, binary.BigEndian, int64(len(data)))
		caf.Write(data)
	}
	desc, _ := hex.DecodeString("40e5888000000000616c61630000000000000000000001600000000200000000")
	cookie, _ := hex.DecodeString("0000000c66726d61616c616300000024616c616300000000000001600010280a0e02000000004004001588800000ac44")
	chunk("desc", desc)
	chunk("kuki", cookie)
	data := make([]byte, 4)
	for _, packet := range packets {
		data = append(data, packet...)
	}
	chunk("data", data)
	pakt := make([]byte, 24)
	binary.BigEndian.PutUint64(pakt, uint64(len(packets)))
	binary.BigEndian.PutUint64(pakt[8:], uint64(len(pcm)/4))
	for _, packet := range packets {
		pakt = append(pakt, byte(len(packet)>>7)|0x80, byte(len(packet)&127))
	}
	chunk("pakt", pakt)
	path := filepath.Join(t.TempDir(), "frame.caf")
	if err := os.WriteFile(path, caf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	decoded, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if !bytes.Equal(decoded, pcm) {
		t.Fatalf("decoded PCM differs: got %d bytes, want %d", len(decoded), len(pcm))
	}
}
