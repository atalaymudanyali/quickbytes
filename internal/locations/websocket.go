package locations

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"

	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
)

// Why the service hung up, in the close message. Unlike UDP, this door can tell a
// phone what's wrong, because a phone it's on a call with is really there. 4000 and
// up are the numbers WebSocket leaves for each application to define.
const (
	// CloseKeyExpired: start the shift again to get a fresh key.
	CloseKeyExpired websocket.StatusCode = 4401
	// CloseBadSignature: this key isn't right, so trying again won't help.
	CloseBadSignature websocket.StatusCode = 4403
)

// handleWebSocket is the WebSocket door: a phone call that stays open. It carries the
// same signed packets as the UDP door, so each ping still proves itself and the
// call itself needs no login.
//
// The door only listens. If nothing arrives for wsSilentAfter it hangs up, which is
// what cleans up after a courier riding into a tunnel. A dead line gives no goodbye,
// so without this the call would stay open, holding memory, until TCP's own checks
// gave up, which takes minutes. A phone that's still connected but stopped sending
// would keep its call open for good.
//
// A ping waits its turn on this call if Redis is busy, and the phone's next one waits
// behind it. That's how TCP works: everything arrives, in order, and a slow reader
// slows the sender down. UDP in the same spot loses the packet instead.
//
// On shutdown the calls are simply dropped. A phone has to reconnect after any lost
// signal anyway, so it needs no special goodbye for this one.
func (s *Service) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has already answered with what's wrong
	}
	defer conn.CloseNow()
	conn.SetReadLimit(PacketSize)

	s.Connections.Add(1)
	defer s.Connections.Add(-1)

	for {
		ctx, cancel := context.WithTimeout(r.Context(), s.wsSilentAfter)
		_, packet, err := conn.Read(ctx)
		timedOut := ctx.Err() != nil && r.Context().Err() == nil
		cancel()
		switch {
		case timedOut:
			s.SilentHangups.Add(1)
			return
		case errors.Is(err, websocket.ErrMessageTooBig):
			s.WebSocket.Received.Add(1)
			s.WebSocket.Malformed.Add(1)
			return
		case err != nil:
			return // the phone hung up, or the service is shutting down
		}

		if err := s.Handle(r.Context(), &s.WebSocket, packet); err != nil {
			_ = conn.Close(closeCodeFor(err), err.Error())
			return
		}
	}
}

func closeCodeFor(err error) websocket.StatusCode {
	switch {
	case errors.Is(err, shiftkey.ErrExpired):
		return CloseKeyExpired
	case errors.Is(err, shiftkey.ErrBadSignature):
		return CloseBadSignature
	default:
		return websocket.StatusPolicyViolation
	}
}
