package httpserver

import (
	"encoding/json"
	"net"
	"net/http"
)

const maxRequestBodyBytes = 1 << 20 // 1 MiB - generous for any Stage 2 JSON payload, bounds a malicious oversized body

func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clientIP extracts the caller's IP from RemoteAddr, stripping the port.
// Stage 2 does not trust X-Forwarded-For (that requires knowing and
// validating the specific proxy chain in front of the service, which
// varies per deployment and is not yet configured) - RemoteAddr is
// whatever actually opened the TCP connection, which is correct for a
// direct connection and becomes the load balancer's own address once one
// is introduced. Revisit when a specific reverse-proxy setup exists to
// trust.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
