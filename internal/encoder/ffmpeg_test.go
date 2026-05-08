package encoder

import (
	"slices"
	"testing"
)

func TestFFmpegArgs(t *testing.T) {
	args := FFmpegArgs("/tmp/source.mp4", "/tmp/dash/manifest.mpd")

	wantContains := [][]string{
		{"-i", "/tmp/source.mp4"},
		{"-c:v:0", "h264_nvenc"},
		{"-c:v:1", "h264_nvenc"},
		{"-b:v:0", "10000k"},
		{"-b:v:1", "3500k"},
		{"-filter:v:0", "scale=-2:1080"},
		{"-filter:v:1", "scale=-2:540"},
		{"-c:a:0", "aac"}, {"-b:a:0", "96k"},
		{"-c:a:1", "aac"}, {"-b:a:1", "64k"},
		{"-f", "dash"},
		{"-adaptation_sets", "id=0,streams=v id=1,streams=a"},
	}
	for _, pair := range wantContains {
		if !containsAdjacent(args, pair) {
			t.Errorf("args missing adjacent pair %v\nargs: %v", pair, args)
		}
	}
	if args[len(args)-1] != "/tmp/dash/manifest.mpd" {
		t.Errorf("last arg = %q, want manifest path", args[len(args)-1])
	}
}

func TestContentTypeFor(t *testing.T) {
	tests := map[string]string{
		"manifest.mpd":            "application/dash+xml",
		"chunk-stream0-00001.m4s": "video/iso.segment",
		"init-stream0.mp4":        "video/mp4",
		"random.bin":              "application/octet-stream",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := ContentTypeFor(in); got != want {
				t.Errorf("got %q, want %q", got, want)
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
