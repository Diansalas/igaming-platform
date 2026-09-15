// Upload validation (directive §21): never trust a client-claimed
// Content-Type, cap size, sniff actual content, cross-check the sniffed
// type against the claimed extension, and sanitize the filename before
// it is ever persisted.
package kyc

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"
)

// MaxDocumentSizeBytes bounds a single upload - a generous but finite
// limit for a photographed/scanned identity document or proof-of-address
// letter. Chosen as a reasonable foundation-stage default, not derived
// from any specific vendor's own limit (none is contracted yet).
const MaxDocumentSizeBytes = 15 * 1024 * 1024 // 15MB

// allowedContentTypes maps a SNIFFED (never claimed) content type to the
// filename extensions considered consistent with it - directive §21's
// "extension/content consistency" and "extension spoofing protection".
var allowedContentTypes = map[string][]string{
	"image/jpeg":      {".jpg", ".jpeg"},
	"image/png":       {".png"},
	"application/pdf": {".pdf"},
}

// ErrUploadInvalid wraps every way ValidateUpload rejects a file (empty,
// too large, disallowed/unsniffable content type, extension mismatch) -
// one sentinel a caller checks via errors.Is to distinguish "this was a
// validation problem" (400) from an unexpected internal error (500),
// without needing to parse the message string.
var ErrUploadInvalid = errors.New("kyc: upload failed validation")

// ValidateUpload sniffs content's ACTUAL type via the standard library's
// content-sniffing algorithm (never the client-supplied Content-Type
// header, which is not trusted at all - directive §21's explicit
// requirement), checks it against the size limit and the fixed allowlist,
// cross-checks the sniffed type against filename's extension, and returns
// a sanitized filename safe to store as metadata. Returns the SNIFFED
// content type - callers must persist THIS, never the client's claimed
// header value.
func ValidateUpload(filename string, content []byte) (sniffedContentType, sanitizedFilename string, err error) {
	if len(content) == 0 {
		return "", "", fmt.Errorf("%w: uploaded file is empty", ErrUploadInvalid)
	}
	if len(content) > MaxDocumentSizeBytes {
		return "", "", fmt.Errorf("%w: uploaded file exceeds the %d byte limit", ErrUploadInvalid, MaxDocumentSizeBytes)
	}

	sniffed := http.DetectContentType(content)
	// http.DetectContentType appends a charset parameter for some text
	// types (e.g. "text/plain; charset=utf-8") - normalize to the bare
	// MIME type before checking the allowlist.
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = strings.TrimSpace(sniffed[:i])
	}
	allowedExts, ok := allowedContentTypes[sniffed]
	if !ok {
		return "", "", fmt.Errorf("%w: file content type %q is not an accepted document format", ErrUploadInvalid, sniffed)
	}

	sanitized := sanitizeFilename(filename)
	ext := strings.ToLower(filepath.Ext(sanitized))
	extMatches := false
	for _, allowed := range allowedExts {
		if ext == allowed {
			extMatches = true
			break
		}
	}
	if !extMatches {
		return "", "", fmt.Errorf("%w: file extension %q does not match its actual content type %q", ErrUploadInvalid, ext, sniffed)
	}

	return sniffed, sanitized, nil
}

// sanitizeFilename strips any path component (directory traversal
// protection - directive §21's "filename normalization") and replaces
// every character outside a small safe set with "_", bounding length.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		s = "document"
	}
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return s
}
