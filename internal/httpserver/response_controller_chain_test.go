package httpserver

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// I-wire final review F-3: before statusRecorder.Unwrap, http.NewResponseController
// behind the access-log wrapper returned ErrNotSupported for SetReadDeadline and
// Flush, so ADR 0097 A5 (armBodyReadDeadline) was a silent no-op in production.
// This drives the real access-log + recover chain over a real connection: both
// controller calls must succeed, and a read deadline set through the chain must
// really cut off a body that never arrives.
func TestStatusRecorder_ResponseControllerReachesTheConnectionThroughTheChain(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	type result struct {
		flushErr, deadlineErr, readErr error
	}
	res := make(chan result, 1)
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out result
		rc := http.NewResponseController(w)
		out.deadlineErr = rc.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		_, out.readErr = io.ReadAll(r.Body) // the client never sends the promised body
		out.flushErr = rc.Flush()
		res <- out
	}), loggingMiddleware(logger), recoverMiddleware(logger))
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-res:
		if out.deadlineErr != nil {
			t.Fatalf("SetReadDeadline must reach the connection through the access-log wrapper, got %v", out.deadlineErr)
		}
		if out.flushErr != nil {
			t.Fatalf("Flush must reach the connection through the access-log wrapper, got %v", out.flushErr)
		}
		if out.readErr == nil || !strings.Contains(out.readErr.Error(), "timeout") {
			t.Fatalf("the read deadline must cut off the missing body with a timeout, got %v", out.readErr)
		}
	case <-time.After(30 * time.Second): // failure guard only
		t.Fatal("the read deadline never fired: it did not reach the connection")
	}
}
