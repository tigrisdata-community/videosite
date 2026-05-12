# Plan: Vast.ai-driven NVENC encoding pipeline

## Context

Today, when a user uploads a video, the upload broker drops the source object
into Tigris at `raw/<video-id>/<filename>` and transitions the `Video` row from
`uploading` → `uploaded`. The state machine then has nowhere to go: nothing
picks up `uploaded` videos, runs ffmpeg over them, or transitions them to
`ready`. The site's `/upload` page exists, but the playback story does not.

This plan adds the missing encoding stage. We rent a GPU on Vast.ai for each
job, run ffmpeg with NVENC inside a Docker container we build, and write the
DASH outputs back to Tigris under `v/<video-id>/`. The container gets a
short-lived, narrowly-scoped Tigris IAM keypair (read one source object, write
one prefix) that we destroy when the job ends. The Vast.ai integration pokes
the REST API directly (no `vastai` CLI shell-out), so the only operational
dependency is `VAST_API_KEY`.

`cmd/orodyagzou` in `Xe/x` was the inspiration but solves a different problem:
it's a long-lived scale-to-zero proxy in front of a single inference server.
Our case is one-shot, fire-and-forget jobs whose lifetime equals the encode.
We borrow its `slog`, flag-parsing, and instance-lifecycle conventions, but
not its `vastaicli` shell-out, since we want a vendored HTTP client.

## High-level architecture

```
                                         ┌─────────────────────────────────┐
[browser] ──upload──> /api/upload ───────>│  cmd/videosite (existing)       │
                                          │  ──────────────────────────     │
                                          │  internal/upload     (broker)   │
                                          │  internal/encoder    (NEW):     │
                                          │    • orchestrator goroutine     │
                                          │    • vastai HTTP client         │
                                          │    • tigris-iam client          │
                                          │    • /api/encode-callback       │
                                          │  internal/models                │
                                          │    + EncodingJob model + DAO    │
                                          └────────────────┬────────────────┘
                                                           │ PUT /asks/{id}/
                                                           v
                                          ┌─────────────────────────────────┐
                                          │  Vast.ai instance (RTX 3090/4090)│
                                          │  Docker: jrottenberg/ffmpeg:    │
                                          │          nvidia + our binary    │
                                          │  ──────────────────────────     │
                                          │  cmd/videosite-encoder (NEW):   │
                                          │    1. GetObject (scoped key)    │
                                          │    2. ffmpeg -c:v h264_nvenc    │
                                          │    3. PutObject v/<id>/* (key)  │
                                          │    4. POST /api/encode-callback │
                                          └─────────────────────────────────┘
```

Two binaries, one repo:

- **`cmd/videosite`** — existing server, gains an orchestrator goroutine and
  webhook route.
- **`cmd/videosite-encoder`** — new, runs _inside_ the Vast.ai container.

Trigger model (per user choice): **DB poll only.** A 5-second sweeper queries
`encoding_jobs WHERE status = 'pending'`, claims one, launches it. The
`finalize()` handler doesn't directly kick the orchestrator — it just creates
the `EncodingJob` row and lets the sweeper pick it up. Simpler, no channels,
no missed signals on restart.

Completion signal (per user choice): **Hybrid (webhook + poll fallback).** The
encoder POSTs to `/api/encode-callback` on success/failure. A second sweeper
goroutine polls Vast.ai every 30s for jobs in `running` and force-fails any
whose instance has exited without a webhook landing.

## Files to create / modify

### New files

| Path                                  | Purpose                                                                                   |
| ------------------------------------- | ----------------------------------------------------------------------------------------- |
| `cmd/videosite-encoder/main.go`       | The container-side binary: download → ffmpeg → upload → webhook.                          |
| `internal/models/encoding_job.go`     | `EncodingJob` GORM model + `EncodingJobStatus` enum.                                      |
| `internal/models/encoding_job_dao.go` | CRUD + state transitions, mirrors `video_dao.go`.                                         |
| `internal/encoder/orchestrator.go`    | Sweeper goroutines: pending-claimer + stuck-job janitor. Owns the lifecycle.              |
| `internal/encoder/vastai.go`          | Direct HTTP client for Vast.ai REST API (search, mint, get, slay).                        |
| `internal/encoder/tigris_iam.go`      | AWS IAM-SDK wrapper for `CreateAccessKey` + `AttachUserPolicy` against `iam.storage.dev`. |
| `internal/encoder/webhook.go`         | `POST /api/encode-callback` handler with HMAC verification.                               |
| `internal/encoder/ffmpeg.go`          | Builds the ffmpeg argv list from the template the user supplied (nvenc swap).             |
| `docker/encoder.Dockerfile`           | `FROM jrottenberg/ffmpeg:nvidia` + Go binary on top.                                      |

### Modified files

| Path                        | Change                                                                                                                   |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `internal/models/dao.go`    | Add `&EncodingJob{}` to `AutoMigrate`.                                                                                   |
| `cmd/videosite/main.go`     | New flags: `vast-api-key`, `encoder-docker-image`, `webhook-base-url`, `encoder-poll-interval`.                          |
| `cmd/videosite/server.go`   | Construct `encoder.Orchestrator`, call `Start(ctx)` to launch sweepers, mount webhook route.                             |
| `internal/upload/upload.go` | After `MarkVideoUploaded` in `finalize()`, also call `dao.CreateEncodingJob(...)`. No goroutine — sweeper does the work. |
| `taskfile.yml`              | Add `task encoder:image` to build/push the Docker image.                                                                 |
| `go.mod` / `go.sum`         | New: `github.com/aws/aws-sdk-go-v2/service/iam`.                                                                         |

## Data model: `EncodingJob`

```go
// internal/models/encoding_job.go
package models

import "time"
import "gorm.io/gorm"

type EncodingJobStatus string

const (
    EncodingJobPending   EncodingJobStatus = "pending"   // row created, no instance yet
    EncodingJobLaunching EncodingJobStatus = "launching" // calling Vast.ai create
    EncodingJobRunning   EncodingJobStatus = "running"   // instance up, encoder working
    EncodingJobSucceeded EncodingJobStatus = "succeeded" // webhook reported ok
    EncodingJobFailed    EncodingJobStatus = "failed"    // webhook said failed, or janitor fired
)

type EncodingJob struct {
    gorm.Model
    ID                 string `gorm:"uniqueIndex"` // UUID
    VideoID            string `gorm:"index"`
    Status             EncodingJobStatus
    VastInstanceID     int    // 0 until launched
    TigrisAccessKeyID  string // empty until launched; for cleanup
    WebhookSecret      string // 32 bytes, hex
    StartedAt          *time.Time
    CompletedAt        *time.Time
    FailureReason      string
    DphTotal           float64 // cost at launch time, for telemetry
}
```

DAO ops (mirror `video_dao.go` — same `transition()` helper pattern, same
error wrapping `fmt.Errorf("models: ... : %w", ErrConflict)`):

- `CreateEncodingJob(ctx, videoID) (*EncodingJob, error)` — generates UUID +
  webhook secret, status = `pending`.
- `ClaimPendingEncodingJob(ctx) (*EncodingJob, error)` — atomic
  `pending → launching` + `LIMIT 1` so concurrent sweepers can't double-claim.
  Returns `nil, ErrNoPending` if empty.
- `MarkEncodingJobRunning(ctx, id, vastInstanceID, accessKeyID, dph) error`.
- `MarkEncodingJobSucceeded(ctx, id) error`.
- `MarkEncodingJobFailed(ctx, id, reason) error` — guarded against `succeeded`
  same way `MarkVideoFailed` is.
- `GetEncodingJob(ctx, id) (*EncodingJob, error)`.
- `ListRunningEncodingJobs(ctx) ([]*EncodingJob, error)` — for the janitor.

When a job succeeds, also call `dao.MarkVideoReady(ctx, videoID)`.
When it fails, also call `dao.MarkVideoFailed(ctx, videoID, reason)`. Wrap
both in a single GORM transaction inside the orchestrator.

## Vast.ai HTTP client (`internal/encoder/vastai.go`)

Thin client, no third-party SDK. Endpoints confirmed against the
[`vast-python` source](https://github.com/vast-ai/vast-python):

| Method   | Path                              | Purpose                                                                                 |
| -------- | --------------------------------- | --------------------------------------------------------------------------------------- |
| `POST`   | `/api/v0/bundles/`                | Search offers (returns `[]Offer` with `ask_contract_id`).                               |
| `PUT`    | `/api/v0/asks/{ask_contract_id}/` | Mint instance — body has `image`, `disk`, `env`, `onstart`, `runtype: "args"`, `label`. |
| `GET`    | `/api/v0/instances/{id}/`         | Status — read `actual_status`, `intended_status`, `status_msg`.                         |
| `DELETE` | `/api/v0/instances/{id}/`         | Slay.                                                                                   |

Auth: `Authorization: Bearer ${VAST_API_KEY}` on every request.

```go
type Client struct {
    apiKey string
    base   string // "https://console.vast.ai"
    http   *http.Client
}

type Offer struct {
    AskContractID int     `json:"ask_contract_id"`
    GpuName       string  `json:"gpu_name"`
    DphTotal      float64 `json:"dph_total"`
    NumGpus       int     `json:"num_gpus"`
    GpuRAM        float64 `json:"gpu_ram"`
}

func (c *Client) SearchOffers(ctx, query map[string]any) ([]Offer, error)
func (c *Client) Mint(ctx, askID int, cfg LaunchConfig) (instanceID int, dph float64, err error)
func (c *Client) GetInstance(ctx, id int) (*Instance, error)
func (c *Client) Destroy(ctx, id int) error
```

**Offer search** — per user choice, prefer RTX 3090, fall back to RTX 4090,
and **don't filter on `verified`**. Unverified hosts are dramatically cheaper
(often 3–5×) and the worst case for a one-shot ffmpeg job is "container
errors out → janitor retries on a different box," which the failure path
already handles. We do still filter on `rentable` and `reliability` so we
don't pick a flapping host:

```go
query := map[string]any{
    // intentionally NOT filtering on `verified` — unverified is much cheaper
    // and the failure-retry path makes it safe.
    "rentable":      map[string]any{"eq": true},
    "reliability":   map[string]any{"gte": 0.95}, // avoid hosts that flap
    "num_gpus":      map[string]any{"eq": 1},
    "gpu_name":      map[string]any{"in": []string{"RTX_3090", "RTX_4090"}},
    "cuda_max_good": map[string]any{"gte": 12.0},
    "inet_down":     map[string]any{"gte": 200}, // need to pull source quickly
    "order":         [][]string{{"dph_total", "asc"}}, // cheapest first
    "limit":         20,
}
```

After fetching, sort offers in Go by `(gpu_name == "RTX_3090") DESC, dph_total ASC`
so a 3090 always beats a 4090 even if the 4090 is cheaper. If no offers,
return a typed `ErrNoOffers` so the orchestrator can mark the job failed with
a clear reason instead of looping.

Add a config flag `--encoder-min-reliability` (default `0.95`) so the
threshold can be tuned without recompiling — if unverified hosts turn out
flakier than expected we can ratchet it up, or drop it to chase even cheaper
offers.

**Launch body** — fields cribbed from `vast-python/vast.py:create__instance`:

```json
{
  "client_id": "me",
  "image": "<our-image>",
  "env": {"SOURCE_BUCKET": "...", "AWS_ACCESS_KEY_ID": "...", ...},
  "disk": 32,
  "onstart": "/usr/local/bin/videosite-encoder",
  "runtype": "args",
  "label": "videosite-encoder/<job-id>",
  "force": false,
  "cancel_unavail": true
}
```

Response: `{"success": true, "new_contract": <int>}` — `new_contract` is the
instance ID.

## Tigris IAM helper (`internal/encoder/tigris_iam.go`)

Use `github.com/aws/aws-sdk-go-v2/service/iam` against
`AWS_ENDPOINT_URL_IAM=https://iam.storage.dev`. Per Tigris docs, policies
attach directly to access keys (no real users), via `AttachUserPolicy` /
`PutUserPolicy`-style calls where the "user" name is the access key ID.

```go
func (t *IAM) CreateScopedKey(
    ctx context.Context,
    bucket, sourceKey, destPrefix string,
) (accessKeyID, secretKey string, err error)

func (t *IAM) DeleteKey(ctx context.Context, accessKeyID string) error
```

Steps inside `CreateScopedKey`:

1. `iam.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{})` — Tigris ignores
   `UserName`, returns `AccessKeyId` + `SecretAccessKey`.
2. Build inline policy JSON:
   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Action": "s3:GetObject",
         "Resource": "arn:aws:s3:::xe-videosite/raw/<id>/<filename>"
       },
       {
         "Effect": "Allow",
         "Action": "s3:PutObject",
         "Resource": "arn:aws:s3:::xe-videosite/v/<id>/*"
       }
     ]
   }
   ```
3. `iam.PutUserPolicy(ctx, &iam.PutUserPolicyInput{UserName: &accessKeyID, PolicyName: "videosite-encoder-job", PolicyDocument: &policyJSON})`
   — Tigris uses the access-key-id as the user-name for attachment.

`DeleteKey` calls `iam.DeleteAccessKey` (which Tigris also accepts with
`UserName=accessKeyID`).

If during plumbing the `PutUserPolicy` form turns out wrong (Tigris's docs
talk about `AttachUserPolicy` against managed-policy ARNs), swap to
`AttachUserPolicy` with a pre-created policy ARN — the unit of testing here
is a real call against `iam.storage.dev`, not a mock.

## Encoder binary (`cmd/videosite-encoder/main.go`)

Reads everything from env vars (Vast.ai injects them via the launch body's
`env` field):

| Env var                                       | Meaning                                         |
| --------------------------------------------- | ----------------------------------------------- |
| `JOB_ID`                                      | EncodingJob UUID                                |
| `VIDEO_ID`                                    | Video UUID                                      |
| `SOURCE_BUCKET`                               | Tigris bucket                                   |
| `SOURCE_KEY`                                  | `raw/<video-id>/<filename>`                     |
| `DEST_PREFIX`                                 | `v/<video-id>/`                                 |
| `AWS_ENDPOINT_URL_S3`                         | `https://t3.storage.dev`                        |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | scoped keypair                                  |
| `WEBHOOK_URL`                                 | `https://videosite.example/api/encode-callback` |
| `WEBHOOK_SECRET`                              | 32 bytes hex, used for HMAC-SHA256              |

Flow:

1. Parse env (panic if any required var is missing — fail fast, the
   container has nothing else useful to do).
2. `mkdir -p /tmp/work/dash`.
3. S3 GetObject `s3://${SOURCE_BUCKET}/${SOURCE_KEY}` → `/tmp/work/source.<ext>`.
4. `exec.CommandContext(ctx, "ffmpeg", argv...)` — argv from
   `internal/encoder/ffmpeg.go`, the user's videotoolbox template with
   `h264_nvenc` swapped in:

   ```
   ffmpeg -hwaccel cuda -i /tmp/work/source.<ext> \
     -map 0:v -c:v:0 h264_nvenc -b:v:0 10000k -filter:v:0 "scale=-2:1080" \
     -map 0:v -c:v:1 h264_nvenc -b:v:1 3500k  -filter:v:1 "scale=-2:540" \
     -map 0:a -c:a:0 aac -b:a:0 96k \
     -map 0:a -c:a:1 aac -b:a:1 64k \
     -f dash -adaptation_sets "id=0,streams=v id=1,streams=a" \
     /tmp/work/dash/manifest.mpd
   ```

   Stream stdout/stderr to the parent's stderr so Vast.ai logs capture them.
   (`-vf:N` from the user's snippet doesn't actually apply per-output in
   modern ffmpeg; `-filter:v:N` is the correct per-stream form. Calling this
   out so we don't reproduce a syntax bug.)

5. `filepath.Walk("/tmp/work/dash")` and PutObject each file to
   `${DEST_PREFIX}<filename>` with appropriate `Content-Type` (manifest is
   `application/dash+xml`, segments are `video/iso.segment`).
6. POST `${WEBHOOK_URL}` with body `{"job_id": "...", "status": "succeeded"}`,
   header `X-Webhook-Signature: sha256=<hmac>`. On any earlier error, POST
   with `"status":"failed"` and `"reason":"..."`.
7. Exit 0 on success, 1 on failure. Either way the orchestrator's janitor
   will see the instance as `exited` and reconcile.

Use `defer` to ensure the webhook fires even on panic (recover in main).

## Orchestrator (`internal/encoder/orchestrator.go`)

```go
type Orchestrator struct {
    dao   *models.DAO
    vast  *Client
    iam   *IAM
    cfg   Config
    log   *slog.Logger
}

func (o *Orchestrator) Start(ctx context.Context) {
    go o.pendingLoop(ctx)
    go o.janitorLoop(ctx)
}
```

**Pending loop** (every `cfg.PollInterval`, default 5s):

1. `ClaimPendingEncodingJob(ctx)` — atomic `pending → launching`. If
   `ErrNoPending`, sleep.
2. `iam.CreateScopedKey(...)` — get scoped credentials.
3. `vast.SearchOffers(...)` — 3090s preferred. Sort, pick first.
4. `vast.Mint(askID, LaunchConfig{Image, Env: ...})` — env map includes the
   scoped credentials, the webhook URL/secret, the source/dest paths.
5. `dao.MarkEncodingJobRunning(jobID, instanceID, accessKeyID, dph)`.
6. On any error in steps 2–4: `failJob(jobID, err)` which:
   - calls `iam.DeleteKey` if a key was created
   - calls `vast.Destroy` if an instance was minted
   - calls `dao.MarkEncodingJobFailed` and `dao.MarkVideoFailed`

**Janitor loop** (every 30s):

1. `ListRunningEncodingJobs(ctx)`.
2. For each job, `vast.GetInstance(jobInstanceID)`.
3. If `actual_status == "exited"` and the job is still `running` (no webhook
   landed): `failJob(jobID, "instance exited without webhook")`. Probably the
   container crashed or networking dropped.
4. If job is older than `cfg.MaxJobDuration` (default 2h) and still
   `running`: same — fail and slay.
5. Always-cleanup: regardless of how the job ended, on transition out of
   `running` slay the instance and delete the IAM key. Idempotent on both
   sides — Vast.ai 404 on already-destroyed, Tigris 404 on already-deleted.

## Webhook handler (`internal/encoder/webhook.go`)

`POST /api/encode-callback`:

1. Read body (cap at 4 KiB).
2. Parse `{"job_id": "...", "status": "...", "reason": "..."}`.
3. Lookup `EncodingJob` by ID; reject with 404 if missing.
4. Compute `hmac.New(sha256.New, []byte(job.WebhookSecret)).Write(body)` and
   constant-time compare against `X-Webhook-Signature` header (strip
   `sha256=` prefix). 401 on mismatch.
5. Branch on status:
   - `succeeded`: `MarkEncodingJobSucceeded(id)` + `MarkVideoReady(videoID)`,
     then async (`go`) cleanup (slay + delete key).
   - `failed`: `MarkEncodingJobFailed(id, reason)` + `MarkVideoFailed`, then
     async cleanup.
6. Respond 204.

Mount in `server.go` next to existing `s.uploader.Mount(mux)`:

```go
s.encoder.Mount(mux) // adds POST /api/encode-callback
```

## Docker image (`docker/encoder.Dockerfile`)

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/videosite-encoder ./cmd/videosite-encoder

FROM jrottenberg/ffmpeg:nvidia
COPY --from=build /out/videosite-encoder /usr/local/bin/videosite-encoder
ENTRYPOINT ["/usr/local/bin/videosite-encoder"]
```

Built from the repo root with
`docker build -f docker/encoder.Dockerfile -t reg.xeiaso.net/xeserv/videosite-encoder:<git-sha> .`,
wired into `taskfile.yml` as `task encoder:image` (matching the
`reg.xeiaso.net/xeserv/waifuwave:latest` registry style from orodyagzou). The
image tag is read by the server from the `--encoder-docker-image` flag.

## New configuration

Added to `cmd/videosite/main.go` (with `flagenv` env-var fallback):

| Flag                      | Env                     | Default                                          |
| ------------------------- | ----------------------- | ------------------------------------------------ |
| `--vast-api-key`          | `VAST_API_KEY`          | `""` (required)                                  |
| `--vast-api-base`         | `VAST_API_BASE`         | `https://console.vast.ai`                        |
| `--encoder-docker-image`  | `ENCODER_DOCKER_IMAGE`  | `reg.xeiaso.net/xeserv/videosite-encoder:latest` |
| `--encoder-disk-gb`       | `ENCODER_DISK_GB`       | `32`                                             |
| `--encoder-poll-interval` | `ENCODER_POLL_INTERVAL` | `5s`                                             |
| `--encoder-max-duration`  | `ENCODER_MAX_DURATION`  | `2h`                                             |
| `--webhook-base-url`      | `WEBHOOK_BASE_URL`      | (required, e.g. `https://videosite.xeiaso.net`)  |
| `--tigris-iam-endpoint`   | `TIGRIS_IAM_ENDPOINT`   | `https://iam.storage.dev`                        |

The Tigris credentials and bucket name from existing flags are reused.

## Reused code from existing repo

- DAO pattern: `internal/models/video_dao.go:11` — `transition()` helper.
  Mirror it verbatim for `EncodingJob`.
- AWS S3 client setup: `internal/upload/upload.go:77-89` — same
  `awsconfig.LoadDefaultConfig` + `WithRegion("auto")` pattern works for both
  the orchestrator's bookkeeping and the encoder binary's upload step.
- HTTP route mount pattern: `internal/upload/upload.go` — `Mount(mux)` method.
- Logger: `slog` everywhere, with structured fields.
- UUIDs: `github.com/google/uuid` (already in `go.mod`).

## Verification

End-to-end:

1. `task encoder:image` builds and pushes the encoder image.
2. `task` (or whatever runs the dev server) starts `cmd/videosite` with
   `VAST_API_KEY`, `WEBHOOK_BASE_URL` (an ngrok tunnel for local dev),
   Tigris creds, `ENCODER_DOCKER_IMAGE`.
3. Open `/upload`, upload a small mp4 (~10 MB).
4. Watch logs: should see `claimed pending job`, `created scoped iam key`,
   `searched offers gpu_name=RTX_3090 count=N`, `minted instance id=...`.
5. Tail the Vast.ai instance logs (`vastai logs <id>`) — confirm ffmpeg ran
   to completion.
6. Webhook lands; logs show `job succeeded`. Video row transitions to `ready`.
   `EncodingJob` row transitions to `succeeded`.
7. Confirm cleanup: `vast.GetInstance(id)` → 404, `iam.GetAccessKey(...)` → 404.
8. Confirm Tigris contents: `s3 ls s3://xe-videosite/v/<id>/` shows
   `manifest.mpd`, `init-stream0.m4s`, `chunk-stream0-*.m4s`, etc.
9. Browse to `/<video-id>` (or wherever playback lives) and play the DASH
   manifest in the existing player.

Failure-path checks:

- Submit a corrupted file, verify ffmpeg exits non-zero, the webhook reports
  `failed`, the `Video` and `EncodingJob` rows both reach `failed`, and
  cleanup still runs.
- Manually delete the Vast.ai instance mid-encode (simulate crash). Within
  30s the janitor should mark the job failed.
- Disconnect the webhook (block the URL). Verify the janitor still
  transitions the job to a terminal state when the instance exits.

Unit tests worth writing (table-driven, per the codebase's style):

- `encoding_job_dao_test.go`: claim concurrency (two goroutines both call
  `ClaimPendingEncodingJob`, only one wins).
- `vastai_test.go`: round-trip of search query → JSON body matches what
  vast-python sends (golden file).
- `webhook_test.go`: HMAC verification accepts good signatures, rejects bad,
  rejects when `WebhookSecret` mismatches.
- `ffmpeg_test.go`: argv builder produces the expected arg list for given
  inputs.

---

## Deviations from plan during implementation

The plan above is preserved as-written for historical reference. The following
changes were made during implementation:

### One ticker, not two goroutines

Plan had a 5s pending-claimer goroutine + a 30s janitor goroutine. Final
implementation collapses both into a single 10s `tick()` that calls
`claimAndLaunchOne()` then `reconcile()` sequentially. One ticker is simpler,
both halves are cheap, and we never want them racing each other on the same
job anyway. Tunables (`tickInterval`, `maxJobDuration`, `minReliability`,
`gpuPrefs`, `encoderDiskGB`) are package-level constants in
`orchestrator.go`; the planned `--encoder-poll-interval`,
`--encoder-max-duration`, and `--encoder-min-reliability` flags were not
added.

### Webhook handler lives in `orchestrator.go`

Plan listed `internal/encoder/webhook.go`. The handler is small (~40 lines)
and tightly coupled to the orchestrator's `completeJob` cleanup path, so it
went into `orchestrator.go` alongside the loop. `webhook_test.go` still
exists as a standalone test file.

### Tigris IAM: managed policies, not inline

Tigris's IAM service returns 501 for `PutUserPolicy` — inline policies
aren't supported. `CreateScopedKey` now does `CreateAccessKey` →
`CreatePolicy` → `AttachUserPolicy`, and the resulting policy ARN has to be
tracked on the `EncodingJob` for cleanup. Consequences:

- New `EncodingJob.TigrisPolicyARN string` field.
- `MarkEncodingJobRunning` takes an extra `policyARN` argument.
- `ScopedKey` is now a struct (`AccessKeyID`, `SecretKey`, `PolicyARN`)
  instead of two return values.
- `DeleteScopedKey` does `DetachUserPolicy` → `DeleteAccessKey` →
  `DeletePolicy`, all idempotent (treats `NoSuchEntity` as success).
- Policy also grants `s3:AbortMultipartUpload` so a crashed encoder doesn't
  leak in-progress multiparts.

### `MarkEncodingJobRunning` / janitor scope

Plan's DAO had `ListRunningEncodingJobs` (only `running`). Final
implementation has `ListEncodingJobsForJanitor` returning both `launching`
and `running` jobs — if the server dies between `Mint` and
`MarkEncodingJobRunning`, the `launching` row would otherwise be stuck. The
reconciler treats a `launching` job with no `VastInstanceID` as a no-op
(waits for the orchestrator to retry or the timeout to fire) but will time
it out via the `maxJobDuration` check.

### `Mint` returns instance ID only

Plan: `Mint(ctx, askID, cfg) (instanceID int, dph float64, err error)`. Real
signature: `Mint(ctx, askID, cfg LaunchConfig) (int, error)`. The dph value
is read off the `Offer` we picked before calling `Mint`, so passing it
through `Mint`'s return was redundant. The `force` / `cancel_unavail` fields
mentioned in the plan's launch body were also dropped from the wire form —
vast-python sends them but they aren't required, and we don't have a use
for either.

### Encoder integration via `uploader.OnUploaded`

Plan: `internal/upload/upload.go` finalize() directly calls
`dao.CreateEncodingJob`. Final: `upload.Handler` exposes an `OnUploaded
func(ctx, videoID)` field; `cmd/videosite/server.go` sets it to a closure
that creates the encoding job when (and only when) the orchestrator is
configured. Keeps `internal/upload` free of any encoder import.

### Status transitions are not in a transaction

Plan said `MarkEncodingJob{Succeeded,Failed}` plus the matching `MarkVideo*`
update would be "wrap[ped] in a single GORM transaction inside the
orchestrator." Implementation does them as two sequential calls and tolerates
mid-flight failure: the second call returns `ErrConflict` if it can't apply
(e.g. video is already in a terminal state), the orchestrator logs and moves
on. Janitor + webhook both call `completeJob`, so a partially-applied state
gets retried next tick.

### Encoder binary: tmpdir + mime detection, no panic recovery

Plan called for `/tmp/work/dash` hardcoded paths and explicit content types
for manifest/segments. Real binary uses `os.MkdirTemp("", "videosite-encoder-*")`
and `mime.TypeByExtension`. There's no top-level `recover()` either — the
plan's note about "defer to ensure the webhook fires even on panic" was
dropped; the existing `os.Exit(1)` + webhook-on-error path is good enough,
and a panic in `run()` will surface in the Vast.ai container logs.

Env var set also includes `AWS_REGION=auto`, which the plan didn't list.

### Docker base image and registry

Plan: `FROM jrottenberg/ffmpeg:nvidia`. Real Dockerfile uses
`roflcoopter/amd64-cuda-ffmpeg:<date-tag>` because `jrottenberg/ffmpeg`'s
nvidia variant lags behind on CUDA toolkit versions and was missing NVENC
support for newer drivers. The image uses `CMD` instead of `ENTRYPOINT`
(matches how `docker:www` is built).

Registry: `ghcr.io/tigrisdata-community/videosite/encoder` (not the planned
`reg.xeiaso.net/xeserv/...` — this repo lives under
`github.com/tigrisdata-community/videosite`). Taskfile target is
`docker:encoder` (alongside `docker:www` and a `docker` umbrella), not the
planned `encoder:image`.

### Optional orchestrator

`cmd/videosite/server.go` only constructs the orchestrator when both
`VAST_API_KEY` and `WEBHOOK_BASE_URL` are non-empty. Without them, the
server logs a warning and skips the encoder entirely — useful for local dev
against just the upload flow.

### Tigris IAM: bucket-Editor scoped keys (May 2026)

The earlier "managed policies, not inline" deviation still required the
videosite root key to hold `NamespaceAdmin` on the Tigris org — the
`CreatePolicy` / `AttachUserPolicy` actions are admin-gated. That's far
more authority than a media-encoding service should ever hold, so the
flow was reworked.

`internal/encoder/tigris_iam.go` now calls Tigris's proprietary IAM API
(`CreateAccessKeyWithBucketsRole` / `DeleteAccessKey`) directly over
SigV4-signed HTTP. The new per-job key is granted the `Editor` role on
the configured bucket and nothing else. The caller (videosite's root
key) only needs `Editor` on that same bucket to mint it — no admin.

The compromise: keys are now bucket-scoped, not path-scoped. Each
encoder job has read/write/delete authority across the entire encoder
bucket for its lifetime, not just `raw/<id>/<filename>` and
`v/<id>/*`. To bound the blast radius:

- The completion path still deletes the key as soon as the job reaches
  a terminal state (unchanged).
- A new hourly cleanup goroutine on the orchestrator (`cleanupLoop` /
  `sweepStaleKeys`) hard-deletes any access key whose `EncodingJob` row
  is older than 48 hours and still has `tigris_access_key_id` set. This
  catches orphans from orchestrator crashes between mint and
  `MarkEncodingJobRunning`, lost webhooks, and any path where
  `completeJob` didn't run. The 48h ceiling is a compromise: long
  enough that legitimate `maxJobDuration=2h` jobs never trip it,
  short enough to cap the worst-case credential lifetime.

Consequences:

- `ScopedKey` is now `{AccessKeyID, SecretKey}` — the `PolicyARN` field
  is gone (no managed policy to track).
- `EncodingJob.TigrisPolicyARN` field is removed. The SQLite column is
  left in place since auto-migrate doesn't drop columns; it sits
  unused.
- `MarkEncodingJobRunning` and `DeleteScopedKey` lost their
  `policyARN` arguments.
- New DAO methods: `ListStaleEncodingJobKeys(ctx, olderThan)` and
  `ClearEncodingJobAccessKey(ctx, jobID)`.
- New package-level constants `cleanupInterval = 1h` and
  `staleKeyAge = 48h` in `orchestrator.go`.
- Per-job key names follow the prefix `videosite-encoder-<jobID>` so
  `tigris access-keys list` is human-readable.
- The `aws-sdk-go-v2/service/iam` dependency is no longer needed (the
  proprietary actions aren't modeled there); `aws/signer/v4` is used
  directly for request signing.
