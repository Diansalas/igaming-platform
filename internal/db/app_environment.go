package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrEnvironmentAnchorMismatch: a production process is connected to a database whose
// database_environment anchor (migration 0135) says development or test.
var ErrEnvironmentAnchorMismatch = errors.New("db: refusing to start: production process on a database anchored as development/test")

// ErrEnvironmentAnchorUnrecorded: a production process is connected to a database with no
// database_environment row. The database reads that as production, but the launch gate (ADR 0115
// S1) requires the provisioning step to have RECORDED it, so a database that was never provisioned
// (or whose anchor was never written) cannot serve production.
var ErrEnvironmentAnchorUnrecorded = errors.New("db: refusing to start: production process on a database whose database_environment anchor is not recorded; " +
	"as the migration-owner role run once: INSERT INTO database_environment (environment) VALUES ('production'); " +
	"(docs/runbooks/production-configuration-checklist.md, \"Database environment anchor\")")

// environmentAnchorReader reads the database_environment anchor ("" when no row is recorded).
type environmentAnchorReader interface {
	ReadEnvironmentAnchor(ctx context.Context) (string, error)
}

// ReadEnvironmentAnchor returns the recorded database_environment value, or "" when none is
// recorded (which the database treats as production).
func (p *Pool) ReadEnvironmentAnchor(ctx context.Context) (string, error) {
	var env string
	err := p.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT environment FROM database_environment`).Scan(&env)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: read database_environment: %w", err)
	}
	return env, nil
}

// VerifyEnvironmentAnchor (ADR 0115 S1, security LOW) refuses a PRODUCTION start (the guarded
// environment, config.GuardEnvironment) against a database anchored as development or test, and
// REFUSES an unrecorded anchor (ErrEnvironmentAnchorUnrecorded; launch gate), and fails closed if
// the anchor cannot be read in production. Outside production it checks nothing (no new friction
// for dev / CI / test). It returns the anchor it read.
func VerifyEnvironmentAnchor(ctx context.Context, guardEnvironment string, r environmentAnchorReader) (string, error) {
	if guardEnvironment != "production" {
		return "", nil
	}
	anchor, err := r.ReadEnvironmentAnchor(ctx)
	if err != nil {
		return "", err
	}
	if anchor == "development" || anchor == "test" {
		return anchor, fmt.Errorf("%w (anchor=%q)", ErrEnvironmentAnchorMismatch, anchor)
	}
	if anchor == "" {
		return anchor, ErrEnvironmentAnchorUnrecorded
	}
	return anchor, nil
}

// dropURLAppEnvironment removes every DATABASE_URL spelling of app.environment (setting names are
// case-insensitive) and refuses an `options` startup parameter that names it: the value always
// comes from the application configuration (config.GuardEnvironment, ADR 0115 S1), never from a
// connection string. This is hygiene, not the security control: any session can still set_config()
// the setting, which is why the database's synthetic-configuration guard also requires the
// owner-written database_environment anchor (migration 0135).
func dropURLAppEnvironment(rp map[string]string) error {
	for k, v := range rp {
		lk := strings.ToLower(k)
		if lk == "options" {
			if strings.Contains(strings.ToLower(v), appEnvironmentParam) {
				return fmt.Errorf("db: database url sets %s through the options parameter; it is set from the application configuration only", appEnvironmentParam)
			}
			continue
		}
		if lk == appEnvironmentParam {
			delete(rp, k)
		}
	}
	return nil
}
