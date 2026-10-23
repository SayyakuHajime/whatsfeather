package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const prepareSourceLimit int64 = 64 << 20
const maxPixels int64 = 16 << 20
const StickerMaxBytes int64 = 100_000
const maxStickerBytes = StickerMaxBytes

// prepareAttachment performs all CPU/file work without retaining App.mu. It intentionally
// stages a private copy; callers can safely delete or change their source afterwards.
func (a *App) prepare(ctx context.Context, raw []byte) (any, error) {
	return a.prepareWithUniqueStage(ctx, raw, false)
}

func (a *App) prepareWithUniqueStage(ctx context.Context, raw []byte, uniqueStage bool) (any, error) {
	var p struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Mode == "" {
		p.Mode = "photo"
	}
	if p.Mode != "photo" && p.Mode != "original" && p.Mode != "document" && p.Mode != "video" && p.Mode != "sticker" && p.Mode != "audio" && p.Mode != "voice-note" {
		return nil, fail("invalid_params", "Unsupported attachment mode")
	}
	if !filepath.IsAbs(p.Path) {
		return nil, fail("invalid_attachment", "Choose a local file")
	}
	f, err := os.OpenFile(p.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fail("invalid_attachment", "File is unavailable or is a symbolic link")
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > prepareSourceLimit {
		f.Close()
		return nil, fail("invalid_attachment", "Choose a regular, non-empty file up to 64 MiB")
	}
	sourceSize := st.Size()
	if p.Mode == "sticker" {
		return a.prepareSticker(ctx, f, sourceSize, p.Path)
	}
	var video *validatedVideo
	if p.Mode == "video" {
		if sourceSize > attachmentLimit {
			f.Close()
			return nil, fail("invalid_attachment", "Video must be no larger than 20 MiB")
		}
		validated, validationErr := validateVideoFile(ctx, f)
		if validationErr != nil {
			f.Close()
			return nil, fail("invalid_attachment", "Unsupported or malformed video: "+validationErr.Error())
		}
		video = &validated
	}
	var audio *validatedAudio
	var data []byte
	audioSnapshot := false
	if p.Mode == "audio" || p.Mode == "voice-note" {
		if sourceSize > audioValidationLimit {
			f.Close()
			return nil, fail("invalid_attachment", "Audio must be no larger than 20 MiB")
		}
		data, err = io.ReadAll(io.LimitReader(f, audioValidationLimit+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil || int64(len(data)) != sourceSize {
			return nil, fail("invalid_attachment", "Audio file changed while preparing")
		}
		validated, validationErr := validateAudioSnapshot(ctx, a.dir, data)
		if validationErr != nil {
			return nil, fail("invalid_attachment", "Unsupported or malformed audio: "+validationErr.Error())
		}
		audio = &validated
		audioSnapshot = true
	}
	head := make([]byte, 512)
	n := 0
	if !audioSnapshot {
		n, _ = f.Read(head)
		_, _ = f.Seek(0, 0)
	}
	mime := http.DetectContentType(head[:n])
	if n >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		mime = "image/webp"
	}
	kind := "document"
	if strings.HasPrefix(mime, "image/") {
		kind = "image"
	}
	if mime == "image/gif" {
		kind = "gif"
	}
	if video != nil {
		kind, mime = "video", "video/mp4"
	}
	if audio != nil {
		kind, mime = "audio", audioMIME
	}
	name := filepath.Base(p.Path)
	if name == "." || name == string(filepath.Separator) {
		f.Close()
		return nil, fail("invalid_attachment", "Unsafe filename")
	}
	if p.Mode == "photo" && (kind == "gif" || kind == "document") {
		f.Close()
		return nil, fail("invalid_attachment", "Photo mode requires a still image")
	}
	var cfg image.Config
	if kind == "image" || kind == "gif" {
		cfg, _, err = image.DecodeConfig(f)
		if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
			f.Close()
			return nil, fail("invalid_attachment", "Image dimensions exceed the safe limit")
		}
		_, _ = f.Seek(0, 0)
	}
	if !audioSnapshot {
		data, err = io.ReadAll(io.LimitReader(f, prepareSourceLimit+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil || int64(len(data)) != sourceSize {
			return nil, fail("invalid_attachment", "File changed while preparing")
		}
	}
	if p.Mode == "photo" && mime != "image/gif" {
		data, mime, cfg, err = compressStill(data, mime)
		if err != nil {
			return nil, fail("invalid_attachment", "Image could not be prepared")
		}
	}
	if len(data) > attachmentLimit {
		return nil, fail("media_too_large", "Prepared attachment exceeds 20 MiB")
	}
	stage := a.stagePrepared
	if uniqueStage {
		stage = a.stagePreparedUnique
	}
	path, err := stage(ctx, data, name)
	if err != nil {
		return nil, err
	}
	meta := Attachment{Name: name, Size: sourceSize, OriginalSize: sourceSize, PreparedSize: int64(len(data)), Compressed: int64(len(data)) < sourceSize, Mode: p.Mode, MIME: mime, Kind: kind, URL: fileURL(path), PreparedPath: path, State: "ready"}
	if video != nil {
		meta.Width, meta.Height, meta.Duration = video.Width, video.Height, video.Duration
		meta.VideoCodec, meta.AudioCodec = video.VideoCodec, video.AudioCodec
	}
	if audio != nil {
		meta.AudioCodec, meta.Duration = audio.Codec, audio.Duration
		meta.IsVoiceNote = p.Mode == "voice-note"
	}
	if cfg.Width > 0 {
		meta.Width, meta.Height = cfg.Width, cfg.Height
	}
	meta.SHA256 = sha256Hex(data)
	a.mediaMu.Lock()
	a.prepared[path] = meta
	a.mediaMu.Unlock()
	return meta, nil
}

func (a *App) prepareSticker(ctx context.Context, file *os.File, sourceSize int64, sourcePath string) (any, error) {
	if sourceSize > maxStickerBytes {
		file.Close()
		return nil, fail("invalid_attachment", "Sticker must be a static WebP up to 100 KB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStickerBytes+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || int64(len(data)) != sourceSize {
		return nil, fail("invalid_attachment", "Sticker file changed while preparing")
	}
	cfg, err := validateStaticWebPSticker(data)
	if err != nil {
		return nil, fail("invalid_attachment", err.Error())
	}
	name := filepath.Base(sourcePath)
	if name == "." || name == string(filepath.Separator) {
		return nil, fail("invalid_attachment", "Unsafe sticker filename")
	}
	preparedPath, err := a.stagePrepared(ctx, data, name)
	if err != nil {
		return nil, err
	}
	meta := Attachment{
		Name: name, Size: sourceSize, OriginalSize: sourceSize, PreparedSize: int64(len(data)),
		Mode: "sticker", MIME: "image/webp", Kind: "sticker", URL: fileURL(preparedPath),
		PreparedPath: preparedPath, State: "ready", Width: cfg.Width, Height: cfg.Height,
		SHA256: sha256Hex(data),
	}
	a.mediaMu.Lock()
	a.prepared[preparedPath] = meta
	a.mediaMu.Unlock()
	return meta, nil
}

func validateStaticWebPSticker(data []byte) (image.Config, error) {
	invalid := func(reason string) (image.Config, error) { return image.Config{}, fmt.Errorf("sticker %s", reason) }
	if len(data) == 0 || int64(len(data)) > maxStickerBytes {
		return invalid("must be a static WebP up to 100 KB")
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return invalid("must use the WebP image format")
	}
	if uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return invalid("has an invalid RIFF length")
	}
	hasFrame := false
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return invalid("has a truncated WebP chunk")
		}
		chunkType := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		chunkStart := offset + 8
		if chunkSize > len(data)-chunkStart {
			return invalid("has an invalid WebP chunk size")
		}
		if chunkType == "ANIM" || chunkType == "ANMF" {
			return invalid("must be static; animated stickers are not supported")
		}
		if chunkType == "VP8X" {
			if chunkSize < 10 {
				return invalid("has an invalid extended header")
			}
			if data[chunkStart]&0x02 != 0 {
				return invalid("must be static; animated stickers are not supported")
			}
		}
		if chunkType == "VP8 " || chunkType == "VP8L" {
			hasFrame = true
		}
		paddedSize := chunkSize + (chunkSize & 1)
		if paddedSize > len(data)-chunkStart {
			return invalid("has a truncated WebP padding byte")
		}
		offset = chunkStart + paddedSize
	}
	if !hasFrame {
		return invalid("does not contain a static image frame")
	}
	cfg, err := webp.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return invalid("is malformed")
	}
	if cfg.Width != 512 || cfg.Height != 512 {
		return invalid("must be exactly 512x512 pixels")
	}
	decoded, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return invalid("is malformed")
	}
	transparent := false
	for y := 0; y < 512 && !transparent; y++ {
		for x := 0; x < 512; x++ {
			_, _, _, alpha := decoded.At(x, y).RGBA()
			if alpha < 0xffff {
				transparent = true
				break
			}
		}
	}
	if !transparent {
		return invalid("must include transparent background pixels")
	}
	return cfg, nil
}

func compressStill(data []byte, mime string) ([]byte, string, image.Config, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, mime, image.Config{}, err
	}
	orientation := jpegOrientation(data)
	img = orientImage(img, orientation)
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	required := orientation != 1 || w > 1600 || h > 1600
	if w > 1600 || h > 1600 {
		if w >= h {
			h = h * 1600 / w
			w = 1600
		} else {
			w = w * 1600 / h
			h = 1600
		}
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
		img = dst
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 82}); err != nil {
		return nil, mime, image.Config{}, err
	}
	if out.Len() >= len(data) && !required {
		return data, mime, image.Config{Width: bounds.Dx(), Height: bounds.Dy()}, nil
	}
	return out.Bytes(), "image/jpeg", image.Config{Width: w, Height: h}, nil
}

func jpegOrientation(data []byte) int { // bounded APP1 scan; decoding remains authoritative
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	for i := 2; i+10 < len(data); {
		if data[i] != 0xff {
			i++
			continue
		}
		marker := data[i+1]
		if marker == 0xda || marker == 0xd9 {
			break
		}
		if i+4 > len(data) {
			break
		}
		l := int(data[i+2])<<8 | int(data[i+3])
		if l < 2 || i+2+l > len(data) {
			break
		}
		if marker == 0xe1 && l >= 10 && string(data[i+4:i+10]) == "Exif\x00" {
			t := data[i+10 : i+2+l]
			return parseTIFFOrientation(t)
		}
		i += 2 + l
	}
	return 1
}
func parseTIFFOrientation(b []byte) int {
	if len(b) < 8 {
		return 1
	}
	le := b[0] == 'I'
	u16 := func(x []byte) int {
		if le {
			return int(x[0]) | int(x[1])<<8
		}
		return int(x[1]) | int(x[0])<<8
	}
	u32 := func(x []byte) int {
		if le {
			return int(x[0]) | int(x[1])<<8 | int(x[2])<<16 | int(x[3])<<24
		}
		return int(x[3]) | int(x[2])<<8 | int(x[1])<<16 | int(x[0])<<24
	}
	if u16(b[2:4]) != 42 {
		return 1
	}
	off := u32(b[4:8])
	if off < 0 || off+2 > len(b) {
		return 1
	}
	n := u16(b[off:])
	for i := 0; i < n && off+2+12*(i+1) <= len(b); i++ {
		e := b[off+2+12*i:]
		if u16(e) == 0x112 {
			return u16(e[8:10])
		}
	}
	return 1
}
func orientImage(src image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return src
	}
	sb := src.Bounds()
	w, h := sb.Dx(), sb.Dy()
	rw, rh := w, h
	if orientation >= 5 {
		rw, rh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, rw, rh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := x, y
			switch orientation {
			case 2:
				dx = w - 1 - x
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dy = h - 1 - y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(sb.Min.X+x, sb.Min.Y+y))
		}
	}
	return dst
}
func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func fileURL(p string) string   { return "file://" + p }
func isLocalPreparedPath(root, p string) bool {
	cleanRoot, err := filepath.Abs(filepath.Join(root, "media", "prepared"))
	if err != nil {
		return false
	}
	cleanPath, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (a *App) stagePrepared(ctx context.Context, data []byte, name string) (string, error) {
	return a.stagePreparedWithUniqueness(ctx, data, name, false)
}

func (a *App) stagePreparedUnique(ctx context.Context, data []byte, name string) (string, error) {
	return a.stagePreparedWithUniqueness(ctx, data, name, true)
}

func (a *App) stagePreparedWithUniqueness(ctx context.Context, data []byte, name string, unique bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a.mu.Lock()
	closing := a.closing || a.closed
	a.mu.Unlock()
	if closing {
		return "", fail("app_closing", "App is closing")
	}
	root := filepath.Join(a.dir, "media", "prepared")
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err := a.checkMediaCacheLocked(); err != nil {
		return "", err
	}
	count, used, err := mediaCacheUsage(filepath.Join(a.dir, "media"))
	if err != nil {
		return "", err
	}
	if count >= 500 || used+int64(len(data)) > mediaQuota {
		return "", fail("quota", "Media cache reached its 256 MiB quota")
	}
	// The basename is intentionally opaque: the trusted display name lives in prepared.
	prefix := "prepare-"
	if unique {
		prefix = "album-"
	}
	tmp, err := os.CreateTemp(root, prefix)
	if err != nil {
		return "", err
	}
	tp := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tp)
		}
	}()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return "", err
	}
	if err = os.Chmod(tp, 0600); err != nil {
		return "", err
	}
	if unique {
		removeTemp = false
		return tp, nil
	}
	dst := filepath.Join(root, sha256Hex(data)+".bin")
	if err = os.Rename(tp, dst); err != nil {
		return "", err
	}
	return dst, nil
}
