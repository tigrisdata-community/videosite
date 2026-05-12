# Source-aware encoder variants

## Context

`internal/encoder/ffmpeg.go` currently hardcodes a two-rendition ladder (1080p @ 10000k and 540p @ 3500k) regardless of the source's resolution. Two problems with that:

1. **Wasted bits on small sources.** A 720p source gets upscaled to 1080p, blowing pixels we don't have. A 480p source likewise wastes the 540p slot upscaling slightly.
2. **No high-quality top variant for 4K/1440p sources.** Anything over 1080p loses detail it could keep.

We want to screen-scrape `ffprobe` for source width/height and build the ladder dynamically:

- Always emit a **source-resolution** variant (re-encoded to NVENC H.264 at a bitrate scaled from pixel area).
- Emit **1080p**, **540p**, and **240p** variants only when source height is strictly greater than the target. (Per user clarification: skip any variant whose height is ≥ source height.)

## Approach

Split the work in two: a probe step that runs `ffprobe` once, then a pure args builder that takes the probe result and produces the variant ladder. Keeping selection separate from arg-stringing means we can unit-test the rules with golden tables without spawning ffprobe.

### 1. `internal/encoder/ffmpeg.go`

Add probe support and refactor `FFmpegArgs`:

```go
type ProbeResult struct {
    Width  int
    Height int
}

// Probe runs `ffprobe` and returns the first video stream's dimensions.
func Probe(ctx context.Context, inputPath string) (ProbeResult, error)

type VideoVariant struct {
    Height  int // target height; 0 means "match source, no scale filter"
    Bitrate int // kbps
}

// PlanVariants returns the renditions to emit for the given source.
// Rules:
//   - Always include source-height variant.
//   - 1080p, 540p, 240p included only if source.Height > target.
//   - Source-variant bitrate scales by pixel area against the 1080p
//     reference (10000k @ 1920*1080), clamped to [3000, 30000] kbps.
func PlanVariants(p ProbeResult) []VideoVariant

// FFmpegArgs now takes the variant list. Audio ladder stays fixed (96k + 64k).
func FFmpegArgs(inputPath, outputPath string, variants []VideoVariant) []string
```

Internal helpers:

- `Probe` invokes `ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of json <input>`, unmarshals `{"streams":[{"width":N,"height":N}]}`, returns the first entry. Error if no video stream.
- `PlanVariants` produces, in order: `{Height: 0, Bitrate: scaled}`, then conditional `{1080, 10000}`, `{540, 3500}`, `{240, 600}`. The leading source variant gives DASH players the best-quality option first.
- `FFmpegArgs` loops variants, emitting `-map 0:v -c:v:N h264_nvenc -b:v:N <Bitrate>k` plus `-filter:v:N scale=-2:<Height>` (omitted when `Height==0`). Audio renditions and the `-adaptation_sets` flag remain unchanged.

Bitrate-scaling formula:

```
br_kbps = clamp(round(width*height / (1920*1080) * 10000), 3000, 30000)
```

Reference points: 4K → ~40000k clamped to 30000k; 1440p → ~17800k; 1080p → 10000k; 720p → 4444k; 480p → ~1975k clamped to 3000k.

### 2. `internal/encoder/ffmpeg_test.go`

Convert to table-driven (per the **go-table-driven-tests** skill). Cases:

- 4K (3840×2160) source → 4 variants: source@30000k, 1080p, 540p, 240p.
- 1080p (1920×1080) source → 3 variants: source@10000k, 540p, 240p. (No 1080p; source serves that role.)
- 720p (1280×720) source → 3 variants: source@~4444k, 540p, 240p.
- 480p (854×480) source → 2 variants: source@3000k (clamped), 240p.
- 240p (426×240) source → 1 variant: source@3000k (clamped).

Each case asserts:

- adjacent-pair presence of `-filter:v:N` / `-b:v:N` / `-c:v:N` per variant index;
- the source-variant has no `-filter:v:0` (when `Height==0`);
- audio block (`-c:a:0 aac`, `-b:a:0 96k`, etc.) and `-f dash` / `-adaptation_sets` unchanged;
- last arg is the manifest path.

Add a separate `TestPlanVariants` table covering the skip rules above without going through `FFmpegArgs`.

Probe parsing gets its own test against a JSON fixture string (parse a `bytes.Reader` rather than shell out) — extract the JSON-to-struct decode into an unexported helper so we don't need ffprobe at test time.

### 3. `cmd/videosite-encoder/main.go`

In `run`, after the source has been downloaded:

```go
probe, err := encoder.Probe(ctx, srcPath)
if err != nil {
    return "", fmt.Errorf("ffprobe: %w", err)
}
lg.Info("probed source", "width", probe.Width, "height", probe.Height)

variants := encoder.PlanVariants(probe)
args := encoder.FFmpegArgs(srcPath, manifestPath, variants)
```

No other call sites change. The encoder Docker image already builds `ffprobe` alongside `ffmpeg` from the FFmpeg source tree (`docker/encoder.Dockerfile`), and `/app/ffmpeg/bin` is on `PATH`, so the binary is available without Dockerfile changes.

## Critical files

- `internal/encoder/ffmpeg.go` — add `Probe`, `ProbeResult`, `VideoVariant`, `PlanVariants`; rewrite `FFmpegArgs`.
- `internal/encoder/ffmpeg_test.go` — rewrite as table-driven covering the new rules.
- `cmd/videosite-encoder/main.go` — call `encoder.Probe` and `encoder.PlanVariants` in `run`.

No DB/model changes, no orchestrator changes, no webhook changes, no Dockerfile changes.

## Verification

1. `go tool task generate` (no templ changes expected, but matches CI).
2. `go tool task test -- ./internal/encoder` — confirm new tests pass.
3. `go tool task test:all` — race build + full suite.
4. Local smoke (optional, requires a GPU host): build the encoder image with `go tool task docker`, run against a known 4K and 720p source, verify the resulting manifest.mpd lists the expected `<Representation height="...">` entries.
5. After deploy, watch a real job: `internal/encoder/orchestrator.go` will log the `probed source` line with the source dimensions; check the resulting bucket prefix `v/<video_id>/` for the expected number of init/segment streams.
