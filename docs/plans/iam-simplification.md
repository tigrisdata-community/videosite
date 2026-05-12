# Plan: Switch encoder IAM to bucket-Editor scoped keys + hourly cleanup

## Context

`internal/encoder/tigris_iam.go` currently mints per-job access keys by
calling AWS-IAM-compatible actions against Tigris's IAM endpoint:
`CreateAccessKey` → `CreatePolicy` → `AttachUserPolicy`, with cleanup as
`DetachUserPolicy` → `DeleteAccessKey` → `DeletePolicy`. Those actions
require the calling key (videosite's root Tigris key) to hold
`NamespaceAdmin` — full org-wide IAM authority — which is far more power
than this service should have.

Tigris's own API has a different, narrower primitive:
`CreateAccessKeyWithBucketsRole`, which is the wire form of
`tigris access-keys create … --bucket X --role Editor`. It atomically
mints a key already scoped to one bucket's `Editor` role. The calling key
only needs `Editor` on that same bucket to invoke it (the same permission
the new key is being granted). No managed-policy hops, no admin
requirement.

The compromise: we lose the ability to scope the per-job key down to a
single source object and dest prefix — Tigris's role system is bucket-
level. Every encoder job will have read/write/delete authority across the
entire encoder bucket for its lifetime. That's a real downgrade from the
current model. To compensate we keep keys short-lived (deleted at job
completion as today) and add an hourly DB-driven sweep that hard-kills
any access key whose `EncodingJob` row is older than 48 hours and still
has a recorded `TigrisAccessKeyID`. The 48h ceiling covers orchestrator
crashes between mint and `MarkEncodingJobRunning`, webhook losses, and
janitor-skipped reconciliations.

## Approach

1. **Rewrite `internal/encoder/tigris_iam.go`** to call Tigris's
   proprietary actions directly over HTTP with SigV4 signing
   (`aws/signer/v4`, already a transitive dep). The S3-compatible AWS IAM
   SDK doesn't model these actions, but the wire protocol is plain form-
   encoded POSTs against the same `tigris-iam-endpoint` URL, signed for
   `service=iam, region=auto` exactly like the existing SDK is doing today.

2. **Drop policy ARNs everywhere.** `ScopedKey` becomes
   `{AccessKeyID, SecretKey}`. The `TigrisPolicyARN` struct field is
   removed from `EncodingJob`; the orphaned SQLite column is left as-is
   (auto-migrate doesn't drop columns and this codebase has no migration
   plumbing — harmless and reversible).

3. **Add a DB-driven cleanup loop.** A second goroutine on the orchestrator
   ticks every hour, queries `EncodingJob` rows with non-empty
   `TigrisAccessKeyID` whose `created_at < now-48h`, and calls
   `DeleteScopedKey` for each. `DeleteAccessKey` is already idempotent
   (treats `NoSuchEntity` as success), so re-running it on a key the
   completion path already deleted is a no-op.

## Tigris API reference (from `tigrisdata/cli` + `tigrisdata/storage`)

Endpoint base: the existing `--tigris-iam-endpoint` flag (e.g.
`https://iam.storage.dev`). Auth: SigV4 with the root key, service `iam`,
region `auto`. All requests are form-encoded POSTs.

| Action                           | URL path                                  | Body                                                                                          | Notes                                                                            |
| -------------------------------- | ----------------------------------------- | --------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| `CreateAccessKeyWithBucketsRole` | `/?Action=CreateAccessKeyWithBucketsRole` | `Req={"req_uuid":"<uuid>","name":"<name>","buckets_role":[{"bucket":"<b>","role":"Editor"}]}` | Returns `CreateAccessKeyResult.AccessKey.{AccessKeyId,SecretAccessKey,UserName}` |
| `DeleteAccessKey`                | `/?Action=DeleteAccessKey`                | `Action=DeleteAccessKey&Version=2010-05-08&AccessKeyId=<id>&UserName=<id>`                    | Same as AWS-compat call; idempotent                                              |

Roles allowed: `Editor`, `ReadOnly`, `NamespaceAdmin`. We use `Editor`.
The name pattern is `videosite-encoder-<jobID>` so a human reading
`t3 keys list` can trace a key back to a row.

## Files to modify

### `internal/encoder/tigris_iam.go` — rewrite

- Drop `aws-sdk-go-v2/service/iam` import.
- Keep `aws-sdk-go-v2/aws`, `config`, `credentials`. Add
  `aws-sdk-go-v2/aws/signer/v4`.
- `TigrisIAM` keeps `bucket` and gains an `endpoint string`,
  `creds aws.CredentialsProvider`, `httpClient *http.Client` (default
  `http.DefaultClient`), `signer *v4.Signer`.
- `NewTigrisIAM`: same validations; build a `credentials.StaticCredentialsProvider`
  from cfg, no more `iam.NewFromConfig` call.
- `ScopedKey` loses `PolicyARN`. Keep `AccessKeyID`, `SecretKey`.
- `CreateScopedKey(ctx, jobID string)` — signature changes from
  `(ctx, sourceKey, destPrefix string)`. Body builds the `Req` JSON with
  `name = "videosite-encoder-" + jobID` and `buckets_role: [{bucket: t.bucket, role: "Editor"}]`.
- `DeleteScopedKey(ctx, accessKeyID string)` — signature loses the
  `policyARN` argument. Sends `DeleteAccessKey` form.
- Add an internal `do(ctx, action, form url.Values, out any) error` that
  signs and sends the request. Signing: hash the form body with SHA-256,
  call `signer.SignHTTP(ctx, creds, req, payloadHash, "iam", "auto", time.Now())`.
- Drop the `CreatePolicy`/`AttachUserPolicy`/`DeletePolicy` helpers and
  `isNoSuchEntity` (no longer applicable — Tigris returns its own error
  shape; treat any 200 OK with `status != "success"` as failure, treat
  404-ish failures on delete as success).

### `internal/encoder/orchestrator.go`

- Line 103: `o.iam.CreateScopedKey(ctx, sourceKey, destPrefix)` →
  `o.iam.CreateScopedKey(ctx, job.ID)`. Note `sourceKey`/`destPrefix` are
  still needed for `buildEnv` so keep those local variables.
- Lines 111, 125, 133: drop `scoped.PolicyARN` arg from `DeleteScopedKey`.
- Line 130: `MarkEncodingJobRunning(ctx, job.ID, instanceID, scoped.AccessKeyID, scoped.PolicyARN, offer.DphTotal)` →
  drop the `scoped.PolicyARN` arg.
- Line 254: `o.iam.DeleteScopedKey(ctx, job.TigrisAccessKeyID, job.TigrisPolicyARN)` →
  `o.iam.DeleteScopedKey(ctx, job.TigrisAccessKeyID)`.
- Add a new constant `staleKeyAge = 48 * time.Hour` and
  `cleanupInterval = 1 * time.Hour` near the existing constants.
- Add `Start` to launch a second goroutine `o.cleanupLoop(ctx)`:
  ticker @ `cleanupInterval`, calls `o.sweepStaleKeys(ctx)`.
- Add `sweepStaleKeys(ctx)` method: calls a new DAO method
  `ListStaleEncodingJobKeys(ctx, staleKeyAge)` → for each returned
  `(jobID, accessKeyID)`, calls `o.iam.DeleteScopedKey(ctx, accessKeyID)`
  and on success calls a new DAO method `ClearEncodingJobAccessKey(ctx, jobID)`
  to null out the column so the next sweep doesn't re-attempt. Log
  per-deletion and total counts.

### `internal/models/encoding_job.go`

- Remove the `TigrisPolicyARN string` field (line 16). Leave
  `TigrisAccessKeyID` alone.

### `internal/models/encoding_job_dao.go`

- `MarkEncodingJobRunning` signature drops `policyARN`. Remove
  `"tigris_policy_arn": policyARN` from the updates map.
- Add `ListStaleEncodingJobKeys(ctx, olderThan time.Duration) ([]struct{ID, AccessKeyID string}, error)`:
  selects `id, tigris_access_key_id` from `encoding_jobs`
  where `tigris_access_key_id <> '' AND created_at < ?` (with the
  threshold). Returns a small struct slice — keep it inline to avoid
  exposing a new public type.
- Add `ClearEncodingJobAccessKey(ctx, jobID string) error`:
  `UPDATE encoding_jobs SET tigris_access_key_id = '' WHERE id = ?`.
  Plain update — no status guard needed since the field is cleanup-only.

### `internal/encoder/webhook_test.go`

- The orchestrator harness here likely constructs a `TigrisIAM` or stubs
  out its calls. Update any stubs to match the new signatures and remove
  references to `PolicyARN`. (No new test coverage is required; the
  sweep logic gets its own table-driven test below.)

### `docs/plans/vast-ai-encoding.md`

- Append a new section to the "Deviations from plan during
  implementation" tail block, describing the May 2026 switch from
  per-object scoping to bucket-Editor scoping. Two paragraphs: what
  changed (above), why (Tigris admin permission requirement was too
  broad), and the 48h cleanup mitigation.

## New tests

- `internal/encoder/cleanup_test.go` (table-driven, per
  `go-table-driven-tests` style): builds an in-memory DAO with a handful
  of `EncodingJob` rows at varying ages and access-key-id states, calls
  `ListStaleEncodingJobKeys`, asserts only the right rows come back.
  Same file tests `ClearEncodingJobAccessKey` zeros the column without
  touching status.
- Optional: a tiny `tigris_iam_test.go` that builds a request via a
  `httptest.Server` standing in for the IAM endpoint, asserts the
  serialized form body matches the documented shape (decode `Req`, check
  `name` + `buckets_role`). Catches future regressions on the wire
  format. Skip if it adds too much fixture mass.

## Critical files

- `internal/encoder/tigris_iam.go` — full rewrite.
- `internal/encoder/orchestrator.go` — call-site updates + cleanup loop.
- `internal/models/encoding_job.go` — drop field.
- `internal/models/encoding_job_dao.go` — signature change + 2 new methods.
- `internal/encoder/webhook_test.go` — update stubs if any reference the
  old API.

Untouched: `cmd/videosite/server.go` (the `TigrisIAMConfig` struct is
unchanged), `cmd/videosite/main.go` (no new flags — the `staleKeyAge`
and `cleanupInterval` follow the existing package-constant convention).

## Verification

End-to-end (real Tigris + Vast.ai):

1. `go tool task generate && go tool task test:all` — full unit suite
   passes. New cleanup tests cover the DAO methods.
2. `go tool task` — start the dev server with `VAST_API_KEY`,
   `WEBHOOK_BASE_URL`, Tigris creds whose root key has bucket-Editor
   (not NamespaceAdmin) on the encoder bucket. **Confirm the orchestrator
   starts and logs `started vast.ai encoder orchestrator` even with a
   non-admin root key** — this is the headline fix.
3. Upload a small mp4. Watch logs for `claimed pending job`, no IAM
   errors during scoped-key creation. Confirm in Tigris dashboard /
   `tigris access-keys list` that a new key named
   `videosite-encoder-<jobID>` appears with the `Editor` role on the
   bucket.
4. Let the job complete. Confirm the key disappears (deleted by
   `completeJob`).
5. Cleanup sweep: manually insert a fake `EncodingJob` row with
   `tigris_access_key_id = '<some real test key id>'` and
   `created_at` set to 49h ago, then wait one `cleanupInterval` (or
   shorten the constant for the test). Confirm the key gets deleted
   and the row's `tigris_access_key_id` is cleared.
6. Crash simulation: kill the dev server mid-encode. Restart. The
   janitor's existing `maxJobDuration` path handles the in-flight job;
   the new sweep catches the IAM key if completion never ran.

Smoke for the wire format (one-shot, separate from the suite):
`AWS_REGION=auto AWS_ENDPOINT_URL_IAM=https://iam.storage.dev aws iam ...`
won't help here (different action). Instead, run a small ad-hoc Go
program that calls `iam.CreateScopedKey` against the real endpoint once
and confirms the returned key works for a `s3 ls` on the bucket and
fails for `s3 ls` on another bucket. Skip if the integration test in
step 3 already proves the same thing.
