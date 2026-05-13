package encoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
)

// ErrNoVideoStream means ffprobe returned no video streams for the input.
var ErrNoVideoStream = errors.New("encoder: no video stream")

// ProbeResult is the dimensions of the source's first video stream.
type ProbeResult struct {
	Width  int
	Height int
}

// VideoVariant is one rendition in the output DASH ladder.
type VideoVariant struct {
	// Height is the target output height. 0 means "match source" — no
	// scale filter is emitted, so ffmpeg copies the source resolution.
	Height int
	// Bitrate is the target video bitrate in kbps.
	Bitrate int
}

// Probe runs ffprobe against inputPath and returns the first video
// stream's width and height.
func Probe(ctx context.Context, inputPath string) (ProbeResult, error) {
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "json",
		inputPath,
	).Output()
	if err != nil {
		return ProbeResult{}, fmt.Errorf("ffprobe: %w", err)
	}
	return parseProbe(bytes.NewReader(out))
}

func parseProbe(r io.Reader) (ProbeResult, error) {
	var doc struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return ProbeResult{}, fmt.Errorf("decode ffprobe json: %w", err)
	}
	if len(doc.Streams) == 0 {
		return ProbeResult{}, ErrNoVideoStream
	}
	s := doc.Streams[0]
	if s.Width <= 0 || s.Height <= 0 {
		return ProbeResult{}, fmt.Errorf("encoder: invalid stream dimensions %dx%d", s.Width, s.Height)
	}
	return ProbeResult{Width: s.Width, Height: s.Height}, nil
}

// PlanVariants returns the DASH renditions to emit for the given source.
//
// Always includes a source-resolution variant (first), then 1080p / 540p /
// 240p only when source.Height is strictly greater than each target.
// The source variant's bitrate scales by pixel area against the 1080p
// reference (10000 kbps @ 1920*1080), clamped to [3000, 30000] kbps.
func PlanVariants(p ProbeResult) []VideoVariant {
	out := []VideoVariant{{Height: 0, Bitrate: sourceBitrate(p)}}
	for _, v := range []VideoVariant{
		{Height: 1080, Bitrate: 10000},
		{Height: 540, Bitrate: 3500},
		{Height: 240, Bitrate: 600},
	} {
		if p.Height > v.Height {
			out = append(out, v)
		}
	}
	return out
}

func sourceBitrate(p ProbeResult) int {
	const (
		refPixels   = 1920 * 1080
		refBitrate  = 10000
		minBitrate  = 3000
		maxBitrate  = 30000
	)
	scaled := int(math.Round(float64(p.Width*p.Height) / float64(refPixels) * float64(refBitrate)))
	if scaled < minBitrate {
		return minBitrate
	}
	if scaled > maxBitrate {
		return maxBitrate
	}
	return scaled
}

// FFmpegArgs returns the argv (excluding the "ffmpeg" program name) for a
// DASH encode using NVENC. The manifest goes to outputPath (typically
// ".../manifest.mpd"); segments are written next to it. variants drives the
// video rendition ladder; the audio ladder is fixed at 96k + 64k.
func FFmpegArgs(inputPath, outputPath string, variants []VideoVariant) []string {
	args := []string{"-y", "-hwaccel", "cuda", "-i", inputPath}
	for i, v := range variants {
		idx := strconv.Itoa(i)
		args = append(args,
			"-map", "0:v",
			"-c:v:"+idx, "h264_nvenc",
			"-b:v:"+idx, strconv.Itoa(v.Bitrate)+"k",
		)
		if v.Height > 0 {
			args = append(args, "-filter:v:"+idx, "scale=-2:"+strconv.Itoa(v.Height))
		}
	}
	args = append(args,
		"-map", "0:a", "-c:a:0", "aac", "-b:a:0", "96k",
		"-map", "0:a", "-c:a:1", "aac", "-b:a:1", "64k",
		"-f", "dash",
		"-adaptation_sets", "id=0,streams=v id=1,streams=a",
		outputPath,
	)
	return args
}
