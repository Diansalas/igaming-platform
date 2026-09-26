package awssm

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/Diansalas/igaming-platform/internal/config"
)

// NewWithSDKFake is TEST SUPPORT ONLY: New with the real Secrets Manager
// client replaced by an in-process SDK fake that answers every
// GetSecretValue with ResourceNotFoundException (so Get always fails
// closed as not_found). It runs exactly New's preflight - the environment
// allow-list, the explicit region, the static-credential refusal, the
// endpoint/CA/trust-root and credential-source refusals, and the ECS
// container-credential endpoint requirement - so a caller outside this package (the
// cmd/platform-api wiring tests) exercises every refusal New applies, with
// no network and no AWS SDK import of its own (the AWS SDK stays confined
// to this package, C12.2).
//
// Its only permitted callers are _test.go files;
// TestNewWithSDKFake_OnlyFromTests enforces that with an AST check. It
// serves no secret, so even a misuse would fail closed.
//
// CODE-HYGIENE-10.3-1 item 5 (N-A): this stays in a regular (non-_test.go)
// file, not behind a build tag or an export_test.go seam, on purpose.
// cmd/platform-api/awssm_wiring_test.go (a different package, with no
// build tag of its own - it runs in plain `go test ./...`) is a real
// caller and needs an ordinary importable symbol: Go does not compile a
// package's _test.go files into what another package can import, and
// gating this file behind e.g. "//go:build integration" would force that
// consuming test behind the same tag too, turning an ordinary fast unit
// test that exists specifically to avoid needing real AWS infrastructure
// into something that only runs under -tags=integration. Both moves are
// net negatives for what is already a fail-closed, AST-guarded fake (see
// the doc comment above) - see gate 10.3-W2/W3's code re-verification (N-A)
// for the residual risk accepted here.
func NewWithSDKFake(_ context.Context, cfg config.Config, region string) (*Store, error) {
	if _, err := preflight(cfg, region); err != nil {
		return nil, err
	}
	return newStore(notFoundClient{}), nil
}

type notFoundClient struct{}

func (notFoundClient) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return nil, &smtypes.ResourceNotFoundException{Message: aws.String("sdk fake")}
}
