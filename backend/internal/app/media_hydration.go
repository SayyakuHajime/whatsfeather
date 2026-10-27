package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// A single RPC may validate at most one maximum-sized attachment. Repeated
// references to an unchanged file are checked once within that RPC.
const mediaHydrationVerificationBudget = maxAlbumPrepared

type mediaHydrationContextKey struct{}

type mediaHydrationReadFunc func(context.Context, *os.File, []byte) (int, error)

type mediaHydrationVerifiedFile struct {
	info   os.FileInfo
	digest string
}

type mediaHydrationScope struct {
	used     int64
	verified []mediaHydrationVerifiedFile
	read     mediaHydrationReadFunc
}

func ensureMediaHydrationScope(ctx context.Context) context.Context {
	if scope, _ := ctx.Value(mediaHydrationContextKey{}).(*mediaHydrationScope); scope != nil {
		return ctx
	}
	return context.WithValue(ctx, mediaHydrationContextKey{}, &mediaHydrationScope{})
}

func mediaHydrationScopeFrom(ctx context.Context) *mediaHydrationScope {
	scope, _ := ctx.Value(mediaHydrationContextKey{}).(*mediaHydrationScope)
	if scope == nil {
		scope = &mediaHydrationScope{}
	}
	return scope
}

func sameMediaSnapshot(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (scope *mediaHydrationScope) hasVerified(info os.FileInfo, digest string) bool {
	for _, previous := range scope.verified {
		if previous.digest == digest && sameMediaSnapshot(previous.info, info) {
			return true
		}
	}
	return false
}

func (scope *mediaHydrationScope) reserve(size int64) error {
	if size < 0 || size > mediaHydrationVerificationBudget-scope.used {
		return fail("media_verification_budget", "Local media verification exceeds the 50 MiB per-request limit")
	}
	scope.used += size
	return nil
}

func hashMediaFile(ctx context.Context, file *os.File, size int64, read mediaHydrationReadFunc) (string, int64, error) {
	if read == nil {
		read = func(_ context.Context, file *os.File, p []byte) (int, error) { return file.Read(p) }
	}
	type result struct {
		digest string
		total  int64
		err    error
	}
	complete := make(chan result, 1)
	go func() {
		digest, total, err := hashMediaFileLoop(ctx, file, size, read)
		complete <- result{digest: digest, total: total, err: err}
	}()
	select {
	case value := <-complete:
		return value.digest, value.total, value.err
	case <-ctx.Done():
		// Closing an *os.File interrupts a blocked regular-file read. The
		// goroutine's result channel is buffered so it can exit after observing
		// the close even if this request has already returned.
		_ = file.Close()
		return "", 0, ctx.Err()
	}
}

func hashMediaFileLoop(ctx context.Context, file *os.File, size int64, read mediaHydrationReadFunc) (string, int64, error) {
	hash := sha256.New()
	buf := make([]byte, 32<<10)
	var total int64
	for total < size {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		want := int64(len(buf))
		if left := size - total; left < want {
			want = left
		}
		n, readErr := read(ctx, file, buf[:int(want)])
		if n < 0 || n > int(want) {
			return "", total, io.ErrUnexpectedEOF
		}
		if n > 0 {
			_, _ = hash.Write(buf[:n])
			total += int64(n)
		}
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		if readErr != nil {
			if readErr != io.EOF {
				return "", total, readErr
			}
			break
		}
		if n == 0 {
			return "", total, io.ErrNoProgress
		}
	}
	if err := ctx.Err(); err != nil {
		return "", total, err
	}
	return hex.EncodeToString(hash.Sum(nil)), total, nil
}

func resolveOwnedMediaURL(ctx context.Context, profileRoot string, meta Attachment) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if meta.State != "ready" || !isLocalFileURL(meta.URL) {
		return "", false, nil
	}
	if meta.SHA256 == "" {
		hash, ok := mediaIdentityHash(meta.URL, meta)
		if !ok {
			return "", false, nil
		}
		meta.SHA256 = hash
	}
	name, ok := mediaCacheFilename(meta)
	if !ok {
		return "", false, nil
	}
	parsed, err := url.Parse(meta.URL)
	if err != nil {
		return "", false, nil
	}
	if parsed.Path != "/media/v1/"+name {
		// Legacy absolute URLs are usable only while they still name this exact
		// profile cache entry. A relocated mount alone cannot prove ownership.
		currentRoot, rootErr := filepath.Abs(profileRoot)
		if rootErr != nil || parsed.Path != filepath.Join(currentRoot, "media", name) {
			return "", false, nil
		}
	}
	mediaRoot := filepath.Join(profileRoot, "media")
	rootInfo, err := os.Lstat(mediaRoot)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", false, nil
	}
	path := filepath.Join(mediaRoot, name)
	pathInfo, err := os.Lstat(path)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		pathInfo.Size() <= 0 || pathInfo.Size() > attachmentLimit || (meta.Size > 0 && pathInfo.Size() != meta.Size) {
		return "", false, nil
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false, nil
	}
	stat, statErr := file.Stat()
	if statErr != nil || !stat.Mode().IsRegular() || !sameMediaSnapshot(pathInfo, stat) {
		_ = file.Close()
		return "", false, nil
	}

	scope := mediaHydrationScopeFrom(ctx)
	if scope.hasVerified(stat, meta.SHA256) {
		closeErr := file.Close()
		if err = ctx.Err(); err != nil {
			return "", false, err
		}
		if closeErr != nil {
			return "", false, nil
		}
		return (&url.URL{Scheme: "file", Path: path}).String(), true, nil
	}
	if err = scope.reserve(stat.Size()); err != nil {
		_ = file.Close()
		return "", false, err
	}
	read := scope.read
	if read == nil {
		read = func(_ context.Context, file *os.File, p []byte) (int, error) { return file.Read(p) }
	}
	digest, n, copyErr := hashMediaFile(ctx, file, stat.Size(), read)
	stable := false
	if copyErr == nil && n == stat.Size() {
		after, afterErr := file.Stat()
		pathAfter, pathErr := os.Lstat(path)
		stable = afterErr == nil && pathErr == nil && pathAfter.Mode()&os.ModeSymlink == 0 &&
			sameMediaSnapshot(stat, after) && sameMediaSnapshot(stat, pathAfter)
	}
	closeErr := file.Close()
	if err = ctx.Err(); err != nil {
		return "", false, err
	}
	if copyErr != nil || !stable || closeErr != nil || n != stat.Size() || digest != meta.SHA256 {
		return "", false, nil
	}
	scope.verified = append(scope.verified, mediaHydrationVerifiedFile{info: stat, digest: digest})
	return (&url.URL{Scheme: "file", Path: path}).String(), true, nil
}
