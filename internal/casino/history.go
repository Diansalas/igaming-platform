// Stage 7 §15/§16: minimum-visibility read model for a casino "round"
// (one casino_launch_sessions row plus whatever bet/win/rollback ledger
// transactions were posted against it). Read-only, additive - no existing
// table, orchestrator function, or financial flow is touched.
//
// ledger_transactions carries no player_account_id/game_id column at all
// (migration 0021 - see its own doc comment: a player never reads raw
// ledger_transactions directly, and the table is deliberately generic
// across every product). A casino round's ledger effects are instead
// correlated back to their originating session via correlation_id =
// roundCorrelationID(tenantID, providerID, roundID) exactly as postBet/
// postWin/postRollback already compute it (orchestrator.go) - this file
// reuses that SAME deterministic derivation, never a new one, and never
// requires roundID to be anything other than whatever the provider (real
// or mock) actually declares on its callback. It relies on casino_launch_
// sessions - not a new table - as the "one row per round" anchor, since
// that row already carries every identity/tenancy/brand/game/provider
// field this view needs and is already RLS-protected exactly like the
// financial tables it is being joined against.
package casino

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// RoundStatus summarizes a round's ledger state for display purposes only
// - never consulted by any financial code path.
type RoundStatus string

const (
	RoundStatusLaunched   RoundStatus = "launched"    // session exists, no bet ever posted (e.g. abandoned)
	RoundStatusWagered    RoundStatus = "wagered"     // bet posted, no win/rollback yet
	RoundStatusWon        RoundStatus = "won"         // bet + win posted, no rollback
	RoundStatusRolledBack RoundStatus = "rolled_back" // the bet and/or win has a rollback posted against it
)

// RoundSummary is one casino_launch_sessions row enriched with whatever
// casino_bet/casino_win/casino_rollback ledger transactions were posted
// under its round's correlation_id. Amount fields are nil when that leg
// never happened (e.g. WinAmount is nil for a round with no win yet).
type RoundSummary struct {
	SessionID            uuid.UUID
	TenantID             uuid.UUID
	BrandID              uuid.UUID
	PlayerAccountID      uuid.UUID
	WalletID             uuid.UUID
	GameID               uuid.UUID
	ProviderID           string
	ProviderGameID       string
	AssetCode            string
	Mode                 GameMode
	Status               RoundStatus
	LaunchedAt           time.Time
	BetAmount            *int64
	BetProviderTxID      string
	WinAmount            *int64
	WinProviderTxID      string
	RollbackAmount       *int64
	RollbackProviderTxID string
	// LedgerTransactionIDs is every ledger_transactions.id posted for this
	// round (bet/win/rollback, in posting order) - the audit-linkage field
	// Stage 7 §15 requires (a Back Office operator cross-references these
	// directly against audit_log, which already records each one under
	// target_type='ledger_transaction').
	LedgerTransactionIDs []uuid.UUID
}

// roundLedgerLeg is one casino_bet/casino_win/casino_rollback row found
// for a round's correlation_id, with its own net amount against the
// round's own wallet/asset (never another wallet's - see the WHERE clause
// below, which pins la.wallet_id/la.asset_code to the session's own
// values exactly like queryNetOutstandingLocked does).
type roundLedgerLeg struct {
	TransactionID   uuid.UUID
	TransactionType ledger.TransactionType
	ProviderTxID    string
	NetAmount       int64
}

// loadRoundLedgerLegs queries every casino_bet/casino_win/casino_rollback
// transaction posted under session's own round correlation_id, scoped to
// its own wallet/asset - the same correlation_id derivation postBet/
// postWin/postRollback already use (roundCorrelationID), applied here
// read-only. roundID is the provider-declared round identifier the
// session's own bet/win/rollback callbacks were posted under - Stage 7's
// new play-simulation endpoints (casino_play_handlers.go) always set it to
// the session's own id, but this function makes no such assumption itself
// (a real provider's own round id is whatever it declares).
func loadRoundLedgerLegs(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, roundID string, walletID uuid.UUID, assetCode string) ([]roundLedgerLeg, error) {
	correlationID := roundCorrelationID(tenantID, providerID, roundID)
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.transaction_type, t.provider_tx_id,
		       COALESCE(SUM(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END), 0) AS net_amount
		  FROM ledger_transactions t
		  JOIN ledger_entries      e  ON e.ledger_transaction_id = t.id
		  JOIN ledger_accounts     la ON la.id = e.ledger_account_id
		 WHERE t.tenant_id = $1
		   AND t.correlation_id = $2
		   AND t.transaction_type = ANY($3)
		   AND la.wallet_id = $4
		   AND la.asset_code = $5
		 GROUP BY t.id, t.transaction_type, t.provider_tx_id
		 ORDER BY t.created_at ASC`,
		tenantID, correlationID,
		[]string{string(ledger.TxCasinoBet), string(ledger.TxCasinoWin), string(ledger.TxCasinoRollback)},
		walletID, assetCode,
	)
	if err != nil {
		return nil, fmt.Errorf("casino: load round ledger legs: %w", err)
	}
	defer rows.Close()

	var out []roundLedgerLeg
	for rows.Next() {
		var leg roundLedgerLeg
		var txType string
		if err := rows.Scan(&leg.TransactionID, &txType, &leg.ProviderTxID, &leg.NetAmount); err != nil {
			return nil, fmt.Errorf("casino: scan round ledger leg: %w", err)
		}
		leg.TransactionType = ledger.TransactionType(txType)
		out = append(out, leg)
	}
	return out, rows.Err()
}

// summarizeRound turns a session row plus its own loaded ledger legs into
// a RoundSummary. The round's own id (session.ID.String()) is what Stage
// 7's new play-simulation endpoints always use as the provider round id -
// see casino_play_handlers.go's own doc comment for why that convention is
// safe to introduce without touching postBet/postWin/postRollback: RoundID
// is already a provider-declared, opaque string as far as the orchestrator
// is concerned, never interpreted as anything but a correlation key.
//
// Ledger-finance review finding: a round can legitimately carry more than
// one bet or win leg (a re-bet, free-spin retriggers, a multi-stage bonus
// game - docs/architecture/08-casino-integration-architecture.md's own
// "two independent rollback events" case for a round needing both its bet
// AND win reversed implies exactly this). BetAmount/WinAmount/
// RollbackAmount are therefore the SUM of every leg of that type, not the
// last one seen - silently overwriting would under-report a round's true
// stake/payout the instant a player interacts with a session more than
// once, which is the normal way this screen is used, not a corner case.
// ProviderTxID fields keep only the LAST leg's reference (a summary
// display field, not a ledger-accurate one) - every individual reference
// is still fully recoverable via LedgerTransactionIDs for audit purposes.
func summarizeRound(session LaunchSession, legs []roundLedgerLeg) RoundSummary {
	s := RoundSummary{
		SessionID: session.ID, TenantID: session.TenantID, BrandID: session.BrandID,
		PlayerAccountID: session.PlayerAccountID, WalletID: session.WalletID, GameID: session.GameID,
		ProviderID: session.ProviderID, ProviderGameID: session.ProviderGameID, AssetCode: session.AssetCode,
		Mode: session.Mode, Status: RoundStatusLaunched,
	}
	rolledBack := false
	for _, leg := range legs {
		s.LedgerTransactionIDs = append(s.LedgerTransactionIDs, leg.TransactionID)
		amt := leg.NetAmount
		if amt < 0 {
			amt = -amt
		}
		switch leg.TransactionType {
		case ledger.TxCasinoBet:
			s.BetAmount = addAmount(s.BetAmount, amt)
			s.BetProviderTxID = leg.ProviderTxID
			if s.Status == RoundStatusLaunched {
				s.Status = RoundStatusWagered
			}
		case ledger.TxCasinoWin:
			s.WinAmount = addAmount(s.WinAmount, amt)
			s.WinProviderTxID = leg.ProviderTxID
			s.Status = RoundStatusWon
		case ledger.TxCasinoRollback:
			s.RollbackAmount = addAmount(s.RollbackAmount, amt)
			s.RollbackProviderTxID = leg.ProviderTxID
			rolledBack = true
		}
	}
	if rolledBack {
		s.Status = RoundStatusRolledBack
	}
	return s
}

// addAmount accumulates amt into the running total held in existing
// (nil means "no leg of this type seen yet"), returning a freshly
// allocated pointer so callers never alias a caller-owned int64.
func addAmount(existing *int64, amt int64) *int64 {
	if existing == nil {
		total := amt
		return &total
	}
	total := *existing + amt
	return &total
}

// TransactionBelongsToRound reports whether a casino_bet/casino_win
// ledger transaction named by providerTxID was posted as part of THIS
// round (same tenant/provider/round-correlation) against THIS wallet.
//
// This is the authorization boundary Stage 7's play-simulation rollback
// endpoint (casino_play_handlers.go) MUST enforce before ever handing
// providerTxID to Orchestrator.ReceiveCallback: postRollback's own lookup
// (orchestrator.go) is intentionally scoped to (tenant_id, provider_id,
// provider_tx_id) ONLY, because for a real, signature-verified provider
// webhook that triple is fully provider-attested and sufficient. The
// play-simulation endpoint instead lets an AUTHENTICATED PLAYER supply
// providerTxID directly (the mock signature is self-issued on the
// player's behalf and authenticates nothing about which transaction they
// name) - three independent specialist reviews (architect, security,
// ledger-finance) confirmed that without this additional check, a player
// could reverse another player's bet/win, or any bet/win from a
// different round of their own, by naming its provider_tx_id. Scoping to
// this round's own correlation_id (not just this wallet) additionally
// keeps a rollback's own posting attributable to the SAME round it
// reverses, so the reversal is never invisible in the round it actually
// affected.
func TransactionBelongsToRound(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, roundID string, walletID uuid.UUID, providerTxID string) (bool, error) {
	correlationID := roundCorrelationID(tenantID, providerID, roundID)
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM ledger_transactions t
			  JOIN ledger_entries      e  ON e.ledger_transaction_id = t.id
			  JOIN ledger_accounts     la ON la.id = e.ledger_account_id
			 WHERE t.tenant_id = $1
			   AND t.provider_id = $2
			   AND t.provider_tx_id = $3
			   AND t.correlation_id = $4
			   AND t.transaction_type = ANY($5)
			   AND la.wallet_id = $6
		)`,
		tenantID, providerID, providerTxID, correlationID,
		[]string{string(ledger.TxCasinoBet), string(ledger.TxCasinoWin)}, walletID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("casino: check transaction belongs to round: %w", err)
	}
	return exists, nil
}

// sessionRow is casino_launch_sessions' own row shape for the history
// listing query - a distinct type from LaunchSession (rather than reusing
// it and overloading one of its fields) specifically so CreatedAt has its
// own honest field instead of being crammed into ExpiresAt, which means
// something else entirely on the real type.
type sessionRow struct {
	LaunchSession
	CreatedAt time.Time
}

// listSessionsPage is shared by ListRoundsForPlayer/ListRoundsForTenant -
// only the WHERE clause differs (player-scoped vs tenant-wide), mirroring
// how the two ListBetsForPlayer/ListBetsForTenant precedents in
// internal/sportsbook share everything but their own scoping predicate.
func listSessionsPage(ctx context.Context, tx pgx.Tx, whereClause string, args []any, limit, offset int) ([]sessionRow, int, error) {
	var total int
	countArgs := append([]any{}, args...)
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE `+whereClause, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("casino: count rounds: %w", err)
	}

	// ORDER BY created_at DESC, id DESC: created_at alone ties for every
	// row written in the same transaction (its default is the
	// transaction timestamp, not the statement timestamp), which would
	// otherwise make pagination non-deterministic - a row could appear on
	// two consecutive pages or vanish from both (database/RLS review
	// finding).
	pagedArgs := append(append([]any{}, args...), limit, offset)
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
		       asset_code, mode, created_at
		  FROM casino_launch_sessions
		 WHERE %s
		 ORDER BY created_at DESC, id DESC
		 LIMIT $%d OFFSET $%d`, whereClause, len(args)+1, len(args)+2), pagedArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("casino: list rounds: %w", err)
	}
	defer rows.Close()

	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		var mode string
		if err := rows.Scan(&s.ID, &s.TenantID, &s.BrandID, &s.PlayerAccountID, &s.WalletID, &s.GameID,
			&s.ProviderID, &s.ProviderGameID, &s.AssetCode, &mode, &s.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("casino: scan round session: %w", err)
		}
		s.Mode = GameMode(mode)
		out = append(out, s)
	}
	return out, total, rows.Err()
}

// ListRoundsForPlayer is the player's own casino round history (Stage 7
// §16's minimal B2C transaction/history view). Call under db.Pool.
// WithTenant, NOT WithPlayerScope: ledger_transactions carries no
// player-scope RLS policy at all - only ledger_entries/ledger_accounts/
// casino_launch_sessions have one (migration 0028 added the
// `AND app.player_account_id IS NULL` clause to ledger_transactions' own
// tenant_isolation policy specifically because it has no legitimate
// player-self-service use case at the time, per that migration's own
// comment) - so a player-scoped connection would see zero
// ledger_transactions rows and this view would report every round as
// permanently "launched" - mirrors newLaunchCasinoGameHandler's/
// newPlaceBetHandler's own WithTenant-not-WithPlayerScope rationale. The
// `player_account_id = $1` predicate in listSessionsPage is therefore THE
// actual authorization boundary here, not row-level security - the
// caller MUST pass the authenticated player's own id, never a
// client-supplied one.
func ListRoundsForPlayer(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID, limit, offset int) ([]RoundSummary, int, error) {
	sessions, total, err := listSessionsPage(ctx, tx, "player_account_id = $1", []any{playerAccountID}, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	summaries, err := summarizeSessions(ctx, tx, sessions)
	return summaries, total, err
}

// ListRoundsForTenant is the Back Office's tenant-wide casino round
// visibility (Stage 7 §15's minimum-visibility view) - staff-scoped
// (app.player_account_id unset), never player-restricted, exactly
// mirroring ListBetsForTenant's identical shape in internal/sportsbook:
// no explicit tenant_id predicate here either - casino_launch_sessions'
// own tenant_staff_scope RLS policy is what actually restricts this to
// the caller's tenant, under db.Pool.WithTenant.
func ListRoundsForTenant(ctx context.Context, tx pgx.Tx, limit, offset int) ([]RoundSummary, int, error) {
	sessions, total, err := listSessionsPage(ctx, tx, "true", nil, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	summaries, err := summarizeSessions(ctx, tx, sessions)
	return summaries, total, err
}

func summarizeSessions(ctx context.Context, tx pgx.Tx, sessions []sessionRow) ([]RoundSummary, error) {
	out := make([]RoundSummary, 0, len(sessions))
	for _, s := range sessions {
		legs, err := loadRoundLedgerLegs(ctx, tx, s.TenantID, s.ProviderID, s.ID.String(), s.WalletID, s.AssetCode)
		if err != nil {
			return nil, err
		}
		summary := summarizeRound(s.LaunchSession, legs)
		summary.LaunchedAt = s.CreatedAt
		out = append(out, summary)
	}
	return out, nil
}
