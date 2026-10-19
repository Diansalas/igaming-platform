package launchgov

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Licensing slice 1, G-12: a licence READ ERROR in R-LIC-VALID refuses (an error) instead of an acknowledgeable
// not_evaluable "read_error" check, aligning the code with ADR 0112 section 8.5.

type fakeLicenceRow struct {
	err                 error
	status              string
	issuedAt, expiresAt *time.Time
}

func (r fakeLicenceRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.status
	*(dest[1].(**time.Time)) = r.issuedAt
	*(dest[2].(**time.Time)) = r.expiresAt
	return nil
}

type fakeTx struct {
	pgx.Tx
	row fakeLicenceRow
}

func (f fakeTx) QueryRow(context.Context, string, ...any) pgx.Row { return f.row }

func licenceInput() EvalInput {
	id := uuid.New()
	return EvalInput{Licensing: &LicensingRecord{Model: LicModelPlatform, Status: LicStatusInForce, LicenceID: &id}}
}

func TestLicenceCheck_ReadErrorRefuses(t *testing.T) {
	s := &Service{}
	_, err := s.licenceCheck(context.Background(), fakeTx{row: fakeLicenceRow{err: errors.New("connection reset")}}, licenceInput(), time.Now())
	if err == nil {
		t.Fatal("a licence read error must refuse (return an error), not record an acknowledgeable check")
	}
}

func TestLicenceCheck_InvalidLicenceStaysAnAcknowledgeableFail(t *testing.T) {
	s := &Service{}
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	today := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		row  fakeLicenceRow
		want string
	}{
		"not found":      {fakeLicenceRow{err: pgx.ErrNoRows}, "licence_not_found"},
		"suspended":      {fakeLicenceRow{status: "suspended"}, "licence_suspended"},
		"status expired": {fakeLicenceRow{status: "expired"}, "licence_status_expired"},
		"expires today":  {fakeLicenceRow{status: "active", expiresAt: &today}, "licence_date_expired"},
	} {
		got, err := s.licenceCheck(context.Background(), fakeTx{row: c.row}, licenceInput(), now)
		if err != nil || got.Result != ResultFail || got.DetailCode != c.want {
			t.Errorf("%s: got %+v err=%v, want fail/%s", name, got, err, c.want)
		}
	}
	got, err := s.licenceCheck(context.Background(), fakeTx{row: fakeLicenceRow{status: "active"}}, licenceInput(), now)
	if err != nil || got.Result != ResultPass {
		t.Errorf("active licence: %+v %v", got, err)
	}
}
