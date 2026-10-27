package raop

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestLegacyRSAAdvertisedWithoutPK(t *testing.T) {
	found, ok := EntryToFound(Entry{Name: "00226C309EC4@Speaker._raop._tcp.local.", Addr: net.ParseIP("192.0.2.1"), Port: 4515, TXT: []string{"et=0,1", "cn=1"}})
	if !ok {
		t.Fatal("device not found")
	}
	key, err := ParsePublicKey(found.Caps.PublicKey)
	if err != nil || key == nil || key.N.BitLen() != 2048 || key.E != 65537 {
		t.Fatalf("legacy RSA key missing: %v", err)
	}
	clear, _ := EntryToFound(Entry{Name: "00226C309EC4@Speaker._raop._tcp.local.", Addr: net.ParseIP("192.0.2.1"), Port: 4515, TXT: []string{"et=0"}})
	if clear.Caps.PublicKey != "" {
		t.Fatal("forced RSA on clear-only device")
	}
}

func TestEncryptedPacketRetainsLengthAndTail(t *testing.T) {
	key, iv := bytes.Repeat([]byte{0x31}, 16), bytes.Repeat([]byte{0x72}, 16)
	for _, size := range []int{0, 3, 16, 31, 1415} {
		pcm := bytes.Repeat([]byte{0xa5}, size)
		enc, err := EncryptFrames(key, iv, pcm)
		if err != nil || len(enc) != size {
			t.Fatalf("size %d: len=%d err=%v", size, len(enc), err)
		}
		n := size / 16 * 16
		if !bytes.Equal(enc[n:], pcm[n:]) {
			t.Fatal("partial AES block encrypted")
		}
		if n > 0 && bytes.Equal(enc[:n], pcm[:n]) {
			t.Fatal("full AES blocks left clear")
		}
		dec, err := DecryptFrames(key, iv, enc)
		if err != nil || !bytes.Equal(dec, pcm) {
			t.Fatalf("round trip size %d: %v", size, err)
		}
	}
}

func TestRTSPRejectsUnconfirmedSuccess(t *testing.T) {
	for _, response := range []string{"RTSP/1.0 200 OK\r\n\r\n", "RTSP/1.0 200 OK\r\nCSeq: 7\r\n\r\n"} {
		client, server := net.Pipe()
		c := &Client{conn: client, br: bufio.NewReader(client), timeout: time.Second}
		go func() {
			defer server.Close()
			buf := make([]byte, 4096)
			_, _ = server.Read(buf)
			_, _ = server.Write([]byte(response))
		}()
		_, err := c.Do(context.Background(), "ANNOUNCE", "rtsp://127.0.0.1/123", nil, nil)
		client.Close()
		if err == nil {
			t.Fatal("accepted unconfirmed 200 response")
		}
	}
}
