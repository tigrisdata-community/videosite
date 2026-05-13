package encoder

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseProbe(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    ProbeResult
		wantErr error
	}{
		{
			name: "1080p stream",
			json: `{"streams":[{"width":1920,"height":1080}]}`,
			want: ProbeResult{Width: 1920, Height: 1080},
		},
		{
			name: "4K stream",
			json: `{"streams":[{"width":3840,"height":2160}]}`,
			want: ProbeResult{Width: 3840, Height: 2160},
		},
		{
			name:    "no streams",
			json:    `{"streams":[]}`,
			wantErr: ErrNoVideoStream,
		},
		{
			name:    "zero dimensions",
			json:    `{"streams":[{"width":0,"height":0}]}`,
			wantErr: nil, // matched by substring
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProbe(strings.NewReader(tt.json))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Logf("want: %v", tt.wantErr)
					t.Logf("got:  %v", err)
					t.Errorf("wrong error")
				}
				return
			}
			if tt.name == "zero dimensions" {
				if err == nil || !strings.Contains(err.Error(), "invalid stream dimensions") {
					t.Errorf("want invalid-dimensions error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanVariants(t *testing.T) {
	tests := []struct {
		name   string
		probe  ProbeResult
		want   []VideoVariant
	}{
		{
			name:  "4K source",
			probe: ProbeResult{Width: 3840, Height: 2160},
			want: []VideoVariant{
				{Height: 0, Bitrate: 30000},
				{Height: 1080, Bitrate: 10000},
				{Height: 540, Bitrate: 3500},
				{Height: 240, Bitrate: 600},
			},
		},
		{
			name:  "1080p source skips 1080p variant",
			probe: ProbeResult{Width: 1920, Height: 1080},
			want: []VideoVariant{
				{Height: 0, Bitrate: 10000},
				{Height: 540, Bitrate: 3500},
				{Height: 240, Bitrate: 600},
			},
		},
		{
			name:  "720p source",
			probe: ProbeResult{Width: 1280, Height: 720},
			want: []VideoVariant{
				{Height: 0, Bitrate: 4444},
				{Height: 540, Bitrate: 3500},
				{Height: 240, Bitrate: 600},
			},
		},
		{
			name:  "480p source clamps to min bitrate, skips 540p",
			probe: ProbeResult{Width: 854, Height: 480},
			want: []VideoVariant{
				{Height: 0, Bitrate: 3000},
				{Height: 240, Bitrate: 600},
			},
		},
		{
			name:  "240p source emits only source variant",
			probe: ProbeResult{Width: 426, Height: 240},
			want: []VideoVariant{
				{Height: 0, Bitrate: 3000},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanVariants(tt.probe)
			if !slices.Equal(got, tt.want) {
				t.Logf("want: %+v", tt.want)
				t.Logf("got:  %+v", got)
				t.Errorf("variants differ")
			}
		})
	}
}

func TestFFmpegArgs(t *testing.T) {
	tests := []struct {
		name     string
		variants []VideoVariant
		// wantPairs are adjacent argument pairs that must appear.
		wantPairs [][]string
		// wantAbsent are arguments that must NOT appear anywhere.
		wantAbsent []string
	}{
		{
			name: "4K ladder",
			variants: []VideoVariant{
				{Height: 0, Bitrate: 30000},
				{Height: 1080, Bitrate: 10000},
				{Height: 540, Bitrate: 3500},
				{Height: 240, Bitrate: 600},
			},
			wantPairs: [][]string{
				{"-i", "/tmp/source.mp4"},
				{"-c:v:0", "h264_nvenc"}, {"-b:v:0", "30000k"},
				{"-c:v:1", "h264_nvenc"}, {"-b:v:1", "10000k"}, {"-filter:v:1", "scale=-2:1080"},
				{"-c:v:2", "h264_nvenc"}, {"-b:v:2", "3500k"}, {"-filter:v:2", "scale=-2:540"},
				{"-c:v:3", "h264_nvenc"}, {"-b:v:3", "600k"}, {"-filter:v:3", "scale=-2:240"},
				{"-c:a:0", "aac"}, {"-b:a:0", "96k"},
				{"-c:a:1", "aac"}, {"-b:a:1", "64k"},
				{"-f", "dash"},
				{"-adaptation_sets", "id=0,streams=v id=1,streams=a"},
			},
			// Source variant (index 0) must not carry a scale filter.
			wantAbsent: []string{"-filter:v:0"},
		},
		{
			name: "1080p source ladder",
			variants: []VideoVariant{
				{Height: 0, Bitrate: 10000},
				{Height: 540, Bitrate: 3500},
				{Height: 240, Bitrate: 600},
			},
			wantPairs: [][]string{
				{"-b:v:0", "10000k"},
				{"-b:v:1", "3500k"}, {"-filter:v:1", "scale=-2:540"},
				{"-b:v:2", "600k"}, {"-filter:v:2", "scale=-2:240"},
			},
			wantAbsent: []string{"-filter:v:0", "-c:v:3"},
		},
		{
			name: "240p source emits only one video rendition",
			variants: []VideoVariant{
				{Height: 0, Bitrate: 3000},
			},
			wantPairs: [][]string{
				{"-b:v:0", "3000k"},
				{"-c:a:0", "aac"},
				{"-f", "dash"},
			},
			wantAbsent: []string{"-filter:v:0", "-c:v:1", "-b:v:1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := FFmpegArgs("/tmp/source.mp4", "/tmp/dash/manifest.mpd", tt.variants)
			for _, pair := range tt.wantPairs {
				if !containsAdjacent(args, pair) {
					t.Errorf("args missing adjacent pair %v\nargs: %v", pair, args)
				}
			}
			for _, tok := range tt.wantAbsent {
				if slices.Contains(args, tok) {
					t.Errorf("args contain forbidden token %q\nargs: %v", tok, args)
				}
			}
			if args[len(args)-1] != "/tmp/dash/manifest.mpd" {
				t.Errorf("last arg = %q, want manifest path", args[len(args)-1])
			}
		})
	}
}

func containsAdjacent(haystack, needle []string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}
