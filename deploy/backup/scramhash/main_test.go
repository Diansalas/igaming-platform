package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// The verifier must have PostgreSQL's SCRAM-SHA-256 shape and must be re-derivable from the password (RFC 5802 / RFC 7677), so the
// server accepts it exactly as if it had computed it from the clear text.
func TestVerifierShapeAndDerivation(t *testing.T) {
	pw := "correct-horse-battery-staple-1234"
	salt := []byte("0123456789abcdef")
	v := verifier(pw, salt)
	if !regexp.MustCompile(`^SCRAM-SHA-256\$4096:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$`).MatchString(v) {
		t.Fatalf("bad shape: %s", v)
	}
	if strings.Contains(v, pw) {
		t.Fatal("verifier contains the password")
	}
	parts := strings.Split(strings.TrimPrefix(v, "SCRAM-SHA-256$4096:"), "$")
	saltB64 := parts[0]
	keys := strings.Split(parts[1], ":")
	if gotSalt, _ := base64.StdEncoding.DecodeString(saltB64); string(gotSalt) != string(salt) {
		t.Fatal("salt not round-tripped")
	}
	salted, err := pbkdf2.Key(sha256.New, pw, salt, 4096, 32)
	if err != nil {
		t.Fatal(err)
	}
	h := hmac.New(sha256.New, salted)
	h.Write([]byte("Client Key"))
	ck := h.Sum(nil)
	stored := sha256.Sum256(ck)
	if keys[0] != base64.StdEncoding.EncodeToString(stored[:]) {
		t.Error("StoredKey does not match an independent derivation")
	}
	h = hmac.New(sha256.New, salted)
	h.Write([]byte("Server Key"))
	if keys[1] != base64.StdEncoding.EncodeToString(h.Sum(nil)) {
		t.Error("ServerKey does not match an independent derivation")
	}
	if verifier(pw, []byte("fedcba9876543210")) == v {
		t.Error("different salts must give different verifiers")
	}
}
