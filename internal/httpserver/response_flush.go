package httpserver

import "net/http"

// flushResponse pushes status line, headers and the body bytes written so far to
// the client NOW. It is used ONLY inline, immediately after a response was
// written on a path that cannot be a panic unwind (never from a defer: on a
// panic before any write, Flush would commit an implicit 200 and the recover
// middleware's 500 would be dropped as a superfluous WriteHeader, which makes a
// payment provider stop redelivering; the I-wire final review F1). It does NOT
// complete the response: without a Content-Length the chunked body's terminating
// chunk (body EOF, connection reuse, a buffering proxy) is still sent only when
// the handler returns, so the alert work after it delays those. The error is
// ignored on purpose: a writer that cannot flush or a client that went away must
// never affect the handler's outcome. Wrapping writers must expose Unwrap
// (statusRecorder does) or the flush silently becomes a no-op.
func flushResponse(w http.ResponseWriter) {
	_ = http.NewResponseController(w).Flush()
}
