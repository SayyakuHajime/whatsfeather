package whatsapp

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestActualSendGIFEncoderProducesPlayableMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	root := t.TempDir()
	path := filepath.Join(root, "input.gif")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 64, 64), palette)
	second := image.NewPaletted(image.Rect(0, 0, 64, 64), palette)
	for i := range second.Pix {
		second.Pix[i] = 1
	}
	if err = gif.EncodeAll(f, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{20, 20}, LoopCount: 0}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	out, err := animatedGIFMP4(context.Background(), input, root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	data, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-show_entries", "stream=codec_name,width,height,nb_read_frames", "-of", "json", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Streams []struct {
			Codec  string `json:"codec_name"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
			Frames string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Streams) != 1 {
		t.Fatalf("unexpected streams: %s", data)
	}
	stream := result.Streams[0]
	frames, _ := strconv.Atoi(stream.Frames)
	if stream.Codec != "h264" || frames < 2 || stream.Width > 640 || stream.Height > 640 {
		t.Fatalf("invalid actual send conversion: %s", data)
	}
}
