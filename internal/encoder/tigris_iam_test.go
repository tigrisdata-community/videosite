package encoder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestCreateScopedKeyWireFormat pins the form body shape against
// Tigris's CreateAccessKeyWithBucketsRole action. If this drifts, the
// next real call will fail in production — the test exists to catch
// that at build time instead.
func TestCreateScopedKeyWireFormat(t *testing.T) {
	var (
		gotAction string
		gotName   string
		gotRole   string
		gotBucket string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAction = r.URL.Query().Get("Action")
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse form: %v", err)
		}
		var req struct {
			Name        string `json:"name"`
			BucketsRole []struct {
				Bucket string `json:"bucket"`
				Role   string `json:"role"`
			} `json:"buckets_role"`
		}
		if err := json.Unmarshal([]byte(form.Get("Req")), &req); err != nil {
			t.Errorf("decode Req: %v", err)
		}
		gotName = req.Name
		if len(req.BucketsRole) == 1 {
			gotRole = req.BucketsRole[0].Role
			gotBucket = req.BucketsRole[0].Bucket
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"CreateAccessKeyResult":{"AccessKey":{"AccessKeyId":"tid_x","SecretAccessKey":"sx","UserName":"`+req.Name+`"}}}`)
	}))
	defer srv.Close()

	iam, err := NewTigrisIAM(context.Background(), TigrisIAMConfig{
		Endpoint: srv.URL, AccessKeyID: "k", SecretKey: "s", Bucket: "encoder-bucket",
	})
	if err != nil {
		t.Fatalf("iam: %v", err)
	}
	got, err := iam.CreateScopedKey(context.Background(), "job-abc")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if gotAction != "CreateAccessKeyWithBucketsRole" {
		t.Errorf("action = %q, want CreateAccessKeyWithBucketsRole", gotAction)
	}
	if gotName != "videosite-encoder-job-abc" {
		t.Errorf("name = %q, want videosite-encoder-job-abc", gotName)
	}
	if gotBucket != "encoder-bucket" {
		t.Errorf("bucket = %q, want encoder-bucket", gotBucket)
	}
	if gotRole != "Editor" {
		t.Errorf("role = %q, want Editor", gotRole)
	}
	if got.AccessKeyID != "tid_x" || got.SecretKey != "sx" {
		t.Errorf("parsed key = %+v, want {tid_x, sx}", got)
	}
}

func TestDeleteScopedKeyIdempotent(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchEntity</Code></Error>`)
	}))
	defer srv.Close()

	iam, err := NewTigrisIAM(context.Background(), TigrisIAMConfig{
		Endpoint: srv.URL, AccessKeyID: "k", SecretKey: "s", Bucket: "b",
	})
	if err != nil {
		t.Fatalf("iam: %v", err)
	}

	if err := iam.DeleteScopedKey(context.Background(), "tid_x"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := iam.DeleteScopedKey(context.Background(), "tid_x"); err != nil {
		t.Errorf("second delete (NoSuchEntity) should be nil, got %v", err)
	}
	if err := iam.DeleteScopedKey(context.Background(), ""); err != nil {
		t.Errorf("empty id should short-circuit to nil, got %v", err)
	}
}
