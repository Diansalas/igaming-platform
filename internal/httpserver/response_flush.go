package httpserver

import "net/http"

// flushResponse pushes the already-written response to the client NOW, before
// the handler goes on to post-response alert work (ADR 0102 7.3/17.2). net/http
// buffers a small response until the handler returns, so without this a raise
// or Pending.Flush placed "after the response" still finishes before the
// provider or admin receives a byte. The error is ignored on purpose: a writer
// that cannot flush (ErrNotSupported) or a client that already went away must
// never affect the handler's outcome. Wrapping writers must expose Unwrap
// (statusRecorder does) or the flush silently becomes a no-op.
func flushResponse(w http.ResponseWriter) {
	_ = http.NewResponseController(w).Flush()
}
