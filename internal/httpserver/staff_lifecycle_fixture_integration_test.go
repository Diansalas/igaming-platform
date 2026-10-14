//go:build integration

package httpserver

// ADR 0119 S5 + S6 test fixtures: a server with the MOCK email provider (invitations and MFA resets are emailed),
// the invite -> accept -> enrol flow over HTTP, and a helper that reads the single-use token back from the mock
// mailbox (the API never returns it).

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// newLifecycleTestServer is newTestServer plus a MOCK email provider (returned for the mailbox).
func newLifecycleTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer) (*httptest.Server, *email.MockProvider) {
	t.Helper()
	// The platform four-eyes routes sign actor proofs (ADR 0110): this process's key in the test database.
	prooftest.Install(t, os.Getenv("TEST_DATABASE_URL"))
	mail := email.NewMockProvider()
	deps := financialTestDeps(pool, issuer, nil)
	deps.EmailProvider = mail
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	return srv, mail
}

// lastMailedToken returns the token of the newest email to `to` (the `token=` parameter of its link).
func lastMailedToken(t *testing.T, mail *email.MockProvider, to string) string {
	t.Helper()
	sent := mail.Sent()
	for i := len(sent) - 1; i >= 0; i-- {
		if !strings.EqualFold(sent[i].To, to) {
			continue
		}
		body := sent[i].Body
		k := strings.Index(body, "/#/enrol?")
		if k < 0 {
			t.Fatalf("fixture: email to %s has no enrolment link", to)
		}
		line := body[k+len("/#/enrol?"):]
		if nl := strings.IndexAny(line, "\n "); nl >= 0 {
			line = line[:nl]
		}
		q, err := url.ParseQuery(line)
		if err != nil || q.Get("token") == "" {
			t.Fatalf("fixture: email link %q: %v", line, err)
		}
		return q.Get("token")
	}
	t.Fatalf("fixture: no email to %s", to)
	return ""
}

// mailCount is the number of emails sent to `to`.
func mailCount(mail *email.MockProvider, to string) int {
	n := 0
	for _, m := range mail.Sent() {
		if strings.EqualFold(m.To, to) {
			n++
		}
	}
	return n
}

// fixtureAcceptAndEnrol accepts an invitation token with password and enrols a TOTP factor (registered in the S3
// fixture registry so mustLoginStaff can log in later); it returns the confirm token pair.
func fixtureAcceptAndEnrol(t *testing.T, srv *httptest.Server, token, password string) tokenPairResponse {
	t.Helper()
	r := staffFixturePost(t, srv, "/v1/staff/invitations/accept", map[string]string{"token": token, "password": password})
	if r.status != http.StatusOK {
		t.Fatalf("fixture: invitations/accept answered %d %s", r.status, r.body)
	}
	var ch staffChallengeWire
	if err := json.Unmarshal(r.body, &ch); err != nil || ch.Challenge.Type != "enrol" {
		t.Fatalf("fixture: accept challenge %s", r.body)
	}
	return fixtureEnrolWithChallenge(t, srv, ch.Challenge.Token)
}

// fixtureEnrolWithChallenge runs enrol/start and enrol/confirm for an `enrol` challenge.
func fixtureEnrolWithChallenge(t *testing.T, srv *httptest.Server, challenge string) tokenPairResponse {
	t.Helper()
	tok, err := staffmfa.ParseToken(challenge)
	if err != nil {
		t.Fatalf("fixture: challenge token: %v", err)
	}
	staffID := tok.StaffID()
	s := staffFixturePost(t, srv, "/v1/staff/auth/enrol/start", map[string]string{"challenge_token": challenge})
	if s.status != http.StatusOK {
		t.Fatalf("fixture: enrol/start answered %d %s", s.status, s.body)
	}
	var start enrolStartWire
	if err := json.Unmarshal(s.body, &start); err != nil {
		t.Fatal(err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(start.SecretBase32)
	if err != nil {
		t.Fatalf("fixture: secret: %v", err)
	}
	staffFixtures.Lock()
	staffFixtures.m[staffID] = &staffFixtureFactor{secret: secret}
	staffFixtures.Unlock()
	c := staffFixturePost(t, srv, "/v1/staff/auth/enrol/confirm", map[string]string{
		"challenge_token": challenge, "factor_id": start.FactorID, "code": staffFixtureNextCode(t, staffID)})
	if c.status != http.StatusOK {
		t.Fatalf("fixture: enrol/confirm answered %d %s", c.status, c.body)
	}
	var conf enrolConfirmWire
	if err := json.Unmarshal(c.body, &conf); err != nil {
		t.Fatal(err)
	}
	return conf.TokenPair
}

// mustSeedPerson inserts a fresh Person and returns its id.
func mustSeedPerson(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, id)
		return err
	}); err != nil {
		t.Fatalf("fixture: person: %v", err)
	}
	return id
}

// mustInviteStaff invites over HTTP (the caller's fresh token satisfies the critical step-up) and returns the
// created entry. personID may be uuid.Nil (no Person).
func mustInviteStaff(t *testing.T, srv *httptest.Server, adminToken string, tenantID uuid.UUID, emailAddr, role string, personID uuid.UUID) staffDirectoryEntryDTO {
	t.Helper()
	body := map[string]string{"email": emailAddr, "role": role}
	if personID != uuid.Nil {
		body["person_id"] = personID.String()
	}
	resp := postJSON(t, srv, "/v1/admin/tenants/"+tenantID.String()+"/staff/invitations", adminToken, body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		var e map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&e)
		t.Fatalf("fixture: invite %s as %s answered %d %v", emailAddr, role, resp.StatusCode, e)
	}
	var out staffLifecycleResultDTO
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.EmailDelivery != "sent" {
		t.Fatalf("fixture: invite email delivery %q", out.EmailDelivery)
	}
	return out.Staff
}
