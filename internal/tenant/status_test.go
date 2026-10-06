package tenant

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTranslateStatusChangeError(t *testing.T) {
	blocked := &pgconn.PgError{Code: "GP020", Detail: "sportsbook_open_bets=3"}
	got := TranslateStatusChangeError(fmt.Errorf("wrapped: %w", blocked))
	var cb *CloseBlockedError
	if !errors.As(got, &cb) || cb.SportsbookOpenBets != 3 {
		t.Fatalf("GP020 must translate with its counts, got %v", got)
	}
	if !errors.Is(got, ErrCloseBlockedOpenRounds) {
		t.Fatal("the typed error must match the sentinel")
	}
	// An unparseable DETAIL still yields the typed refusal (counts zero).
	if got := TranslateStatusChangeError(&pgconn.PgError{Code: "GP020", Detail: "garbage"}); !errors.Is(got, ErrCloseBlockedOpenRounds) {
		t.Fatalf("GP020 with an unreadable detail must still be the typed refusal, got %v", got)
	}
	// Any other error is returned unchanged.
	other := &pgconn.PgError{Code: "GP010"}
	if got := TranslateStatusChangeError(other); got != error(other) {
		t.Fatalf("a non-GP020 error must pass through, got %v", got)
	}
	plain := errors.New("boom")
	if got := TranslateStatusChangeError(plain); got != plain {
		t.Fatalf("plain error must pass through")
	}
	// The message never carries more than the counts.
	if msg := (&CloseBlockedError{SportsbookOpenBets: 2}).Error(); msg != "tenant: closure refused: open gaming rounds exist: sportsbook_open_bets=2" {
		t.Fatalf("unexpected message %q", msg)
	}
}

func TestChangeStatusValidation(t *testing.T) {
	for _, p := range []ChangeStatusParams{
		{NewStatus: "gone"},
		{NewStatus: "closed"},
	} {
		if err := ChangeStatus(nil, nil, p); !errors.Is(err, ErrInvalidStatusChange) { //nolint:staticcheck // validation fails before the runner or context is used
			t.Fatalf("%+v: expected ErrInvalidStatusChange, got %v", p, err)
		}
	}
}
