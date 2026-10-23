package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const videoValidationLimit int64 = 20 << 20

// Video support is deliberately narrow: MP4/ISO-BMFF with one H.264 AVC
// yuv420p video stream and at most one AAC-LC audio stream. ffprobe inspects the
// container/codec metadata and ffmpeg decodes the complete streams before bytes
// can be staged or sent.
type validatedVideo struct {
	Width      int
	Height     int
	Duration   int64
	VideoCodec string
	AudioCodec string
}

type videoProbeResult struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType   string `json:"codec_type"`
		CodecName   string `json:"codec_name"`
		Profile     string `json:"profile"`
		CodecTag    string `json:"codec_tag_string"`
		PixelFormat string `json:"pix_fmt"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Duration    string `json:"duration"`
	} `json:"streams"`
}

func videoRuntimeAvailable() bool {
	_, probeErr := exec.LookPath("ffprobe")
	_, decodeErr := exec.LookPath("ffmpeg")
	return probeErr == nil && decodeErr == nil
}

func supportedMP4Brand(file *os.File) bool {
	var header [12]byte
	if n, err := file.ReadAt(header[:], 0); err != nil || n != len(header) || string(header[4:8]) != "ftyp" {
		return false
	}
	boxSize := uint32(header[0])<<24 | uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3])
	if boxSize < 16 {
		return false
	}
	switch string(header[8:12]) {
	case "isom", "iso2", "mp41", "mp42", "avc1":
		return true
	default:
		return false
	}
}

func validateVideoFile(ctx context.Context, file *os.File) (validatedVideo, error) {
	var zero validatedVideo
	if file == nil {
		return zero, fmt.Errorf("video file is unavailable")
	}
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > videoValidationLimit {
		return zero, fmt.Errorf("video must be a regular MP4 file no larger than 20 MiB")
	}
	if !supportedMP4Brand(file) {
		return zero, fmt.Errorf("video container is not a supported MP4")
	}
	probePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return zero, fmt.Errorf("video validation is unavailable: ffprobe is not installed")
	}
	decodePath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return zero, fmt.Errorf("video validation is unavailable: ffmpeg is not installed")
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return zero, fmt.Errorf("video file cannot be inspected")
	}
	probe := exec.CommandContext(bounded, probePath,
		"-v", "error", "-protocol_whitelist", "file,pipe",
		"-show_entries", "format=format_name,duration:stream=codec_type,codec_name,profile,codec_tag_string,width,height,pix_fmt,duration",
		"-of", "json", "/dev/fd/3")
	probe.ExtraFiles = []*os.File{file}
	probe.Stderr = io.Discard
	output, err := probe.Output()
	if err != nil {
		return zero, fmt.Errorf("video container could not be probed")
	}
	var info videoProbeResult
	if err := json.Unmarshal(output, &info); err != nil {
		return zero, fmt.Errorf("video metadata is malformed")
	}
	isMP4 := false
	for _, format := range strings.Split(info.Format.FormatName, ",") {
		if format == "mp4" {
			isMP4 = true
			break
		}
	}
	if !isMP4 || len(info.Streams) < 1 || len(info.Streams) > 2 {
		return zero, fmt.Errorf("video must be an MP4 with one video stream and at most one audio stream")
	}
	result := validatedVideo{VideoCodec: "h264"}
	videoCount, audioCount := 0, 0
	for _, stream := range info.Streams {
		switch stream.CodecType {
		case "video":
			videoCount++
			profileOK := stream.Profile == "Baseline" || stream.Profile == "Constrained Baseline" || stream.Profile == "Main" || stream.Profile == "High"
			if stream.CodecName != "h264" || (stream.CodecTag != "avc1" && stream.CodecTag != "avc3") || !profileOK || stream.PixelFormat != "yuv420p" {
				return zero, fmt.Errorf("video codec is unsupported; use H.264 AVC 4:2:0")
			}
			if stream.Width < 1 || stream.Height < 1 || stream.Width > 1920 || stream.Height > 1920 || int64(stream.Width)*int64(stream.Height) > 1920*1080 {
				return zero, fmt.Errorf("video dimensions exceed 1920 × 1080 pixels")
			}
			result.Width, result.Height = stream.Width, stream.Height
		case "audio":
			audioCount++
			if stream.CodecName != "aac" || stream.CodecTag != "mp4a" || stream.Profile != "LC" {
				return zero, fmt.Errorf("audio codec is unsupported; use AAC-LC or remove audio")
			}
			result.AudioCodec = "aac"
		default:
			return zero, fmt.Errorf("video contains an unsupported stream type")
		}
	}
	if videoCount != 1 || audioCount > 1 || result.Width == 0 || result.Height == 0 {
		return zero, fmt.Errorf("video must contain exactly one supported H.264 video stream")
	}
	duration := info.Format.Duration
	if duration == "" {
		for _, stream := range info.Streams {
			if stream.CodecType == "video" {
				duration = stream.Duration
				break
			}
		}
	}
	seconds, err := strconv.ParseFloat(duration, 64)
	if err != nil || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return zero, fmt.Errorf("video duration is unknown or invalid")
	}
	result.Duration = int64(math.Ceil(seconds))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return zero, fmt.Errorf("video file cannot be decoded")
	}
	decode := exec.CommandContext(bounded, decodePath,
		"-hide_banner", "-v", "error", "-xerror", "-nostdin", "-threads", "2",
		"-protocol_whitelist", "file,pipe", "-i", "/dev/fd/3", "-map", "0:v:0", "-map", "0:a:0?", "-f", "null", "-")
	decode.ExtraFiles = []*os.File{file}
	decode.Stdout = io.Discard
	decode.Stderr = io.Discard
	if err := decode.Run(); err != nil {
		if bounded.Err() != nil {
			return zero, fmt.Errorf("video validation exceeded its 15-second limit")
		}
		return zero, fmt.Errorf("video contains malformed or undecodable frames")
	}
	return result, nil
}

func applyVideoMetadata(ctx context.Context, file *os.File, meta *Attachment) error {
	validated, err := validateVideoFile(ctx, file)
	if err != nil {
		return fail("invalid_attachment", "Unsupported or malformed video: "+err.Error())
	}
	meta.Kind = "video"
	meta.MIME = "video/mp4"
	meta.Width = validated.Width
	meta.Height = validated.Height
	meta.Duration = validated.Duration
	meta.VideoCodec = validated.VideoCodec
	meta.AudioCodec = validated.AudioCodec
	return nil
}
