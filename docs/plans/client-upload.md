# Plan: `/upload` page for cmd/videosite

## Context

The videosite is a Go + templ + HTMX app. The site already has a navbar entry for **Upload** (`web/index.templ:18`) but the route 404s — there's no handler. We want to let the user upload video files directly from the browser to Tigris (S3) so the bytes don't proxy through our server (free egress on Tigris, faster uploads, simpler backend).

The Tigris JS SDK ships a documented "client uploads" protocol: a single JSON broker endpoint that switches on an `action` field (`singlepart-init`, `multipart-init`, `multipart-get-parts`, `multipart-complete`) and hands back presigned S3 URLs. The browser then `PUT`s the bytes directly to those URLs. We're going to port that exact protocol to Go using `aws-sdk-go-v2` against the Tigris S3 endpoint.

Because video files are large, this page will **always use multipart** uploads. Alpine.js drives the per-file progress UI; HTMX handles the post-upload swap (rendering a "video added" row from the DAO).

### What's already in place

- `internal/alpinejs/` package exists with `Use()` template and `Mount()` — but `main.go` does **not** currently call `alpinejs.Mount(mux)`, so the script 404s today. (`web/index.templ:50` already references `@alpinejs.Use()`.) Plan must fix this.
- `internal/htmx/` is the reference template to mirror (script-serving via `embed.FS` + `UnchangingCache`).
- `web.HeadArea()` (`web/index.templ:48`) already pulls in HTMX + Alpine — no template change needed.
- `models.DAO` (`internal/models/dao.go`) is built and `AutoMigrate`s `Video`. The DAO instance is created in `main.go:49` but discarded with `_ = dao`. Plan needs to thread it into handlers and add CRUD methods.
- `models.Video` has `ID string`, `Status VideoStatus` (`uploaded`/`encoding`/`ready`), and `gorm.Model` timestamps.
- Existing flags (`bucket-name`, `bucket-url-base`) already use `flagenv`.

### Decisions (from clarifying Qs)

- Credentials: **new flags via flagenv** (`tigris-access-key-id`, `tigris-secret-access-key`, `tigris-endpoint`).
- Scope: **full pipeline through DB** — write `Video{Status:uploaded}` on `multipart-init`, return HTMX fragment on finalize.
- Key layout: **`raw/<uuid>/<filename>`** — server generates the UUID and stamps it as the `Video.ID`.
- Always **multipart** (video files are large).

---

## File changes

### 1. New: `internal/models/dao.go` additions

Add two methods on `*DAO`:

```go
func (d *DAO) CreateVideo(ctx context.Context, id string) (*Video, error)
func (d *DAO) MarkVideoUploaded(ctx context.Context, id string) error
func (d *DAO) GetVideo(ctx context.Context, id string) (*Video, error)
```

- `CreateVideo` — called at `multipart-init` time. Inserts a `Video{ID: id, Status: VideoStatusUploaded}`. (Today "uploaded" basically means "row exists, bytes pending".) Could be split into a `pending` state later, but per the existing const set we'll reuse `VideoStatusUploaded` and let a future commit add `pending`.
- `MarkVideoUploaded` — no-op for now since the row already starts `uploaded`; included as the explicit hook the finalize endpoint calls so the call-site reads correctly when statuses get richer (e.g., transcoding pipeline).
- `GetVideo` — used by the finalize endpoint to render the HTMX fragment.

Convention to follow: errors wrapped with `fmt.Errorf("models: <op>: %w", err)`. Match the style already in `dao.go`.

### 2. New: `internal/upload/upload.go`

A new package that owns the Tigris/S3 client and the JSON broker handler. Keeping it separate from `web/` so the broker is testable without templ.

```go
package upload

type Config struct {
    Bucket      string
    Endpoint    string // e.g. "https://t3.storage.dev"
    AccessKeyID string
    SecretKey   string
    URLBase     string // public URL prefix used in finalize
    Expires     time.Duration
}

type Handler struct {
    cfg     Config
    s3      *s3.Client
    presign *s3.PresignClient
    dao     *models.DAO
    log     *slog.Logger
}

func New(cfg Config, dao *models.DAO, lg *slog.Logger) (*Handler, error)
func (h *Handler) Mount(mux *http.ServeMux)   // registers /api/upload and /api/upload/finalize
func (h *Handler) ServeHTTP(...)              // /api/upload — JSON broker
func (h *Handler) finalize(...)               // /api/upload/finalize — HTMX fragment
```

Wire format (mirrors the JS SDK exactly — see `packages/storage/src/lib/upload/{client,server,shared}.ts` in the cloned reference at `/var/folders/g4/xyx7_tsj3z5brtsvlr72bnw40000gn/T/tmp.5PFFFksP3w/storage`):

```go
type Action string
const (
    SinglepartInit    Action = "singlepart-init"
    MultipartInit     Action = "multipart-init"
    MultipartGetParts Action = "multipart-get-parts"
    MultipartComplete Action = "multipart-complete"
)

type request struct {
    Action   Action              `json:"action"`
    Name     string              `json:"name"`
    UploadID string              `json:"uploadId,omitempty"`
    Parts    []int32             `json:"parts,omitempty"`
    PartIDs  []map[string]string `json:"partIds,omitempty"`
}

type response struct {
    Data  any    `json:"data,omitempty"`
    Error string `json:"error,omitempty"`
}
```

Per-action behavior:

- **`multipart-init`**:
  1. Validate `req.Name` (no `..`, no leading `/`, non-empty, length cap).
  2. Generate `id := uuid.NewString()`, build `key := "raw/" + id + "/" + req.Name`.
  3. `dao.CreateVideo(ctx, id)` — fail the request if this errors (don't initiate S3 upload for a row we couldn't create).
  4. `s3.CreateMultipartUpload({Bucket, Key: key})` → `uploadId`.
  5. Return `{ data: { uploadId, key, id } }`. **The client must echo `key` back in subsequent calls — that's the canonical object key.** Adding `key` to the response is a small extension over the JS SDK's wire format (which assumes the client knows the key from the start); this is the cleanest way to keep server-side key construction.
- **`multipart-get-parts`**: presign `UploadPartCommand` for each part number; return `[{part, url}, ...]`. Use `req.Name` as the **already-prefixed key** (the client now knows it).
- **`multipart-complete`**:
  1. Sort `partIds` by part number, build `[]types.CompletedPart`.
  2. `s3.CompleteMultipartUpload`.
  3. Return `{ data: { path, url } }` where `url = cfg.URLBase + key` (public URL — bucket is presumed public-read for `raw/` or we'll presign GET later).
- **`singlepart-init`**: keep for parity but the upload page will never call it. Implement and presign `PutObjectCommand`.

Validation: cap request body size (`http.MaxBytesReader` 64 KiB), reject unknown actions, log with `slog`.

S3 client setup (in `New`):

```go
cfg, err := awsconfig.LoadDefaultConfig(ctx,
    awsconfig.WithRegion("auto"),
    awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
        c.AccessKeyID, c.SecretKey, "")),
)
s3c := s3.NewFromConfig(cfg, func(o *s3.Options) {
    o.BaseEndpoint = aws.String(c.Endpoint)
    o.UsePathStyle = false
})
presign := s3.NewPresignClient(s3c)
```

#### `finalize` endpoint

`POST /api/upload/finalize` — invoked by the browser via HTMX after the JS finishes the multipart dance. Request form:

- `id` (the UUID returned at `multipart-init`)
- `key` (the full S3 key)
- `name` (display name)

Calls `dao.MarkVideoUploaded(ctx, id)` and returns a templ fragment (rendered with `templ.Handler` or by calling `.Render(ctx, w)` on the component) showing the new row. This gets `hx-swap`-appended to the page's "uploads" list.

### 3. New: `web/upload.templ`

```templ
package web

import (
    "tangled.org/xeiaso.net/videosite/internal/models"
)

templ Upload() {
    <h2>Upload a video</h2>

    <div x-data="uploader()">
        <input type="file" accept="video/*"
               @change="enqueue($event.target.files)" multiple />

        <template x-for="u in items" :key="u.id">
            <div class="upload-row">
                <span x-text="u.name"></span>
                <progress :value="u.pct" max="100"></progress>
                <span x-text="u.status"></span>
            </div>
        </template>
    </div>

    <ul id="uploads" hx-swap-oob="true"></ul>

    <script src="/static/js/upload.js" type="module"></script>
}

templ UploadedRow(v *models.Video) {
    <li>
        <code>{ v.ID }</code> — { string(v.Status) }
    </li>
}
```

The `Upload()` page renders the form and the `<ul id="uploads">` list. `UploadedRow` is the HTMX response fragment from `/api/upload/finalize`.

### 4. New: `web/static/js/upload.js`

Vanilla JS (ES module) driving Alpine. Mirrors `client.ts` from the Tigris SDK. Key points:

- Use `XMLHttpRequest` (not `fetch`) for `xhr.upload.onprogress`.
- Always multipart, default `partSize = 5 * 1024 * 1024`, default `concurrency = 4`.
- `PUT` raw chunks (`file.slice(start, end)`) to presigned URLs.
- Strip surrounding quotes from each part's `ETag` response header before posting back.
- After `multipart-complete` succeeds, call `htmx.ajax("POST", "/api/upload/finalize", {...})` with `target: "#uploads", swap: "beforeend"` so the server-rendered row gets appended.
- Expose `window.uploader = () => ({ items: [...], async enqueue(files) { ... } })` for the Alpine `x-data="uploader()"` directive.

The full JS skeleton was sketched in the prior report; lift it directly with two changes:

1. The `multipart-init` response now includes `key` and `id` — store both on the upload-state object and pass `key` (not `name`) as the `name` field in subsequent broker calls. (The wire field is named `name` but it really means "object key" — keeping the JS SDK's naming.)
2. Always multipart, no size threshold.

### 5. Edit: `cmd/videosite/main.go`

Three changes:

1. **Add credential flags** alongside the existing block:

   ```go
   tigrisEndpoint    = flag.String("tigris-endpoint", "https://t3.storage.dev", "Tigris S3 endpoint")
   tigrisAccessKeyID = flag.String("tigris-access-key-id", "", "Tigris access key ID")
   tigrisSecretKey   = flag.String("tigris-secret-access-key", "", "Tigris secret access key")
   ```

   `flagenv` will already pick up `TIGRIS_ENDPOINT`, `TIGRIS_ACCESS_KEY_ID`, `TIGRIS_SECRET_ACCESS_KEY`.

2. **Mount Alpine + the upload handler** (existing `Mount` calls are at lines 56–57):

   ```go
   alpinejs.Mount(mux)
   uploadH, err := upload.New(upload.Config{
       Bucket:      *bucketName,
       Endpoint:    *tigrisEndpoint,
       AccessKeyID: *tigrisAccessKeyID,
       SecretKey:   *tigrisSecretKey,
       URLBase:     *bucketURLBase,
       Expires:     time.Hour,
   }, dao, lg.With("component", "upload"))
   if err != nil { log.Fatal(err) }
   uploadH.Mount(mux)
   ```

   Drop the `_ = dao` line.

3. **Register `/upload` route** before the catch-all:
   ```go
   mux.Handle("/upload", templ.Handler(
       xess.Base(
           "Upload | videosite",
           web.HeadArea(),
           web.Navbar(),
           web.Upload(),
           web.Footer(),
       ),
   ))
   ```

### 6. New: `go.mod` deps

```
github.com/aws/aws-sdk-go-v2
github.com/aws/aws-sdk-go-v2/config
github.com/aws/aws-sdk-go-v2/credentials
github.com/aws/aws-sdk-go-v2/service/s3
github.com/google/uuid
```

Run `go mod tidy` after editing.

### 7. CORS configuration (out-of-band, document only)

The Tigris bucket needs CORS rules permitting `PUT` from the site's origin, `Access-Control-Expose-Headers: ETag`, and a generous `Access-Control-Allow-Headers: *`. Without `ETag` exposed, multipart breaks because the JS can't read part etags. This is a one-time bucket setup via `tigris bucket cors set` (see `tigris-storage:tigris-buckets` skill) — call it out in a follow-up ticket but don't try to wire it from the Go code.

---

## Critical existing code to reuse

- `internal/htmx/htmx.go:27` — `Mount` shape to mirror in `internal/upload`.
- `internal/htmx/htmx.go:51` — `htmx.Is(r)` is available if a handler ever needs to branch on HTMX vs full-page (not needed for upload but worth knowing).
- `internal/alpinejs/alpinejs.go:23` — needs `Mount(mux)` called from `main.go`; otherwise the embedded JS 404s.
- `internal/xess/xess.templ:3` — `xess.Base(title, headArea, navBar, bodyArea, footer)` is the page wrapper; reuse it for `/upload`.
- `web/index.templ:48-51` — `web.HeadArea()` already pulls in HTMX + Alpine. Don't duplicate.
- `internal/models/dao.go:13` — `*DAO` struct; new methods go in this package.
- `cmd/videosite/main.go:28` — `flagenv.Parse()` already runs before `flag.Parse()`, so new flags get env-var support for free.

---

## Verification

1. **Build & generate**:
   ```sh
   go generate ./...   # regenerates *_templ.go for new web/upload.templ
   go build ./...
   go vet ./...
   ```
2. **Set up creds locally** (`.envrc` or shell):
   ```sh
   export TIGRIS_ACCESS_KEY_ID=tid_...
   export TIGRIS_SECRET_ACCESS_KEY=tsec_...
   export BUCKET_NAME=xe-videosite
   ```
3. **Set bucket CORS once** (using the `tigris-storage:tigris-buckets` skill flow): allow `PUT` from `http://localhost:8080`, expose `ETag`.
4. **Run server**: `go run ./cmd/videosite`.
5. **Smoke test the page**:
   - Visit `/upload`. Should render with HTMX + Alpine both loaded (DevTools Network: `htmx.js` and `alpine-3.5.11.js` both 200, neither 404).
   - Pick a small (<5MB) video file. Watch DevTools: 1 `multipart-init`, 1 `multipart-get-parts`, N `PUT` requests to `https://xe-videosite.t3.storage.dev/...`, 1 `multipart-complete`, 1 `finalize`.
   - Pick a large (>500MB) video file. Confirm parts upload in parallel (4 simultaneous `PUT`s in waterfall) and progress bar advances smoothly.
   - On completion, `<ul id="uploads">` gains a row showing the UUID and `uploaded` status. The row came from the server (view-source: it's HTML, not JS-rendered).
6. **Verify DB write**: `sqlite3 ./var/data.db 'select id, status, created_at from videos;'` — should show the new row.
7. **Verify object exists**: `t3 ls --bucket xe-videosite raw/` — should show `raw/<uuid>/<filename>`.
8. **Failure cases to test manually**:
   - Drop network during a part upload → upload reports error, no `finalize` call, DB row exists in `uploaded` state (acceptable for now; cleanup is a follow-up).
   - POST garbage JSON to `/api/upload` → 400, no panic.
   - POST `{"action":"bogus"}` → 400 with `Invalid action`.
9. **No regressions**: visit `/` — video still plays, navbar still shows Upload link.

---

## Deviations from plan during implementation

The plan above is preserved as-written for historical reference. The following changes were made during implementation:

### Server struct extraction (`cmd/videosite/server.go`)

The plan put route wiring inline in `main.go`. Mid-implementation we extracted a `Server` struct (in a new file `cmd/videosite/server.go`) that owns the DAO, the upload handler, and route registration. `main.go` now only parses flags and calls `NewServer(...).Routes()`. Future handlers that need the DAO will be methods on `*Server`.

### `.env` autoload

Added `_ "github.com/joho/godotenv/autoload"` to `main.go` and `.env` to `.gitignore`. Lets local development read credentials from `.env` without exporting them in the shell. Plan didn't mention this; it became necessary as soon as we had real Tigris credentials to manage.

### Flag rename

Plan named the credential flags `tigris-endpoint`, `tigris-access-key-id`, `tigris-secret-access-key`. Final names are `tigris-storage-endpoint`, `tigris-storage-access-key-id`, `tigris-storage-secret-access-key` — matching the env-var convention used by Tigris-published examples (`TIGRIS_STORAGE_*`).

### Alpine.js bug fixes

Two pre-existing bugs in `internal/alpinejs/` were fixed during this work:

1. `URL` constant was `/.within.website/x/web/alpinejs` (no trailing slash). With `http.StripPrefix(URL, ...)` this means the prefix is stripped only on exact-match requests, not on `URL + "alpine-3.5.11.js"`. Added a trailing slash so the file actually serves.
2. `<script>` tag was missing `defer`. Without it, the script can execute before `body` exists, and — more importantly for our use case — the `alpine:init` event the upload component listens on can fire before the upload module finishes registering its data component. Added `defer`.

### JS component registration via `alpine:init`

Plan said the JS would expose `window.uploader = () => ({...})` for the `x-data="uploader()"` directive. Final implementation registers the component on the `alpine:init` event:

```js
document.addEventListener("alpine:init", () => {
  window.Alpine.data("uploader", () => ({...}));
});
```

This is the documented Alpine pattern and avoids a load-order race: `x-data="uploader"` (no parens) looks up the named component, which Alpine resolves at component-init time.

### Stage/submit UX instead of immediate upload

Plan's `Upload()` template auto-uploaded on file pick (`@change="enqueue($event.target.files)"`). Final UX has a stage/submit split: file pick adds rows with status `ready`, an explicit Upload button drains them (`stage()` + `submit()`), and a Clear button discards staged rows before submitting. Both buttons disable during upload.
