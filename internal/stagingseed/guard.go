// Package stagingseed holds the shared gating and the reviewed write helpers of the STAGING-PREP-3 seeding commands
// (cmd/seed-market, cmd/seed-staging-world, the runner's record-anchor mode). These commands write SYNTHETIC
// configuration into a development or staging database only, so every one of them applies the same two gates BEFORE
// it reads or writes anything else (security review SRP3-1):
//
//   - APP_ENV must be "development" or "staging" (self-attested, hygiene only);
//   - the database_environment anchor (migration 0135, written by the owner, write-once) must be "staging",
//     "development" or "test". "No row" means PRODUCTION to the database, so an absent anchor is REFUSED.
//     The anchor is the binding control: APP_ENV is forgeable by whoever runs the binary.
package stagingseed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxField bounds every free-text input recorded in an audit row.
const MaxField = 200

// AnchorReader is satisfied by *db.Pool.
type AnchorReader interface {
	ReadEnvironmentAnchor(ctx context.Context) (string, error)
}

// CheckAppEnv refuses everything but development and staging (production is named in the error).
func CheckAppEnv(appEnv string) error {
	switch strings.TrimSpace(appEnv) {
	case "development", "staging":
		return nil
	case "production":
		return errors.New("refused: APP_ENV=production. This tool writes synthetic development/staging configuration only; production configuration is a reviewed, human-authorised change")
	default:
		return fmt.Errorf("refused: APP_ENV must be development or staging (got %q)", strings.TrimSpace(appEnv))
	}
}

// RequireSyntheticAnchor reads the database_environment anchor and refuses unless it is staging, development or test.
// An absent anchor (the database's definition of production), "production" and any unreadable anchor are all refused.
// It returns the anchor it read.
func RequireSyntheticAnchor(ctx context.Context, r AnchorReader) (string, error) {
	anchor, err := r.ReadEnvironmentAnchor(ctx)
	if err != nil {
		return "", fmt.Errorf("refused: cannot read the database_environment anchor: %w", err)
	}
	switch anchor {
	case "staging", "development", "test":
		return anchor, nil
	case "":
		return "", errors.New("refused: the database has no database_environment anchor, which the database treats as PRODUCTION. Record 'staging' first (runner mode record-anchor, runbook step 6.2)")
	default:
		return anchor, fmt.Errorf("refused: the database_environment anchor is %q, not staging, development or test", anchor)
	}
}

// CheckField mirrors cmd/seed-admin's operator/reason hygiene: required, bounded, no control, format (bidi / zero-width)
// or line-separator characters, no surrounding whitespace, so an audit row cannot visually misrepresent who ran the job or why.
func CheckField(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(v) > MaxField || strings.TrimSpace(v) != v || !utf8.ValidString(v) || strings.IndexFunc(v, unsafeRune) >= 0 {
		return fmt.Errorf("%s must be 1-%d bytes of UTF-8 without control, format or line-separator characters or surrounding whitespace", name, MaxField)
	}
	return nil
}

func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.In(r, unicode.Zl, unicode.Zp)
}
