# Plan: Capture FFmpeg Logs on Encoder Failure

## Context

When an encoding job fails, the per-video status page shows only the short Go error string (e.g. "ffmpeg: exit status 1"). The actual ffmpeg stderr — containing codec errors, unsupported format details, and the real failure cause — is lost to the container logs. This plan captures that output and surfaces it on the status page when the video fails.

## Changes (in dependency order)

### 1. Add `EncodeLogs` column to Video model

**File:** `internal/models/video.go`

Add `EncodeLogs string` field. GORM AutoMigrate adds the column automatically.

### 2. Add `MarkVideoFailedWithLogs` DAO method

**File:** `internal/models/video_dao.go`

New method alongside existing `MarkVideoFailed` — sets `status`, `failure_reason`, and `encode_logs` in one update. Keeps `MarkVideoFailed` unchanged so upload handler and reconciler callers are unaffected.

### 3. Update WebhookBody and webhook handler

**File:** `internal/encoder/orchestrator.go`

- Add `Logs string \`json:"logs,omitempty"\``to`WebhookBody`
- Increase `handleWebhook` body read limit from 4KB to 128KB
- Add `logs string` param to `completeJob` — pass `""` from reconciler/success callers, pass `msg.Logs` from webhook failure branch
- In `completeJob` failure branch: use `MarkVideoFailedWithLogs` when `logs != ""`, else `MarkVideoFailed`

### 4. Capture ffmpeg output in encoder

**File:** `cmd/videosite-encoder/main.go`

- Add `limitedWriter` type (64KB cap, silently drops overflow)
- Use `io.MultiWriter(os.Stderr, lw)` for ffmpeg stdout+stderr — output still flows to container logs while being captured
- Change `run` to return `(logs string, err error)` — returns captured logs on ffmpeg failure, empty string for non-ffmpeg errors (download/upload failures)
- Update `postWebhook` signature to accept `logs string`
- Update `main` to pass logs through

### 5. Display logs on status page

**File:** `web/video.templ`

Add a `<details><summary>Encoder output</summary>` with a scrollable `<pre>` block showing `v.EncodeLogs`, only when status is failed and logs are non-empty. Inline styles for dark terminal-like appearance.

### 6. Add test for failure-with-logs webhook path

**File:** `internal/encoder/webhook_test.go`

Extend the existing table-driven test with a "failed with logs" case: create video+job, post webhook with `Status: failed` and `Logs: "ffmpeg output here"`, verify both `FailureReason` and `EncodeLogs` are persisted on the video.

## Size limits

- **Encoder capture:** 64KB (covers the tail of even long encodes)
- **Webhook body:** 128KB (64KB logs + JSON overhead + headroom)

## Verification

1. `go tool task generate` — regenerate templ
2. `go tool task test:all` — all tests pass including new webhook test
3. Manual: upload a video that will fail encoding, check that the status page shows the encoder output in a collapsible section
