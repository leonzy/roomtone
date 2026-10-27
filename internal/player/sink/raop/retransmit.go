package raop

import (
	"encoding/binary"
	"net"
)

const frameSamples = 352
const packetBacklogSize = 1024

type cachedPacket struct {
	seq  uint16
	data []byte
}

// Receivers request missing sequence numbers on the sender's control port.
// Cache complete encrypted RTP packets: re-encrypting or advancing sequence/
// sample counters would create a different stream instead of repairing a loss.
func (s *Sink) startControlReader(conn *net.UDPConn, receiver net.IP) {
	go func() {
		buf := make([]byte, 1500)
		for {
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n != 8 || !peer.IP.Equal(receiver) || buf[0]>>6 != 2 || buf[1]&0x7f != 0x55 {
				continue
			}
			first, count := binary.BigEndian.Uint16(buf[4:6]), int(binary.BigEndian.Uint16(buf[6:8]))
			if count == 0 || count > packetBacklogSize {
				continue
			}
			s.mu.Lock()
			if s.control != conn {
				s.mu.Unlock()
				return
			}
			for i := 0; i < count; i++ {
				seq := first + uint16(i)
				cached := s.backlog[int(seq)%packetBacklogSize]
				if len(cached.data) == 0 || cached.seq != seq {
					s.resendMisses++
					continue
				}
				response := make([]byte, 4+len(cached.data))
				response[0], response[1] = 0x80, 0xd6
				binary.BigEndian.PutUint16(response[2:4], seq)
				copy(response[4:], cached.data)
				if _, err := conn.WriteToUDP(response, peer); err == nil {
					s.resent++
				}
			}
			s.mu.Unlock()
		}
	}()
}
