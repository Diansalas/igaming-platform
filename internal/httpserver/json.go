package httpserver

import (
	"encoding/json"
	"errors"
	"io"
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

var errTrailingJSON = errors.New("trailing content after the JSON value")

// decodeSingleValue decodes exactly ONE JSON value from dec into dst and refuses anything after it (a second value or garbage;
// trailing whitespace is fine). It is the shared core of decodeJSONStrict (the handler's decode) and decodeLikeHandlerStrict
// (the step-up classifier's read of the same bytes), so the classifier and the handler can never disagree about a body such as
// `{"reason_class":"manual",...}{"reason_class":"confirmed_sanctions",...}` (ADR 0116 A7 round 3; AML routes only: the other
// routes keep decodeJSON's historical behaviour).
func decodeSingleValue(dec *json.Decoder, dst any) error {
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errTrailingJSON
	}
	return nil
}

// decodeJSONStrict is decodeJSON plus the refusal of trailing content.
func decodeJSONStrict(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxRequestBodyBytes)
	return decodeSingleValue(json.NewDecoder(r.Body), dst)
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
