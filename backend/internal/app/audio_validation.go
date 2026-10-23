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

const audioValidationLimit int64 = 20 << 20
const audioDurationLimitSeconds = 30 * 60
const audioMIME = "audio/ogg; codecs=opus"

type validatedAudio struct {
	Duration int64
	Codec    string
}

type audioProbeResult struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
		Duration   string `json:"duration"`
	} `json:"streams"`
}

// validateAudioSnapshot validates a private byte snapshot. Preparation hashes
// and stages this same byte slice after validation, so changes to the caller's
// source cannot swap content between probing and staging.
func validateAudioSnapshot(ctx context.Context, profileDir string, data []byte) (validatedAudio, error) {
	var zero validatedAudio
	if len(data) == 0 || int64(len(data)) > audioValidationLimit {
		return zero, fmt.Errorf("audio must be a regular OGG/Opus file no larger than 20 MiB")
	}
	file, err := os.CreateTemp(profileDir, ".audio-validation-*")
	if err != nil {
		return zero, fmt.Errorf("audio snapshot could not be created")
	}
	path := file.Name()
	defer os.Remove(path)
	if err := os.Remove(path); err != nil {
		file.Close()
		return zero, fmt.Errorf("audio snapshot could not be isolated")
	}
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		file.Close()
		return zero, fmt.Errorf("audio snapshot could not be written")
	}
	validated, validationErr := validateAudioFile(ctx, file)
	closeErr := file.Close()
	if validationErr != nil {
		return zero, validationErr
	}
	if closeErr != nil {
		return zero, fmt.Errorf("audio snapshot could not be closed")
	}
	return validated, nil
}

func validateAudioFile(ctx context.Context, file *os.File) (validatedAudio, error) {
	var zero validatedAudio
	if file == nil {
		return zero, fmt.Errorf("audio file is unavailable")
	}
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > audioValidationLimit {
		return zero, fmt.Errorf("audio must be a regular OGG/Opus file no larger than 20 MiB")
	}
	var signature [4]byte
	if n, readErr := file.ReadAt(signature[:], 0); readErr != nil || n != len(signature) || string(signature[:]) != "OggS" {
		return zero, fmt.Errorf("audio container must be OGG")
	}
	probePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return zero, fmt.Errorf("audio validation is unavailable: ffprobe is not installed")
	}
	decodePath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return zero, fmt.Errorf("audio validation is unavailable: ffmpeg is not installed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return zero, fmt.Errorf("audio file cannot be inspected")
	}
	probe := exec.CommandContext(bounded, probePath,
		"-v", "error", "-protocol_whitelist", "file,pipe",
		"-show_entries", "format=format_name,duration:stream=codec_type,codec_name,sample_rate,channels,duration",
		"-of", "json", "/dev/fd/3")
	probe.ExtraFiles = []*os.File{file}
	probe.Stderr = io.Discard
	output, err := probe.Output()
	if err != nil {
		return zero, fmt.Errorf("audio container could not be probed")
	}
	var info audioProbeResult
	if err = json.Unmarshal(output, &info); err != nil {
		return zero, fmt.Errorf("audio metadata is malformed")
	}
	isOGG := false
	for _, format := range strings.Split(info.Format.FormatName, ",") {
		if format == "ogg" {
			isOGG = true
			break
		}
	}
	if !isOGG || len(info.Streams) != 1 || info.Streams[0].CodecType != "audio" ||
		info.Streams[0].CodecName != "opus" || info.Streams[0].SampleRate != "48000" ||
		info.Streams[0].Channels != 1 {
		return zero, fmt.Errorf("audio must contain one mono 48 kHz Opus stream in an OGG container")
	}
	duration := info.Format.Duration
	if duration == "" {
		duration = info.Streams[0].Duration
	}
	seconds, err := strconv.ParseFloat(duration, 64)
	if err != nil || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds > audioDurationLimitSeconds {
		return zero, fmt.Errorf("audio duration is unknown or exceeds 30 minutes")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return zero, fmt.Errorf("audio file cannot be decoded")
	}
	decode := exec.CommandContext(bounded, decodePath,
		"-hide_banner", "-v", "error", "-xerror", "-nostdin", "-threads", "2",
		"-protocol_whitelist", "file,pipe", "-i", "/dev/fd/3", "-map", "0:a:0", "-f", "null", "-")
	decode.ExtraFiles = []*os.File{file}
	decode.Stdout = io.Discard
	decode.Stderr = io.Discard
	if err := decode.Run(); err != nil {
		if bounded.Err() != nil {
			return zero, fmt.Errorf("audio validation exceeded its 15-second limit")
		}
		return zero, fmt.Errorf("audio contains malformed or undecodable packets")
	}
	return validatedAudio{Duration: int64(math.Ceil(seconds)), Codec: "opus"}, nil
}
