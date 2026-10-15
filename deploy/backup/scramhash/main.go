// Command scramhash computes a PostgreSQL SCRAM-SHA-256 password verifier on the CLIENT, so that a role's password never travels
// in clear text inside a statement that the server (log_statement, log_min_duration_statement) or pgaudit could record
// (A12 security review M2). The password is read from standard input (never argv); the verifier is written to standard output.
//
//	printf '%s' "$password" | scramhash        ->  SCRAM-SHA-256$4096:<salt>$<StoredKey>:<ServerKey>
//
// The verifier is accepted by PostgreSQL in ALTER/CREATE ROLE ... PASSWORD '<verifier>' exactly as the server would have
// computed it from the clear text. The password is limited to printable ASCII (no SASLprep normalisation is implemented).
package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

const iterations = 4096

func main() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		fmt.Fprintln(os.Stderr, "scramhash:", err)
		os.Exit(2)
	}
	pw := strings.TrimRight(string(raw), "\r\n")
	if len(pw) < 16 {
		fmt.Fprintln(os.Stderr, "scramhash: password must be at least 16 characters")
		os.Exit(2)
	}
	for _, r := range pw {
		if r < 0x21 || r > 0x7e {
			fmt.Fprintln(os.Stderr, "scramhash: password must be printable ASCII without spaces")
			os.Exit(2)
		}
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		fmt.Fprintln(os.Stderr, "scramhash:", err)
		os.Exit(2)
	}
	fmt.Println(verifier(pw, salt))
}

func verifier(pw string, salt []byte) string {
	salted, err := pbkdf2.Key(sha256.New, pw, salt, iterations, sha256.Size)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scramhash:", err)
		os.Exit(2)
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	serverKey := mac(salted, "Server Key")
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, enc(salt), enc(stored[:]), enc(serverKey))
}
