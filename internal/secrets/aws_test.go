package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

type fakeAWSClient struct {
	getFunc func(ctx context.Context, in *secretsmanager.GetSecretValueInput) (*secretsmanager.GetSecretValueOutput, error)
}

func (f *fakeAWSClient) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return f.getFunc(ctx, in)
}

func TestAWSProvider_GetSecret_Success(t *testing.T) {
	secret := "supersecret"
	versionID := "v1abc"
	client := &fakeAWSClient{
		getFunc: func(_ context.Context, in *secretsmanager.GetSecretValueInput) (*secretsmanager.GetSecretValueOutput, error) {
			if in.SecretId == nil || *in.SecretId != "kapp/jwt/primary" {
				t.Errorf("got SecretId %v want kapp/jwt/primary", in.SecretId)
			}
			return &secretsmanager.GetSecretValueOutput{
				SecretString: &secret,
				VersionId:    &versionID,
			}, nil
		},
	}
	p := &AWSProvider{client: client, prefix: "kapp"}
	v, err := p.GetSecret(context.Background(), "jwt/primary")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(v.Bytes) != "supersecret" {
		t.Fatalf("got %q want supersecret", string(v.Bytes))
	}
	if v.Version != "v1abc" {
		t.Fatalf("got version %q want v1abc", v.Version)
	}
}

func TestAWSProvider_GetSecret_NotFound(t *testing.T) {
	client := &fakeAWSClient{
		getFunc: func(_ context.Context, _ *secretsmanager.GetSecretValueInput) (*secretsmanager.GetSecretValueOutput, error) {
			return nil, &smtypes.ResourceNotFoundException{}
		},
	}
	p := &AWSProvider{client: client}
	_, err := p.GetSecret(context.Background(), "missing")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}
}

func TestAWSProvider_GetSecret_Throttled(t *testing.T) {
	client := &fakeAWSClient{
		getFunc: func(_ context.Context, _ *secretsmanager.GetSecretValueInput) (*secretsmanager.GetSecretValueOutput, error) {
			return nil, &smtypes.LimitExceededException{}
		},
	}
	p := &AWSProvider{client: client}
	_, err := p.GetSecret(context.Background(), "x")
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for throttle, got %v", err)
	}
}

func TestAWSProvider_GetSecret_BinarySecret(t *testing.T) {
	binary := []byte{0x01, 0x02, 0x03}
	versionID := "vbin"
	client := &fakeAWSClient{
		getFunc: func(_ context.Context, _ *secretsmanager.GetSecretValueInput) (*secretsmanager.GetSecretValueOutput, error) {
			return &secretsmanager.GetSecretValueOutput{
				SecretBinary: binary,
				VersionId:    &versionID,
			}, nil
		},
	}
	p := &AWSProvider{client: client}
	v, err := p.GetSecret(context.Background(), "x")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if len(v.Bytes) != 3 || v.Bytes[0] != 0x01 {
		t.Fatalf("binary bytes not threaded through: %v", v.Bytes)
	}
}

func TestAWSProvider_SecretID_ARNPassthrough(t *testing.T) {
	p := &AWSProvider{prefix: "kapp"}
	got := p.secretID("arn:aws:secretsmanager:eu-west-1:1234:secret:custom/jwt-abc")
	if got != "arn:aws:secretsmanager:eu-west-1:1234:secret:custom/jwt-abc" {
		t.Fatalf("ARN not passed through verbatim: %s", got)
	}
}

func TestAWSProvider_SecretID_PrefixComposition(t *testing.T) {
	p := &AWSProvider{prefix: "kapp"}
	got := p.secretID("jwt/primary")
	if got != "kapp/jwt/primary" {
		t.Fatalf("got %q want kapp/jwt/primary", got)
	}
}

func TestAWSProvider_RejectsEmptyRegion(t *testing.T) {
	_, err := NewAWSProvider(context.Background(), AWSProviderConfig{Region: ""})
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured, got %v", err)
	}
}
