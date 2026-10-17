package httpserver

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestLogDepositTombstoneProofKeyFault(t *testing.T) {
	for _, code := range []string{"AP002", "AP003"} {
		var buf bytes.Buffer
		lg := slog.New(slog.NewTextHandler(&buf, nil))
		// The message text carries proof-like material that must NOT reach the log.
		err := fmt.Errorf("post tombstone: %w", &pgconn.PgError{Code: code, Message: "actor_proof: proof rejected SECRETPROOFMATERIAL"})
		if !logDepositTombstoneProofKeyFault(lg, err, "psp-1", "tenant-1", "req-1") {
			t.Fatalf("%s must be handled", code)
		}
		out := buf.String()
		for _, want := range []string{"payment_webhook_deposit_tombstone_proof_key_fault", "code=" + code, "provider_id=psp-1", "tenant_id=tenant-1", "request_id=req-1"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: log %q lacks %q", code, out, want)
			}
		}
		if strings.Contains(out, "SECRETPROOFMATERIAL") || strings.Contains(out, "error=") {
			t.Errorf("%s: log must carry the code only: %q", code, out)
		}
	}
}

func TestLogDepositTombstoneProofKeyFault_OtherErrorsNotHandled(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	for _, err := range []error{
		errors.New("boom"),
		&pgconn.PgError{Code: "AP001"},
		&pgconn.PgError{Code: "DT001"},
		&pgconn.PgError{Code: "23505"},
	} {
		if logDepositTombstoneProofKeyFault(lg, err, "p", "t", "r") {
			t.Errorf("%v must fall through to payment_webhook_failed", err)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("nothing may be logged: %q", buf.String())
	}
}
