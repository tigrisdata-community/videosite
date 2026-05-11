package encoder

// FFmpegArgs returns the argv (excluding the "ffmpeg" program name) for a
// two-rendition DASH encode using NVENC. The manifest goes to outputPath
// (typically ".../manifest.mpd"); segments are written next to it.
func FFmpegArgs(inputPath, outputPath string) []string {
	return []string{
		"-y", "-hwaccel", "cuda", "-i", inputPath,
		"-map", "0:v", "-c:v:0", "h264_nvenc", "-b:v:0", "10000k", "-filter:v:0", "scale=-2:1080",
		"-map", "0:v", "-c:v:1", "h264_nvenc", "-b:v:1", "3500k", "-filter:v:1", "scale=-2:540",
		"-map", "0:a", "-c:a:0", "aac", "-b:a:0", "96k",
		"-map", "0:a", "-c:a:1", "aac", "-b:a:1", "64k",
		"-f", "dash",
		"-adaptation_sets", "id=0,streams=v id=1,streams=a",
		outputPath,
	}
}
