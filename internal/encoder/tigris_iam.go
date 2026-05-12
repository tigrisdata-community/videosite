package encoder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/google/uuid"
)

// TigrisIAM wraps Tigris's proprietary IAM API. The S3-compatible AWS
// IAM endpoint requires NamespaceAdmin on the caller to create policies,
// which is more authority than videosite should hold. Instead this client
// calls Tigris's own action — CreateAccessKeyWithBucketsRole — which only
// needs the caller to hold Editor on the same bucket the new key is
// scoped to. Requests are plain form-encoded POSTs signed SigV4 for
// service=iam, region=auto.
type TigrisIAM struct {
	httpClient *http.Client
	signer     *v4.Signer
	creds      aws.CredentialsProvider
	endpoint   string
	bucket     string
}

type TigrisIAMConfig struct {
	Endpoint    string // e.g. https://iam.storage.dev
	AccessKeyID string // root creds used to mint per-job keys
	SecretKey   string
	Bucket      string
}

// accessKeyNamePrefix lets a human reading `tigris access-keys list`
// trace a key back to a row, and lets the cleanup loop differentiate
// ours from any other keys in the org if we ever switch to a
// list-driven sweep.
const accessKeyNamePrefix = "videosite-encoder-"

func NewTigrisIAM(_ context.Context, cfg TigrisIAMConfig) (*TigrisIAM, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("encoder/iam: Endpoint is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretKey == "" {
		return nil, errors.New("encoder/iam: AccessKeyID and SecretKey are required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("encoder/iam: Bucket is required")
	}
	return &TigrisIAM{
		httpClient: http.DefaultClient,
		signer:     v4.NewSigner(),
		creds:      credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretKey, ""),
		endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
		bucket:     cfg.Bucket,
	}, nil
}

type ScopedKey struct {
	AccessKeyID string
	SecretKey   string
}

// CreateScopedKey mints a fresh access key with Editor role on the
// configured bucket. The key name is keyed off jobID so it's traceable.
// Bucket-level Editor is broader than the original per-object policy
// scope; the trade-off is documented in docs/plans/vast-ai-encoding.md.
func (t *TigrisIAM) CreateScopedKey(ctx context.Context, jobID string) (*ScopedKey, error) {
	reqBody, err := json.Marshal(map[string]any{
		"req_uuid": uuid.NewString(),
		"name":     accessKeyNamePrefix + jobID,
		"buckets_role": []map[string]string{
			{"bucket": t.bucket, "role": "Editor"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encoder/iam: marshal create req: %w", err)
	}
	form := url.Values{}
	form.Set("Req", string(reqBody))

	var resp struct {
		CreateAccessKeyResult struct {
			AccessKey struct {
				AccessKeyID     string `json:"AccessKeyId"`
				SecretAccessKey string `json:"SecretAccessKey"`
				UserName        string `json:"UserName"`
			} `json:"AccessKey"`
		} `json:"CreateAccessKeyResult"`
	}
	if err := t.do(ctx, "CreateAccessKeyWithBucketsRole", form, &resp); err != nil {
		return nil, fmt.Errorf("encoder/iam: create scoped key: %w", err)
	}
	if resp.CreateAccessKeyResult.AccessKey.AccessKeyID == "" {
		return nil, errors.New("encoder/iam: create scoped key: empty access key id in response")
	}
	return &ScopedKey{
		AccessKeyID: resp.CreateAccessKeyResult.AccessKey.AccessKeyID,
		SecretKey:   resp.CreateAccessKeyResult.AccessKey.SecretAccessKey,
	}, nil
}

// DeleteScopedKey deletes the access key. Already-gone keys are not
// errors so this is safe to call from cleanup paths that may race the
// completion path.
func (t *TigrisIAM) DeleteScopedKey(ctx context.Context, accessKeyID string) error {
	if accessKeyID == "" {
		return nil
	}
	form := url.Values{}
	form.Set("Action", "DeleteAccessKey")
	form.Set("Version", "2010-05-08")
	form.Set("AccessKeyId", accessKeyID)
	form.Set("UserName", accessKeyID)

	if err := t.do(ctx, "DeleteAccessKey", form, nil); err != nil {
		if isAlreadyGone(err) {
			return nil
		}
		return fmt.Errorf("encoder/iam: delete access key %q: %w", accessKeyID, err)
	}
	return nil
}

// do signs and sends a form-encoded POST to t.endpoint?Action=<action>,
// then unmarshals the response into out (if non-nil). A non-2xx response
// is returned as a *httpError so callers can branch on status.
func (t *TigrisIAM) do(ctx context.Context, action string, form url.Values, out any) error {
	body := form.Encode()
	endpoint := t.endpoint + "/?Action=" + action

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	creds, err := t.creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("retrieve creds: %w", err)
	}
	sum := sha256.Sum256([]byte(body))
	if err := t.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "iam", "auto", time.Now()); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode, body: string(respBody)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w (body=%s)", err, respBody)
	}
	return nil
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("tigris iam: http %d: %s", e.status, e.body)
}

// isAlreadyGone returns true when the error represents a delete on a
// key that's already missing. Tigris returns 404 or includes
// "NoSuchEntity" in the body for both AWS-compat and proprietary
// actions.
func isAlreadyGone(err error) bool {
	var herr *httpError
	if !errors.As(err, &herr) {
		return false
	}
	if herr.status == http.StatusNotFound {
		return true
	}
	return strings.Contains(herr.body, "NoSuchEntity")
}
