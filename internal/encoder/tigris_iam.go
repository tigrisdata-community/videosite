package encoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// TigrisIAM wraps the AWS IAM SDK pointed at Tigris's IAM endpoint. Tigris
// doesn't have IAM Users in the AWS sense — policies attach to access keys,
// and the SDK's UserName field is reused as the access-key-id. Tigris also
// does NOT support inline policies (PutUserPolicy returns 501), so the
// flow is CreatePolicy → AttachUserPolicy and the policy ARN has to be
// tracked for cleanup.
type TigrisIAM struct {
	client *iam.Client
	bucket string
}

type TigrisIAMConfig struct {
	Endpoint    string // e.g. https://iam.storage.dev
	AccessKeyID string // root creds used to mint per-job keys
	SecretKey   string
	Bucket      string
}

func NewTigrisIAM(ctx context.Context, cfg TigrisIAMConfig) (*TigrisIAM, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("encoder/iam: Endpoint is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretKey == "" {
		return nil, errors.New("encoder/iam: AccessKeyID and SecretKey are required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("encoder/iam: Bucket is required")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("encoder/iam: load aws config: %w", err)
	}
	c := iam.NewFromConfig(awsCfg, func(o *iam.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
	})
	return &TigrisIAM{client: c, bucket: cfg.Bucket}, nil
}

type ScopedKey struct {
	AccessKeyID string
	SecretKey   string
	PolicyARN   string
}

// CreateScopedKey mints a fresh access key, creates a managed policy that
// allows GetObject on exactly sourceKey and PutObject under destPrefix, and
// attaches the policy to the key. destPrefix should end with a slash. If
// any step fails the partial work is best-effort rolled back.
func (t *TigrisIAM) CreateScopedKey(ctx context.Context, sourceKey, destPrefix string) (*ScopedKey, error) {
	if !strings.HasSuffix(destPrefix, "/") {
		destPrefix += "/"
	}

	out, err := t.client.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{})
	if err != nil {
		return nil, fmt.Errorf("encoder/iam: create access key: %w", err)
	}
	keyID := aws.ToString(out.AccessKey.AccessKeyId)
	secret := aws.ToString(out.AccessKey.SecretAccessKey)

	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Effect":   "Allow",
				"Action":   []string{"s3:GetObject"},
				"Resource": fmt.Sprintf("arn:aws:s3:::%s/%s", t.bucket, sourceKey),
			},
			{
				"Effect":   "Allow",
				"Action":   []string{"s3:PutObject", "s3:AbortMultipartUpload"},
				"Resource": fmt.Sprintf("arn:aws:s3:::%s/%s*", t.bucket, destPrefix),
			},
		},
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("encoder/iam: marshal policy: %w", err)
	}

	// Policy name is keyed off the access key id so it's unique and so
	// orphaned policies can be traced back to a specific job.
	policyName := "videosite-encoder-" + keyID
	createOut, err := t.client.CreatePolicy(ctx, &iam.CreatePolicyInput{
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(string(policyJSON)),
	})
	if err != nil {
		t.deleteAccessKey(ctx, keyID)
		return nil, fmt.Errorf("encoder/iam: create policy: %w", err)
	}
	policyARN := aws.ToString(createOut.Policy.Arn)

	_, err = t.client.AttachUserPolicy(ctx, &iam.AttachUserPolicyInput{
		UserName:  aws.String(keyID),
		PolicyArn: aws.String(policyARN),
	})
	if err != nil {
		t.deletePolicy(ctx, policyARN)
		t.deleteAccessKey(ctx, keyID)
		return nil, fmt.Errorf("encoder/iam: attach policy: %w", err)
	}

	return &ScopedKey{AccessKeyID: keyID, SecretKey: secret, PolicyARN: policyARN}, nil
}

// DeleteScopedKey tears down everything CreateScopedKey created. Each step
// is idempotent and best-effort — already-gone resources are not errors.
func (t *TigrisIAM) DeleteScopedKey(ctx context.Context, accessKeyID, policyARN string) error {
	var firstErr error
	if accessKeyID != "" && policyARN != "" {
		_, err := t.client.DetachUserPolicy(ctx, &iam.DetachUserPolicyInput{
			UserName:  aws.String(accessKeyID),
			PolicyArn: aws.String(policyARN),
		})
		if err != nil && !isNoSuchEntity(err) {
			firstErr = fmt.Errorf("detach policy: %w", err)
		}
	}
	if accessKeyID != "" {
		if err := t.deleteAccessKey(ctx, accessKeyID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if policyARN != "" {
		if err := t.deletePolicy(ctx, policyARN); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t *TigrisIAM) deleteAccessKey(ctx context.Context, keyID string) error {
	_, err := t.client.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
		AccessKeyId: aws.String(keyID),
		UserName:    aws.String(keyID),
	})
	if err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("encoder/iam: delete access key %q: %w", keyID, err)
	}
	return nil
}

func (t *TigrisIAM) deletePolicy(ctx context.Context, arn string) error {
	_, err := t.client.DeletePolicy(ctx, &iam.DeletePolicyInput{
		PolicyArn: aws.String(arn),
	})
	if err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("encoder/iam: delete policy %q: %w", arn, err)
	}
	return nil
}

func isNoSuchEntity(err error) bool {
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchEntity" {
		return true
	}
	return false
}
