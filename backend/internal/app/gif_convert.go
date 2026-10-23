package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// convertGIFToMP4 is deliberately a staging helper: it does not send anything.
// ffmpeg is invoked without a shell and writes only to a private tempfile.
func convertGIFToMP4(ctx context.Context, gif []byte) ([]byte, error) {
	if len(gif) == 0 || len(gif) > 10<<20 {
		return nil, fmt.Errorf("GIF exceeds conversion input limit")
	}
	in, err := os.CreateTemp("", "whatsfeather-gif-*.gif")
	if err != nil {
		return nil, err
	}
	inPath := in.Name()
	defer os.Remove(inPath)
	if _, err = in.Write(gif); err != nil {
		in.Close()
		return nil, err
	}
	if err = in.Close(); err != nil {
		return nil, err
	}
	out, err := os.CreateTemp("", "whatsfeather-gif-*.mp4")
	if err != nil {
		return nil, err
	}
	outPath := out.Name()
	out.Close()
	defer os.Remove(outPath)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1", "-f", "gif", "-i", inPath, "-an", "-t", "15", "-vf", "scale=min(640\\,iw):-2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-y", outPath)
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("GIF conversion unavailable: %w", err)
	}
	st, err := os.Stat(outPath)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > 20<<20 {
		return nil, fmt.Errorf("converted GIF exceeds output limit")
	}
	return os.ReadFile(outPath)
}
