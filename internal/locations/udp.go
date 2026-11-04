package locations

import (
	"context"
	"errors"
	"net"
	"sync"
)

// udpWorkers is how many packets can be in hand at once. Each worker holds one
// through its trip to Redis, so more workers means more trips at the same time.
const udpWorkers = 32

// ServeUDP is the UDP door: shouting across the street. It runs until ctx ends.
//
// There's no connection and no login, so each packet stands alone and proves itself
// with its signature. The door never answers. An answer would let someone fake the
// sender's address and aim our replies at a victim, and a phone couldn't do much
// with one anyway, because its next ping is already on the way.
//
// There's no queue either. When every worker is busy, packets wait in the operating
// system's small buffer for this socket. When that fills up, the operating system
// throws new ones away. For positions that's right: a queue would only make us
// spend time on positions that are getting older.
func (s *Service) ServeUDP(ctx context.Context, conn *net.UDPConn) {
	// Closing the socket is the only way to wake up a worker waiting for a packet.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	var wg sync.WaitGroup
	for range udpWorkers {
		wg.Go(func() { s.readPackets(ctx, conn) })
	}
	wg.Wait()
}

func (s *Service) readPackets(ctx context.Context, conn *net.UDPConn) {
	// One byte more than a packet: a longer message gets cut off to this size,
	// so it still can't pass for a real packet.
	buf := make([]byte, PacketSize+1)
	for {
		n, err := conn.Read(buf)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			s.logger.Warn("udp read failed", "err", err)
			continue
		}
		// Refused packets are counted by Handle. Nobody gets told.
		_ = s.Handle(ctx, &s.UDP, buf[:n])
	}
}
