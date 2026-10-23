package app

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestGIFToMP4ConversionProducesAnimatedPlaybackEnvelope(t *testing.T) {
	var src bytes.Buffer
	pal := color.Palette{color.Black, color.White}
	g := &gif.GIF{}
	for i := 0; i < 4; i++ {
		im := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
		im.SetColorIndex(i%8, i%8, 1)
		g.Image = append(g.Image, im)
		g.Delay = append(g.Delay, 5)
	}
	if err := gif.EncodeAll(&src, g); err != nil {
		t.Fatal(err)
	}
	mp4, err := convertGIFToMP4(context.Background(), src.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name,width,height,nb_frames", "-of", "default=noprint_wrappers=1:nokey=1", "-i", "pipe:0")
	probe.Stdin = bytes.NewReader(mp4)
	out, err := probe.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(out))
	if len(lines) < 4 || lines[0] != "h264" || atoi(lines[1]) > 640 || atoi(lines[3]) < 4 {
		t.Fatalf("unexpected GifPlayback envelope: %q", out)
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
