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
