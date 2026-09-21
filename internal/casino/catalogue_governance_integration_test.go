//go:build integration

// Stage 9.2, Workstream A: closes ARCH-DB-2 Phase 2 (docs/decisions/0081
// §5.2) - functional and adversarial coverage for FileChangeRequest/
// DecideChangeRequest and the casino_games_dual_control trigger they feed
// (migration 0086), matching this codebase's own established four-eyes
// testing idioms: internal/withdrawal's stage9_four_eyes_adversarial_test.go
// (duplicate/concurrent-duplicate approval, same-person-two-logins
// self-approval, both sequential and concurrent) and
// internal/casino/lockorder_integration_test.go (deterministic
// concurrency).
package casino

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// seedGovernanceGame registers a fresh platform-catalogue title with the
// given jurisdiction_blocklist/status - the target every test below files
// a change request against.
func seedGovernanceGame(t *testing.T, pool *db.Pool, blocklist []string, status GameStatus) Game {
	t.Helper()
	var g Game
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: "gov-provider-" + uuid.New().String()[:8], ProviderGameID: "gov-game-" + uuid.New().String()[:8],
			Name: "Governance Test Game", GameType: "slot",
			JurisdictionBlocklist: blocklist, Status: status,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed governance game: %v", err)
	}
	return g
}

// seedTwoPlatformPrincipalsSharingPerson inserts two distinct, platform-
// scoped staff_users rows that both resolve to the SAME persons row - one
// human with two logins. Mirrors internal/withdrawal's
// s9CreateApproversSharingOnePerson exactly, adapted to platform scope
// (tenant_id NULL) since casino_games governance is platform-only.
func seedTwoPlatformPrincipalsSharingPerson(t *testing.T, pool *db.Pool) []uuid.UUID {
	t.Helper()
	personID := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatalf("seed shared person: %v", err)
	}
	ids := make([]uuid.UUID, 2)
	for i := range ids {
		id := uuid.New()
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, NULL, $2, 'x', 'platform_admin', $3)`,
				id, "shared-person-"+id.String()+"@test.example", personID)
			return err
		}); err != nil {
			t.Fatalf("seed shared-person platform principal %d: %v", i, err)
		}
		ids[i] = id
	}
	return ids
}

// fileGovernanceRequest is a small helper wrapping FileChangeRequest in
// its own WithPlatformAdmin transaction.
func fileGovernanceRequest(t *testing.T, pool *db.Pool, p FileChangeRequestParams) ChangeRequest {
	t.Helper()
	var req ChangeRequest
	err := pool.WithPlatformAdmin(context.Background(), p.RequestedByPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		req, err = FileChangeRequest(ctx, tx, p)
		return err
	})
	if err != nil {
		t.Fatalf("file change request: %v", err)
	}
	return req
}

// decideGovernanceRequest is a small helper wrapping DecideChangeRequest
// in its own WithPlatformAdmin transaction.
func decideGovernanceRequest(pool *db.Pool, p DecideChangeRequestParams) (ChangeApproval, error) {
	var ap ChangeApproval
	err := pool.WithPlatformAdmin(context.Background(), p.ApproverPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ap, err = DecideChangeRequest(ctx, tx, p)
		return err
	})
	return ap, err
}

// --- 1. Transaction scope -------------------------------------------------

func TestFileChangeRequest_RequiresPlatformScope(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
			ReasonCode: "test", RequestedByPrincipalID: uuid.New(),
		})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope, got %v", err)
	}
}

func TestDecideChangeRequest_RequiresPlatformScope(t *testing.T) {
	pool := testPool(t)
	err := pool.WithTenant(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: uuid.New(), Approve: true, ReasonCode: "test", ApproverPrincipalID: uuid.New(),
		})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope, got %v", err)
	}
}

// --- 2. Input validation ---------------------------------------------------

func TestFileChangeRequest_JurisdictionUnblock_RequiresCodesCurrentlyBlocked(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"FR"},
			ReasonCode: "test", RequestedByPrincipalID: requester,
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a code not currently blocked, got %v", err)
	}
}

func TestFileChangeRequest_StatusActivate_RequiresGameCurrentlyDisabled(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, nil, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeStatusActivate, GameID: game.ID,
			ReasonCode: "test", RequestedByPrincipalID: requester,
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a game that is not disabled, got %v", err)
	}
}

func TestFileChangeRequest_UnknownGame_ReturnsErrGameNotFound(t *testing.T) {
	pool := testPool(t)
	requester := seedPlatformAdminStaffPrincipal(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeStatusActivate, GameID: uuid.New(),
			ReasonCode: "test", RequestedByPrincipalID: requester,
		})
		return err
	})
	if !errors.Is(err, ErrGameNotFound) {
		t.Fatalf("expected ErrGameNotFound, got %v", err)
	}
}

// TestFileChangeRequest_NormalizesRemovedCodes proves the approved payload
// is sorted/de-duplicated regardless of the caller's own ordering/
// repetition - migration 0086's trigger compares SORTED SETS, so this
// normalization is what makes that comparison meaningful rather than
// accidentally order-sensitive.
func TestFileChangeRequest_NormalizesRemovedCodes(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE", "FR"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)

	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"FR", "DE", "FR"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	codes, ok := req.Payload["removed_codes"].([]string)
	if !ok {
		t.Fatalf("expected payload[removed_codes] to be []string, got %T: %v", req.Payload["removed_codes"], req.Payload["removed_codes"])
	}
	if len(codes) != 2 || codes[0] != "DE" || codes[1] != "FR" {
		t.Fatalf("expected normalized sorted/deduped [DE FR], got %v", codes)
	}
}

// --- 3. Self-approval -------------------------------------------------

func TestDecideChangeRequest_SelfApprovalDenied(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})

	_, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: requester,
	})
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("expected ErrSelfApproval, got %v", err)
	}
}

// TestDecideChangeRequest_SelfApprovalDenied_SamePersonTwoLogins is the
// bypass a mere distinct-principal check cannot catch: two staff logins,
// one human. Mirrors
// internal/withdrawal.TestStage9_TwoStaffLoginsOnePerson_CannotSatisfyFourEyesAlone.
func TestDecideChangeRequest_SelfApprovalDenied_SamePersonTwoLogins(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	logins := seedTwoPlatformPrincipalsSharingPerson(t, pool)

	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: logins[0],
	})

	_, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: logins[1],
	})
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("expected ErrSelfApproval (same person, second staff login), got %v", err)
	}

	// A genuinely different human can still approve it.
	third := seedPlatformAdminStaffPrincipal(t, pool)
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: third,
	}); err != nil {
		t.Fatalf("a genuinely distinct principal must be able to approve: %v", err)
	}
}

// --- 3b. Principal eligibility hardening (migration 0089, SEC-S92-1) -------
//
// Migration 0086's original self-approval/principal-eligibility check
// mirrored migration 0044's ORIGINAL (weak) shape: the person comparison
// only fired when BOTH sides' person_id were non-NULL, and there was no
// staff_users.status = 'active' check anywhere. Security reproduced,
// empirically, that two platform_admin staff accounts with person_id IS
// NULL - seed-admin's actual default output - could file -> approve ->
// apply the SAME change end to end (one human, two accounts, four-eyes
// defeated), and separately that a SUSPENDED staff principal was accepted
// as a valid second approver. The tests below prove migration 0089's fix
// (mirroring migration 0047's hardened asset-registry shape) closes both.

// TestFileChangeRequest_UnlinkedPrincipal_Rejected proves an unlinked
// (person_id IS NULL) principal cannot even FILE a request any more -
// the first step of the reproduced bypass now fails outright.
func TestFileChangeRequest_UnlinkedPrincipal_Rejected(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedUnlinkedPlatformAdminStaffPrincipal(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
			ReasonCode: "test", RequestedByPrincipalID: requester,
		})
		return err
	})
	if !errors.Is(err, ErrPrincipalNotEligible) {
		t.Fatalf("expected ErrPrincipalNotEligible for an unlinked (person_id IS NULL) requester, got %v", err)
	}
}

// TestDecideChangeRequest_UnlinkedApprover_Rejected proves an unlinked
// SECOND account cannot approve either, even when the request was itself
// filed (before this migration's fix, or by direct SQL bypassing
// FileChangeRequest's own Go-level validation - the request row itself
// carries no eligibility state, so this exercises the DecideChangeRequest/
// approvals-side trigger specifically) by a properly linked requester.
func TestDecideChangeRequest_UnlinkedApprover_Rejected(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})

	approver := seedUnlinkedPlatformAdminStaffPrincipal(t, pool)
	_, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	})
	if !errors.Is(err, ErrPrincipalNotEligible) {
		t.Fatalf("expected ErrPrincipalNotEligible for an unlinked (person_id IS NULL) approver, got %v", err)
	}
}

// TestDecideChangeRequest_SuspendedApprover_Rejected proves migration
// 0089's new status='active' check - absent entirely from migration 0086
// - actually fires: a person-linked but SUSPENDED second account must not
// count as a valid approver.
func TestDecideChangeRequest_SuspendedApprover_Rejected(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})

	approver := seedSuspendedPlatformAdminStaffPrincipal(t, pool)
	_, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	})
	if !errors.Is(err, ErrPrincipalNotEligible) {
		t.Fatalf("expected ErrPrincipalNotEligible for a suspended approver, got %v", err)
	}
}

// TestOldWeakBypass_TwoUnlinkedPrincipals_FileThenApprove_NowRejected
// reproduces security's exact empirical finding end to end: two
// platform_admin staff accounts, BOTH with person_id IS NULL (the
// seed-admin default shape) - one human, two accounts - attempting
// file -> approve. Under migration 0086 this succeeded in full (self-
// approval through a second staff account went undetected because the
// person comparison never fired with both sides NULL). Under migration
// 0089 it must fail at the very first step, not silently succeed.
func TestOldWeakBypass_TwoUnlinkedPrincipals_FileThenApprove_NowRejected(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	principalA := seedUnlinkedPlatformAdminStaffPrincipal(t, pool)
	principalB := seedUnlinkedPlatformAdminStaffPrincipal(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), principalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
			ReasonCode: "one-human-two-accounts", RequestedByPrincipalID: principalA,
		})
		return err
	})
	if !errors.Is(err, ErrPrincipalNotEligible) {
		t.Fatalf("expected the file step of the old bypass to be rejected with ErrPrincipalNotEligible, got %v", err)
	}

	// Even if a request had somehow been filed (e.g. a pre-existing row
	// from before this migration), the second unlinked account still
	// cannot decide it - belt and braces, proven directly against the
	// approvals-side trigger via a request filed by a properly linked
	// third party.
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	_, err = decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: principalB,
	})
	if !errors.Is(err, ErrPrincipalNotEligible) {
		t.Fatalf("expected the approve step of the old bypass to be rejected with ErrPrincipalNotEligible, got %v", err)
	}
}

// --- 4. Duplicate approval, sequential and concurrent ----------------------

func TestDecideChangeRequest_DuplicateApprovalBySameApprover_Sequential(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	approver := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})

	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("first approval must succeed: %v", err)
	}

	_, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	})
	if !errors.Is(err, ErrDuplicateDecision) {
		t.Fatalf("expected ErrDuplicateDecision on a replayed approval, got %v", err)
	}
}

// TestDecideChangeRequest_ConcurrentSameApproverDoubleSubmit is the
// realistic shape of the case above: one approver's client submits the
// same decision twice at the same instant. Mirrors
// internal/withdrawal.TestStage9_ConcurrentSameApproverDoubleSubmit_RecordsExactlyOneApproval.
func TestDecideChangeRequest_ConcurrentSameApproverDoubleSubmit(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	approver := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})

	const n = 4
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = decideGovernanceRequest(pool, DecideChangeRequestParams{
				RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var succeeded, duplicates int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrDuplicateDecision):
			duplicates++
		default:
			t.Fatalf("goroutine %d: expected nil or ErrDuplicateDecision, got %v", i, err)
		}
	}
	if succeeded != 1 || duplicates != n-1 {
		t.Fatalf("expected exactly 1 of %d concurrent identical approvals to succeed, got succeeded=%d duplicates=%d", n, succeeded, duplicates)
	}

	count := countApprovals(t, pool, req.ID)
	if count != 1 {
		t.Fatalf("expected exactly 1 recorded approval row, got %d", count)
	}
}

// --- 5. Two distinct approvers racing, and approve-vs-reject racing -------

// TestDecideChangeRequest_TwoDistinctApproversRacing_BothRecorded proves
// two genuinely different principals may decide the same request
// concurrently without interfering with each other (no false conflict).
func TestDecideChangeRequest_TwoDistinctApproversRacing_BothRecorded(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	approverA := seedPlatformAdminStaffPrincipal(t, pool)
	approverB := seedPlatformAdminStaffPrincipal(t, pool)

	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, approver := range []uuid.UUID{approverA, approverB} {
		wg.Add(1)
		go func(i int, approver uuid.UUID) {
			defer wg.Done()
			<-start
			_, errs[i] = decideGovernanceRequest(pool, DecideChangeRequestParams{
				RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
			})
		}(i, approver)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("distinct approver %d: expected success, got %v", i, err)
		}
	}
	if count := countApprovals(t, pool, req.ID); count != 2 {
		t.Fatalf("expected 2 recorded approvals, got %d", count)
	}
}

// TestApplyDualControl_RejectionRacingApproval_NeverApplies races one
// approve against one reject from two distinct principals, then proves
// the rejection permanently blocks apply regardless of which decision
// physically committed first - migration
// 0086's casino_catalogue_change_consume_approved_request requires BOTH
// "at least one approval by a different principal" AND "no rejection by
// anyone", so a racing reject must never be overridden by a racing
// approve.
func TestApplyDualControl_RejectionRacingApproval_NeverApplies(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, nil, GameStatusDisabled)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeStatusActivate, GameID: game.ID,
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	approver := seedPlatformAdminStaffPrincipal(t, pool)
	rejecter := seedPlatformAdminStaffPrincipal(t, pool)

	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = decideGovernanceRequest(pool, DecideChangeRequestParams{
			RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errs[1] = decideGovernanceRequest(pool, DecideChangeRequestParams{
			RequestID: req.ID, Approve: false, ReasonCode: "test", ApproverPrincipalID: rejecter,
		})
	}()
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("decision %d: expected success recording the decision itself, got %v", i, err)
		}
	}

	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			Status: GameStatusActive,
		})
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("expected ErrDualControlRequired (a rejection must permanently block apply), got %v", err)
	}
}

// --- 6. Deciding a request that has already left 'pending' ----------------

func TestDecideChangeRequest_AlreadyApplied_Rejected(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, nil, GameStatusDisabled)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeStatusActivate, GameID: game.ID,
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	approver := seedPlatformAdminStaffPrincipal(t, pool)
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approval must succeed: %v", err)
	}

	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			Status: GameStatusActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("applying an approved request must succeed: %v", err)
	}

	thirdParty := seedPlatformAdminStaffPrincipal(t, pool)
	_, err = decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: thirdParty,
	})
	if !errors.Is(err, ErrChangeRequestNotPending) {
		t.Fatalf("expected ErrChangeRequestNotPending for a decision on an already-applied request, got %v", err)
	}
}

// TestDecideChangeRequest_AlreadyCancelled_Rejected mirrors
// internal/assetregistry's own test idiom (direct SQL to reach the
// 'cancelled' state, since no application-level cancel operation exists
// yet - the ADR's schema reserves the value for a future admin action):
// once a request has left 'pending' by any means, DecideChangeRequest
// must refuse a decision on it.
func TestDecideChangeRequest_AlreadyCancelled_Rejected(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})

	if err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE casino_catalogue_change_requests SET state = 'cancelled' WHERE id = $1`, req.ID)
		return err
	}); err != nil {
		t.Fatalf("manually cancel request: %v", err)
	}

	approver := seedPlatformAdminStaffPrincipal(t, pool)
	_, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	})
	if !errors.Is(err, ErrChangeRequestNotPending) {
		t.Fatalf("expected ErrChangeRequestNotPending for a decision on an already-cancelled request, got %v", err)
	}
}

// --- 7. Rollback / failure leaves no partial state -------------------------

// TestDecideChangeRequest_TransactionRollback_LeavesNoPartialState proves
// that when the transaction DecideChangeRequest ran in is rolled back
// (simulating a downstream failure, e.g. the caller's own audit write
// failing), neither the approval row nor any request-state change
// persists - CLAUDE.md's "every financial/administrative write is
// transactional" rule, applied to this governance surface.
func TestDecideChangeRequest_TransactionRollback_LeavesNoPartialState(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	approver := seedPlatformAdminStaffPrincipal(t, pool)

	simulatedFailure := errors.New("simulated downstream failure (e.g. audit write)")
	err := pool.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
		}); err != nil {
			return err
		}
		return simulatedFailure
	})
	if !errors.Is(err, simulatedFailure) {
		t.Fatalf("expected the simulated downstream failure to propagate, got %v", err)
	}

	if count := countApprovals(t, pool, req.ID); count != 0 {
		t.Fatalf("expected 0 approval rows after a rolled-back transaction, got %d", count)
	}

	var state string
	if err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM casino_catalogue_change_requests WHERE id = $1`, req.ID).Scan(&state)
	}); err != nil {
		t.Fatalf("read request state: %v", err)
	}
	if state != "pending" {
		t.Fatalf("expected request to remain 'pending' after a rolled-back decision, got %q", state)
	}
}

// --- 8. Payload-match discipline -------------------------------------------

// TestApplyDualControl_PayloadMismatch_Rejected proves approving "unblock
// DE" can never be spent applying "unblock DE, FR". Migration 0089
// changed selection itself to be payload-correlated (SEC-S92-5): the
// approved request's removed_codes must CONTAIN every code actually being
// removed, so an approval that only covers DE is no longer even a
// candidate for a removal of DE+FR - the refusal now surfaces as
// ErrDualControlRequired ("no approved request covers this") rather than
// migration 0086's ErrInvalidInput ("found a request, but its payload
// didn't match"), since there is no longer a wrong candidate to find in
// the first place. Either way the whole UPDATE is aborted and the
// pending request is left untouched - see
// TestApplyDualControl_SupersetApproval_PartialRemoval_Rejected below for
// the payload-mismatch branch that IS still reachable (an approval wider
// than the removal actually being made).
func TestApplyDualControl_PayloadMismatch_Rejected(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE", "FR"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	approver := seedPlatformAdminStaffPrincipal(t, pool)
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approval must succeed: %v", err)
	}

	// Attempt to remove BOTH DE and FR using an approval that only covers
	// DE.
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{}, Status: game.Status,
		})
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("expected ErrDualControlRequired for a removal broader than any approved request (approved DE only, attempted DE+FR), got %v", err)
	}

	// The failed attempt must not have consumed the request - it is still
	// approved-and-pending, usable for the CORRECT removal.
	var state string
	if err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM casino_catalogue_change_requests WHERE id = $1`, req.ID).Scan(&state)
	}); err != nil {
		t.Fatalf("read request state: %v", err)
	}
	if state != "pending" {
		t.Fatalf("expected request to remain 'pending' after a rejected payload-mismatched apply attempt, got %q", state)
	}

	err = pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{"FR"}, Status: game.Status,
		})
		return err
	})
	if err != nil {
		t.Fatalf("removing exactly the approved code (DE) must succeed: %v", err)
	}
}

// TestApplyDualControl_SupersetApproval_PartialRemoval_Rejected is the
// payload-mismatch branch that IS still reachable after migration 0089's
// containment-based selection: an approval WIDER than the removal
// actually being made (approved DE+FR, but this UPDATE only removes DE)
// still selects that request (its removed_codes contains DE), but the
// post-hoc exact-sorted-set-equality check must still refuse spending a
// broader approval on a narrower removal.
func TestApplyDualControl_SupersetApproval_PartialRemoval_Rejected(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE", "FR"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE", "FR"},
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	approver := seedPlatformAdminStaffPrincipal(t, pool)
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approval must succeed: %v", err)
	}

	// Remove only DE, though DE+FR was approved together.
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{"FR"}, Status: game.Status,
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput spending a DE+FR approval on a DE-only removal, got %v", err)
	}

	// The request must remain pending and usable for the FULL removal it
	// was actually approved for.
	if got := changeRequestState(t, pool, requester, req.ID); got != "pending" {
		t.Fatalf("expected request to remain 'pending' after a rejected superset-approval partial removal, got %q", got)
	}

	err = pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{}, Status: game.Status,
		})
		return err
	})
	if err != nil {
		t.Fatalf("removing exactly the approved codes (DE+FR) together must succeed: %v", err)
	}
}

// --- 9. Atomicity of consume-and-apply under concurrency -------------------

// TestCasinoCatalogueChangeConsumeApprovedRequest_ConcurrentCalls_OnlyOneSucceeds
// is the precise atomicity proof the directive asks for ("two simultaneous
// approve-then-immediately-try-apply sequences must not both succeed"),
// exercised directly against casino_catalogue_change_consume_approved_
// request rather than through UpsertGame's UPDATE: calling UpsertGame
// concurrently for the SAME already-approved transition is not a suitable
// probe for this, because under READ COMMITTED a blocked UPDATE that wakes
// up re-reads the row the first committer just wrote - so goroutines 2..n
// legitimately observe OLD.status already 'active' and never attempt a
// second consumption at all (proven separately below, as the correct,
// idempotent, single-consumption behaviour it is - not a race). Calling
// the SQL function itself directly, with fixed (game_id, operation)
// arguments that do not depend on any row's current state, is what
// actually forces every goroutine to attempt the SAME consumption, so only
// FOR UPDATE - not incidental MVCC re-reading - can be what serializes
// them.
func TestCasinoCatalogueChangeConsumeApprovedRequest_ConcurrentCalls_OnlyOneSucceeds(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, nil, GameStatusDisabled)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeStatusActivate, GameID: game.ID,
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	approver := seedPlatformAdminStaffPrincipal(t, pool)
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approval must succeed: %v", err)
	}

	const n = 6
	errs := make([]error, n)
	consumedIDs := make([]uuid.UUID, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT casino_catalogue_change_consume_approved_request($1, $2)`,
					game.ID, string(ChangeStatusActivate)).Scan(&consumedIDs[i])
			})
			if errs[i] != nil {
				errs[i] = classifyChangeRequestTriggerError(errs[i])
			}
		}(i)
	}
	close(start)
	wg.Wait()

	var succeeded, denied int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
			if consumedIDs[i] != req.ID {
				t.Fatalf("goroutine %d: consumed unexpected request id %s (want %s)", i, consumedIDs[i], req.ID)
			}
		case errors.Is(err, ErrDualControlRequired):
			denied++
		default:
			t.Fatalf("goroutine %d: expected nil or ErrDualControlRequired, got %v", i, err)
		}
	}
	if succeeded != 1 || denied != n-1 {
		t.Fatalf("expected exactly 1 of %d concurrent consume calls to succeed, got succeeded=%d denied=%d", n, succeeded, denied)
	}
}

// TestApplyDualControl_ConcurrentIdempotentApplyAttempts_ConsumesRequestExactlyOnce
// documents the REALISTIC caller-facing shape of the same guarantee:
// concurrent UpsertGame calls all requesting the SAME already-approved
// transition all succeed (a later one is an idempotent no-op once the
// first has already applied it - never an error, never a double-charge of
// the approval), and the underlying request is nonetheless consumed
// exactly once.
func TestApplyDualControl_ConcurrentIdempotentApplyAttempts_ConsumesRequestExactlyOnce(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, nil, GameStatusDisabled)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	req := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeStatusActivate, GameID: game.ID,
		ReasonCode: "test", RequestedByPrincipalID: requester,
	})
	approver := seedPlatformAdminStaffPrincipal(t, pool)
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: req.ID, Approve: true, ReasonCode: "test", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approval must succeed: %v", err)
	}

	const n = 4
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
				_, err := UpsertGame(ctx, tx, UpsertGameInput{
					ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
					Status: GameStatusActive,
				})
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: an idempotent re-application of an already-applied transition must never error, got %v", i, err)
		}
	}

	var state string
	var appliedBy *uuid.UUID
	if err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, applied_by_principal_id FROM casino_catalogue_change_requests WHERE id = $1`, req.ID).Scan(&state, &appliedBy)
	}); err != nil {
		t.Fatalf("read request state: %v", err)
	}
	if state != "applied" {
		t.Fatalf("expected request state 'applied', got %q", state)
	}
	if appliedBy == nil {
		t.Fatal("expected applied_by_principal_id to be set")
	}
}

// --- 10. Fail-closed direction and creation remain single-actor -----------

// TestUpsertGame_AddingBlocklistCode_RemainsSingleActor proves the
// fail-closed direction (adding a code) needs no approval, unaffected by
// this migration.
func TestUpsertGame_AddingBlocklistCode_RemainsSingleActor(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, nil, GameStatusActive)

	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{"DE"}, Status: game.Status,
		})
		return err
	})
	if err != nil {
		t.Fatalf("adding a jurisdiction_blocklist code must remain single-actor: %v", err)
	}
}

// TestUpsertGame_DisablingGame_RemainsSingleActor proves the fail-closed
// direction (status active->disabled) needs no approval.
func TestUpsertGame_DisablingGame_RemainsSingleActor(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, nil, GameStatusActive)

	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			Status: GameStatusDisabled,
		})
		return err
	})
	if err != nil {
		t.Fatalf("disabling a game must remain single-actor: %v", err)
	}
}

// TestUpsertGame_CreationIsNotDualControlled proves a brand-new row may be
// created directly active/disabled with any blocklist, with no approval -
// dual control applies only to the UPDATE path (ADR 0081 §2.5.1/§5).
func TestUpsertGame_CreationIsNotDualControlled(t *testing.T) {
	pool := testPool(t)
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: "gov-create-" + uuid.New().String()[:8], ProviderGameID: "gov-create-game",
			Name: "Fresh Game", GameType: "slot",
			JurisdictionBlocklist: []string{"DE"}, Status: GameStatusDisabled,
		})
		return err
	})
	if err != nil {
		t.Fatalf("creation must never require an approval: %v", err)
	}
}

// --- 11. Payload-correlated consume selection (migration 0089, SEC-S92-5) --
//
// Migration 0086's casino_catalogue_change_consume_approved_request
// selected the OLDEST pending-with-approval request for (game_id,
// operation), ignoring payload entirely. Both reviewers reproduced: two
// pending jurisdiction_unblock requests for the same game (one approved
// to unblock DE, a separate LATER one approved to unblock FR) - a PUT
// removing only FR consumed the OLDER DE request instead, failed the
// payload-match check, and aborted the whole UPDATE, permanently
// deadlocking the FR approval (no cancel/withdraw endpoint exists). The
// tests below prove migration 0089's payload-correlated selection fixes
// this in BOTH orders.

// TestApplyDualControl_TwoDistinctApprovedRequests_ApplyIndependently_FRFirst
// is the precise reproduction: the OLDER request (DE) must NOT be
// consumed by a PUT that only removes FR.
func TestApplyDualControl_TwoDistinctApprovedRequests_ApplyIndependently_FRFirst(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE", "FR"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	approver := seedPlatformAdminStaffPrincipal(t, pool)

	reqDE := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "de-cleared", RequestedByPrincipalID: requester,
	})
	reqFR := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"FR"},
		ReasonCode: "fr-cleared", RequestedByPrincipalID: requester,
	})
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: reqDE.ID, Approve: true, ReasonCode: "ok", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approve DE request: %v", err)
	}
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: reqFR.ID, Approve: true, ReasonCode: "ok", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approve FR request: %v", err)
	}

	// Remove ONLY FR first - the OLDER DE request must be left alone and
	// its own approval must not be spent or invalidated.
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{"DE"}, Status: game.Status,
		})
		return err
	})
	if err != nil {
		t.Fatalf("removing FR via its own matching approval must succeed even though an older, unrelated DE request is also pending-approved: %v", err)
	}

	// The DE request must still be pending (untouched), and now applies
	// cleanly on its own.
	deState := changeRequestState(t, pool, requester, reqDE.ID)
	if deState != "pending" {
		t.Fatalf("expected the older DE request to remain 'pending' after the FR removal, got %q", deState)
	}

	err = pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{}, Status: game.Status,
		})
		return err
	})
	if err != nil {
		t.Fatalf("removing DE via its own separate approval afterwards must also succeed: %v", err)
	}

	if got := changeRequestState(t, pool, requester, reqDE.ID); got != "applied" {
		t.Fatalf("expected DE request to be 'applied', got %q", got)
	}
	if got := changeRequestState(t, pool, requester, reqFR.ID); got != "applied" {
		t.Fatalf("expected FR request to be 'applied', got %q", got)
	}
}

// TestApplyDualControl_TwoDistinctApprovedRequests_ApplyIndependently_DEFirst
// is the other order: applying the OLDER (DE) request first must not
// regress, and the newer (FR) request must still apply independently
// afterward.
func TestApplyDualControl_TwoDistinctApprovedRequests_ApplyIndependently_DEFirst(t *testing.T) {
	pool := testPool(t)
	game := seedGovernanceGame(t, pool, []string{"DE", "FR"}, GameStatusActive)
	requester := seedPlatformAdminStaffPrincipal(t, pool)
	approver := seedPlatformAdminStaffPrincipal(t, pool)

	reqDE := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"DE"},
		ReasonCode: "de-cleared", RequestedByPrincipalID: requester,
	})
	reqFR := fileGovernanceRequest(t, pool, FileChangeRequestParams{
		Operation: ChangeJurisdictionUnblock, GameID: game.ID, RemovedCodes: []string{"FR"},
		ReasonCode: "fr-cleared", RequestedByPrincipalID: requester,
	})
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: reqDE.ID, Approve: true, ReasonCode: "ok", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approve DE request: %v", err)
	}
	if _, err := decideGovernanceRequest(pool, DecideChangeRequestParams{
		RequestID: reqFR.ID, Approve: true, ReasonCode: "ok", ApproverPrincipalID: approver,
	}); err != nil {
		t.Fatalf("approve FR request: %v", err)
	}

	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{"FR"}, Status: game.Status,
		})
		return err
	})
	if err != nil {
		t.Fatalf("removing DE via its own matching approval must succeed: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			JurisdictionBlocklist: []string{}, Status: game.Status,
		})
		return err
	})
	if err != nil {
		t.Fatalf("removing FR via its own separate approval afterwards must also succeed: %v", err)
	}

	if got := changeRequestState(t, pool, requester, reqDE.ID); got != "applied" {
		t.Fatalf("expected DE request to be 'applied', got %q", got)
	}
	if got := changeRequestState(t, pool, requester, reqFR.ID); got != "applied" {
		t.Fatalf("expected FR request to be 'applied', got %q", got)
	}
}

// --- test helpers -----------------------------------------------------

// changeRequestState reads back a casino_catalogue_change_requests row's
// current state.
func changeRequestState(t *testing.T, pool *db.Pool, scopePrincipal uuid.UUID, requestID uuid.UUID) string {
	t.Helper()
	var state string
	err := pool.WithPlatformAdmin(context.Background(), scopePrincipal, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM casino_catalogue_change_requests WHERE id = $1`, requestID).Scan(&state)
	})
	if err != nil {
		t.Fatalf("read change request state: %v", err)
	}
	return state
}

func countApprovals(t *testing.T, pool *db.Pool, requestID uuid.UUID) int {
	t.Helper()
	var count int
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_catalogue_change_approvals WHERE request_id = $1`, requestID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	return count
}
