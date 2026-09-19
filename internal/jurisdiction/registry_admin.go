package jurisdiction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// This file implements item B-1 (canonical-model §11.1, RECON P-11/§5):
// the admin write surface for `jurisdictions` and `licences`. Before this
// file, neither table had ANY application write path - rows could only
// be created by direct database access, which meant no FK to
// `jurisdictions` could ever be satisfied by anything other than a
// manual insert. This is a HARD PREREQUISITE for the resolver (B-3/B-4):
// the resolver can only ever return a jurisdiction code that already
// exists as a row here.
//
// `jurisdictions`/`licences` are platform-scoped reference data with NO
// row-level security (canonical-model §6.1: "jurisdictions stays
// platform-scoped with no RLS... It is a platform fact and an FK target
// every tenant-scoped transaction must read"). The ONLY control on the
// write side is the new platform-only permission gating the HTTP layer
// (auth.PermJurisdictionRegistryManage, granted only to
// RolePlatformAdmin - the exact precedent of PermCasinoCatalogueManage/
// PermAssetRegistryManage). Every write is audited in the same
// transaction regardless (CLAUDE.md's audit rule has no carve-out for
// "the table has no RLS").
//
// ErrNotFound mirrors assetregistry's identical package-local sentinel
// convention.
var ErrNotFound = errors.New("jurisdiction: not found")

// ActorContext is the audit-trail context every mutating registry
// operation carries - mirrors assetregistry.ActorContext exactly.
type ActorContext struct {
	ActorID    uuid.UUID
	IPAddress  string
	UserAgent  string
	RequestID  string
	ReasonCode string
}

func (a ActorContext) validate() error {
	if a.ActorID == uuid.Nil {
		return fmt.Errorf("%w: actor id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(a.ReasonCode) == "" {
		return fmt.Errorf("%w: reason_code is required on every mutating jurisdiction-registry operation", ErrInvalidInput)
	}
	return nil
}

// Jurisdiction mirrors the `jurisdictions` row (migration 0002).
type Jurisdiction struct {
	ID             uuid.UUID
	Code           string
	Name           string
	RegulatoryBody string
	Notes          string
	CreatedAt      time.Time
}

// CreateJurisdictionParams is CreateJurisdiction's input.
type CreateJurisdictionParams struct {
	Code           string
	Name           string
	RegulatoryBody string
	Notes          string
	Actor          ActorContext
}

// CreateJurisdiction inserts a `jurisdictions` row. tx must be a
// platform-scoped transaction (db.Pool.WithPlatformAdmin) - there is no
// RLS on this table today, so this is a convention this package's own
// callers (the HTTP handlers, same commit) must follow, not something
// the database itself can yet enforce; see this file's own header
// comment for why that is the canonical model's own, deliberate posture
// for this specific table.
func CreateJurisdiction(ctx context.Context, tx pgx.Tx, p CreateJurisdictionParams) (Jurisdiction, error) {
	if err := p.Actor.validate(); err != nil {
		return Jurisdiction{}, err
	}
	code := strings.TrimSpace(p.Code)
	name := strings.TrimSpace(p.Name)
	if code == "" || name == "" {
		return Jurisdiction{}, fmt.Errorf("%w: code and name are required", ErrInvalidInput)
	}

	var j Jurisdiction
	err := tx.QueryRow(ctx, `
		INSERT INTO jurisdictions (code, name, regulatory_body, notes)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''))
		RETURNING id, code, name, COALESCE(regulatory_body, ''), COALESCE(notes, ''), created_at`,
		code, name, p.RegulatoryBody, p.Notes,
	).Scan(&j.ID, &j.Code, &j.Name, &j.RegulatoryBody, &j.Notes, &j.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Jurisdiction{}, fmt.Errorf("%w: jurisdiction code %q already exists", ErrInvalidInput, code)
		}
		return Jurisdiction{}, fmt.Errorf("jurisdiction: insert jurisdictions row: %w", err)
	}

	if err := recordRegistryAudit(ctx, tx, p.Actor, "jurisdiction_registry.jurisdiction_created", "jurisdiction", j.ID.String(), map[string]any{
		"before": nil, "after": jurisdictionState(j),
	}); err != nil {
		return Jurisdiction{}, err
	}
	return j, nil
}

// ListJurisdictions returns every jurisdiction row, platform-wide.
func ListJurisdictions(ctx context.Context, tx pgx.Tx) ([]Jurisdiction, error) {
	rows, err := tx.Query(ctx, `SELECT id, code, name, COALESCE(regulatory_body, ''), COALESCE(notes, ''), created_at FROM jurisdictions ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: list jurisdictions: %w", err)
	}
	defer rows.Close()
	var out []Jurisdiction
	for rows.Next() {
		var j Jurisdiction
		if err := rows.Scan(&j.ID, &j.Code, &j.Name, &j.RegulatoryBody, &j.Notes, &j.CreatedAt); err != nil {
			return nil, fmt.Errorf("jurisdiction: scan jurisdiction row: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Licence mirrors the `licences` row (migration 0002).
type Licence struct {
	ID                uuid.UUID
	JurisdictionID    uuid.UUID
	Licensee          string
	LicenceNumber     string
	Status            string
	PermittedProducts []string
	PermittedMarkets  []string
	CreatedAt         time.Time
}

// CreateLicenceParams is CreateLicence's input. PermittedProducts/
// PermittedMarkets are accepted so an operator can record a licence's
// real scope from day one (licences.permitted_products/permitted_markets,
// migration 0002) - this package does not itself VALIDATE a resolution
// against them (canonical-model §10.2: "validating a resolved
// jurisdiction against licences.permitted_markets is an AUTHORIZATION
// check... not a Risk rule" - a later phase's job, blocked on HDR-J-6).
type CreateLicenceParams struct {
	JurisdictionID    uuid.UUID
	Licensee          string
	LicenceNumber     string
	PermittedProducts []string
	PermittedMarkets  []string
	Actor             ActorContext
}

var validLicensees = map[string]bool{"platform": true, "tenant": true}

// CreateLicence inserts a `licences` row. Status always starts 'active'
// (migration 0002's own default) - this file deliberately does not
// expose a status-transition operation: no Stage 4I consumer needs one,
// and CLAUDE.md's no-uncontrolled-scope rule applies to admin surfaces
// too (a suspend/expire operation is a legitimate future addition should
// a real consumer need it).
func CreateLicence(ctx context.Context, tx pgx.Tx, p CreateLicenceParams) (Licence, error) {
	if err := p.Actor.validate(); err != nil {
		return Licence{}, err
	}
	if p.JurisdictionID == uuid.Nil {
		return Licence{}, fmt.Errorf("%w: jurisdiction_id is required", ErrInvalidInput)
	}
	if !validLicensees[p.Licensee] {
		return Licence{}, fmt.Errorf("%w: licensee must be 'platform' or 'tenant'", ErrInvalidInput)
	}
	if strings.TrimSpace(p.LicenceNumber) == "" {
		return Licence{}, fmt.Errorf("%w: licence_number is required", ErrInvalidInput)
	}

	products := p.PermittedProducts
	if products == nil {
		products = []string{}
	}
	markets := p.PermittedMarkets
	if markets == nil {
		markets = []string{}
	}
	productsJSON, err := json.Marshal(products)
	if err != nil {
		return Licence{}, fmt.Errorf("jurisdiction: marshal permitted_products: %w", err)
	}
	marketsJSON, err := json.Marshal(markets)
	if err != nil {
		return Licence{}, fmt.Errorf("jurisdiction: marshal permitted_markets: %w", err)
	}

	var l Licence
	err = tx.QueryRow(ctx, `
		INSERT INTO licences (jurisdiction_id, licensee, licence_number, permitted_products, permitted_markets)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, jurisdiction_id, licensee, licence_number, status, created_at`,
		p.JurisdictionID, p.Licensee, p.LicenceNumber, productsJSON, marketsJSON,
	).Scan(&l.ID, &l.JurisdictionID, &l.Licensee, &l.LicenceNumber, &l.Status, &l.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return Licence{}, fmt.Errorf("%w: unknown jurisdiction_id %s", ErrInvalidInput, p.JurisdictionID)
		}
		return Licence{}, fmt.Errorf("jurisdiction: insert licences row: %w", err)
	}
	l.PermittedProducts = products
	l.PermittedMarkets = markets

	if err := recordRegistryAudit(ctx, tx, p.Actor, "jurisdiction_registry.licence_created", "licence", l.ID.String(), map[string]any{
		"before": nil, "after": licenceState(l),
	}); err != nil {
		return Licence{}, err
	}
	return l, nil
}

// ListLicences returns every licence row, platform-wide.
func ListLicences(ctx context.Context, tx pgx.Tx) ([]Licence, error) {
	rows, err := tx.Query(ctx, `SELECT id, jurisdiction_id, licensee, licence_number, status, permitted_products, permitted_markets, created_at FROM licences ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: list licences: %w", err)
	}
	defer rows.Close()
	var out []Licence
	for rows.Next() {
		var l Licence
		var productsJSON, marketsJSON []byte
		if err := rows.Scan(&l.ID, &l.JurisdictionID, &l.Licensee, &l.LicenceNumber, &l.Status, &productsJSON, &marketsJSON, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("jurisdiction: scan licence row: %w", err)
		}
		if err := json.Unmarshal(productsJSON, &l.PermittedProducts); err != nil {
			return nil, fmt.Errorf("jurisdiction: unmarshal permitted_products: %w", err)
		}
		if err := json.Unmarshal(marketsJSON, &l.PermittedMarkets); err != nil {
			return nil, fmt.Errorf("jurisdiction: unmarshal permitted_markets: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func jurisdictionState(j Jurisdiction) map[string]any {
	return map[string]any{"id": j.ID.String(), "code": j.Code, "name": j.Name, "regulatory_body": j.RegulatoryBody}
}

func licenceState(l Licence) map[string]any {
	return map[string]any{
		"id": l.ID.String(), "jurisdiction_id": l.JurisdictionID.String(), "licensee": l.Licensee,
		"licence_number": l.LicenceNumber, "status": l.Status,
		"permitted_products": l.PermittedProducts, "permitted_markets": l.PermittedMarkets,
	}
}

// recordRegistryAudit writes the mandatory audit record for a
// PLATFORM-scoped mutation (tenant_id NULL - audit_log's dual-scope RLS,
// ADR 0013), in the SAME transaction as the mutation itself.
func recordRegistryAudit(ctx context.Context, tx pgx.Tx, actor ActorContext, action, targetType, targetID string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["reason_code"] = actor.ReasonCode
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: uuid.Nil, ActorType: audit.ActorStaff, ActorID: actor.ActorID,
		Action: action, TargetType: targetType, TargetID: targetID,
		Outcome: audit.OutcomeSuccess, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent,
		RequestID: actor.RequestID, Metadata: metadata,
	}); err != nil {
		return fmt.Errorf("jurisdiction: audit %s: %w", action, err)
	}
	return nil
}
