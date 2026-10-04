package httpserver

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

type simLogger struct{}

func (simLogger) Error(string, ...any) {}

// ADR 0102 8 row 11: the simulation route's payload-mismatch alert hook fires
// for exactly the payload-mismatch class (and answers 409 as before), never
// for any other error.
func TestWriteDepositCallbackError_PayloadMismatchHookFiresOnlyForPayloadMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"payload_mismatch", fmt.Errorf("wrap: %w", payments.ErrCallbackPayloadMismatch), true},
		{"provider_mismatch", payments.ErrCallbackProviderMismatch, false},
		{"already_reversed", payments.ErrDepositAlreadyReversed, false},
		{"not_found", payments.ErrDepositIntentNotFound, false},
		{"unrelated", errors.New("boom"), false},
		{"other_domain", casino.ErrBetNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fired int
			rec := httptest.NewRecorder()
			writeDepositCallbackError(rec, "req-1", simLogger{}, tc.err, func() { fired++ })
			if (fired == 1) != tc.want || fired > 1 {
				t.Fatalf("hook fired %d times, want fired=%v", fired, tc.want)
			}
			if tc.name == "payload_mismatch" && rec.Code != http.StatusConflict {
				t.Fatalf("payload mismatch must keep answering 409, got %d", rec.Code)
			}
		})
	}
	// A nil hook is allowed (no panic).
	writeDepositCallbackError(httptest.NewRecorder(), "req-1", simLogger{}, payments.ErrCallbackPayloadMismatch, nil)
}

// C1: the simulation route's payload-mismatch hook runs only after the response
// has been written AND flushed to the client.
func TestWriteDepositCallbackError_PayloadMismatchHookRunsAfterTheResponseIsFlushed(t *testing.T) {
	rec := httptest.NewRecorder()
	var bodyAtHook, flushedAtHook bool
	writeDepositCallbackError(rec, "req-1", simLogger{}, payments.ErrCallbackPayloadMismatch, func() {
		bodyAtHook = rec.Body.Len() > 0
		flushedAtHook = rec.Flushed
	})
	if !bodyAtHook || !flushedAtHook {
		t.Fatalf("the hook must run after the body is written and flushed: body=%v flushed=%v", bodyAtHook, flushedAtHook)
	}
}
