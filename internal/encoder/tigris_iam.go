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

// TigrisIAM is a thin wrapper around the AWS IAM SDK pointed at Tigris's IAM
// endpoint. Tigris doesn't have IAM Users in the AWS sense — policies attach
// directly to an access key, and the SDK's UserName field is reused as the
// access-key-id when calling PutUserPolicy / DeleteAccessKey.
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
}

// CreateScopedKey mints a fresh access key and attaches an inline policy that
// allows GetObject on exactly sourceKey and PutObject under destPrefix.
// destPrefix should end with a slash.
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

	_, err = t.client.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
		UserName:       aws.String(keyID),
		PolicyName:     aws.String("videosite-encoder-job"),
		PolicyDocument: aws.String(string(policyJSON)),
	})
	if err != nil {
		// Best-effort cleanup of the orphaned key.
		_, _ = t.client.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
			AccessKeyId: aws.String(keyID),
			UserName:    aws.String(keyID),
		})
		return nil, fmt.Errorf("encoder/iam: put user policy: %w", err)
	}

	return &ScopedKey{AccessKeyID: keyID, SecretKey: secret}, nil
}

// DeleteKey removes the key. Idempotent — already-gone keys aren't an error.
func (t *TigrisIAM) DeleteKey(ctx context.Context, accessKeyID string) error {
	if accessKeyID == "" {
		return nil
	}
	// Drop the inline policy first so the key truly stops working even if
	// DeleteAccessKey 404s for some reason.
	_, _ = t.client.DeleteUserPolicy(ctx, &iam.DeleteUserPolicyInput{
		UserName:   aws.String(accessKeyID),
		PolicyName: aws.String("videosite-encoder-job"),
	})
	_, err := t.client.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
		AccessKeyId: aws.String(accessKeyID),
		UserName:    aws.String(accessKeyID),
	})
	if err != nil {
		// Treat NoSuchEntity as already-gone.
		var nse interface{ ErrorCode() string }
		if errors.As(err, &nse) && nse.ErrorCode() == "NoSuchEntity" {
			return nil
		}
		return fmt.Errorf("encoder/iam: delete access key %q: %w", accessKeyID, err)
	}
	return nil
}
