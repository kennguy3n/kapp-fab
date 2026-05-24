package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// AWSProvider resolves secrets from AWS Secrets Manager. It is
// the recommended production backend for deployments running on
// AWS infrastructure: every secret is encrypted with KMS at
// rest, the SDK auto-rotates STS credentials when running under
// an EC2 / EKS instance profile, and the IAM-resource model
// composes cleanly with Kapp's per-tenant scoping (one IAM role
// per Kapp deployment, scoped to read only the kapp/* prefix).
//
// # Key-to-ARN mapping
//
// Operator-supplied keys map to AWS secret-id values in one of
// two ways depending on Prefix:
//
//   - Prefix is empty: key is passed through verbatim, so an
//     operator who already manages secrets under arn:aws:...
//     paths can request them by full ARN.
//   - Prefix is non-empty: key is suffix-joined to the prefix
//     with "/" so a request for "jwt/primary" against prefix
//     "kapp/" resolves to "kapp/jwt/primary".
//
// # Versioning
//
// SecretValue.Version is the AWS VersionId from
// GetSecretValueOutput. Distinct upstream rotations always
// produce distinct VersionIds; AWSCURRENT is followed by
// default (omitting VersionStage in the request).
type AWSProvider struct {
	client   AWSSecretsClient
	prefix   string
	region   string
	endpoint string
}

// AWSSecretsClient is the subset of the secretsmanager SDK
// client we actually use. Defined here so unit tests can swap
// in a fake without depending on the real SDK and so a future
// caller can wrap the real client (e.g. with retry / circuit
// breaker) without changing this package.
type AWSSecretsClient interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// AWSProviderConfig is the construction-time configuration.
// Region is mandatory (the SDK refuses to dispatch without one);
// Endpoint is only used for LocalStack-style test setups and
// must be empty in production. Prefix is optional but
// recommended: it lets the operator namespace Kapp's secrets
// under a dedicated path so the IAM resource scope (kapp/*)
// stays a tight one-liner.
type AWSProviderConfig struct {
	Region   string
	Endpoint string
	Prefix   string
}

// NewAWSProvider returns an AWSProvider that talks to AWS
// Secrets Manager via the standard aws-sdk-go-v2 config chain
// (env vars, instance metadata, ~/.aws/credentials, EKS
// IRSA, etc.). Returns ErrProviderNotConfigured when Region is
// empty so the boot fails loudly rather than dispatching
// blindly to us-east-1.
func NewAWSProvider(ctx context.Context, cfg AWSProviderConfig) (*AWSProvider, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("%w: aws provider region empty", ErrProviderNotConfigured)
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("secrets: load aws config: %w", err)
	}
	clientOpts := []func(*secretsmanager.Options){}
	if cfg.Endpoint != "" {
		// LocalStack / testing path. Production deployments
		// MUST NOT set this; an Endpoint override on a real
		// account would silently route requests to the wrong
		// host.
		ep := cfg.Endpoint
		clientOpts = append(clientOpts, func(o *secretsmanager.Options) {
			o.BaseEndpoint = &ep
		})
	}
	client := secretsmanager.NewFromConfig(awsCfg, clientOpts...)
	return &AWSProvider{
		client:   client,
		prefix:   strings.TrimRight(cfg.Prefix, "/"),
		region:   cfg.Region,
		endpoint: cfg.Endpoint,
	}, nil
}

// Name returns the literal "aws".
func (*AWSProvider) Name() string { return "aws" }

// GetSecret fetches the current value of the named secret from
// AWS Secrets Manager. Returns ErrSecretNotFound for the
// ResourceNotFoundException response, ErrProviderUnavailable
// for transient errors (throttling, network), and a wrapped
// error for everything else (permission denied, schema
// mismatch).
func (p *AWSProvider) GetSecret(ctx context.Context, key string) (SecretValue, error) {
	secretID := p.secretID(key)
	out, err := p.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &secretID,
	})
	if err != nil {
		var notFound *smtypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return SecretValue{}, fmt.Errorf("%w: aws secret %s missing", ErrSecretNotFound, secretID)
		}
		var throttling *smtypes.LimitExceededException
		if errors.As(err, &throttling) {
			return SecretValue{}, fmt.Errorf("%w: aws throttled on %s: %w", ErrProviderUnavailable, secretID, err)
		}
		return SecretValue{}, fmt.Errorf("secrets: aws GetSecretValue %s: %w", secretID, err)
	}
	if out == nil || (out.SecretString == nil && len(out.SecretBinary) == 0) {
		return SecretValue{}, fmt.Errorf("%w: aws secret %s has no value", ErrSecretNotFound, secretID)
	}
	var raw []byte
	switch {
	case out.SecretString != nil:
		raw = []byte(*out.SecretString)
	default:
		raw = out.SecretBinary
	}
	version := ""
	if out.VersionId != nil {
		version = *out.VersionId
	}
	return SecretValue{Bytes: raw, Version: version}, nil
}

// secretID composes the prefix (when set) with the
// operator-supplied key. Keys that already start with "arn:"
// are passed through verbatim so a deployment that pins
// specific ARNs in config can do so even when a prefix is
// configured for the rest.
func (p *AWSProvider) secretID(key string) string {
	if strings.HasPrefix(key, "arn:") {
		return key
	}
	if p.prefix == "" {
		return key
	}
	return p.prefix + "/" + strings.TrimLeft(key, "/")
}
