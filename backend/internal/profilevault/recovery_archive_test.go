package profilevault

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

const recoveryTestPassphrase = "synthetic-test-passphrase-only"

func recoveryTestInput(t *testing.T, migrationStarted bool) RecoveryArchiveInput {
	t.Helper()
	manifest, err := json.Marshal(vaultManifest{
		Format: manifestFormat, Version: manifestVersion,
		VaultID: "0123456789abcdef0123456789abcdef", State: manifestReadyState,
		MigrationStarted: migrationStarted,
	})
	if err != nil {
		t.Fatal(err)
	}
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	key := []byte(base64.RawStdEncoding.EncodeToString(rawKey))
	for i := range rawKey {
		rawKey[i] = 0
	}
	return RecoveryArchiveInput{
		AppID:      "org.example.WhatsFeather",
		ProfileID:  "synthetic-profile-01",
		Manifest:   manifest,
		ProfileKey: key,
		CipherEntries: []RecoveryCipherEntry{
			{Path: "gocryptfs.conf", Mode: fs.FileMode(0o444), Size: int64(len("synthetic encrypted configuration")), Reader: bytes.NewReader([]byte("synthetic encrypted configuration"))},
			{Path: "history", Mode: fs.FileMode(0o755), Directory: true},
			{Path: "history/history.db", Mode: fs.FileMode(0o600), Size: int64(len("synthetic ciphertext record")), Reader: bytes.NewReader([]byte("synthetic ciphertext record"))},
		},
	}
}

type recoveryTestStager struct {
	directories map[string]fs.FileMode
	files       map[string][]byte
	modes       map[string]fs.FileMode
}

func newRecoveryTestStager() *recoveryTestStager {
	return &recoveryTestStager{
		directories: make(map[string]fs.FileMode),
		files:       make(map[string][]byte),
		modes:       make(map[string]fs.FileMode),
	}
}

func (s *recoveryTestStager) MakeCipherDir(path string, mode fs.FileMode) error {
	s.directories[path] = mode
	return nil
}

func (s *recoveryTestStager) CreateCipherFile(path string, mode fs.FileMode) (io.WriteCloser, error) {
	var body bytes.Buffer
	writer := &recoveryTestFileWriter{Buffer: &body, onClose: func() {
		s.files[path] = append([]byte(nil), body.Bytes()...)
		s.modes[path] = mode
	}}
	return writer, nil
}

type recoveryTestFileWriter struct {
	*bytes.Buffer
	onClose func()
}

func (w *recoveryTestFileWriter) Close() error {
	w.onClose()
	return nil
}

func TestRecoveryArchiveRoundTripsIdentityManifestKeyAndCiphertext(t *testing.T) {
	input := recoveryTestInput(t, true)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	stager := newRecoveryTestStager()
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, stager, RecoveryArchiveLimits{},
	)
	if err != nil {
		t.Fatalf("decrypt recovery archive: %v", err)
	}
	if result.AppID != input.AppID || result.ProfileID != input.ProfileID || result.VaultID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("recovered identity mismatch: %#v", result)
	}
	if !result.MigrationStarted {
		t.Fatal("migration_started was not recovered")
	}
	if !bytes.Equal(result.Manifest, input.Manifest) || !bytes.Equal(result.ProfileKey, input.ProfileKey) {
		t.Fatal("recovered manifest or opaque key differs from source")
	}
	if len(result.InventorySHA256) != sha256.Size*2 {
		t.Fatalf("inventory digest length = %d, want %d", len(result.InventorySHA256), sha256.Size*2)
	}
	if _, err := hex.DecodeString(result.InventorySHA256); err != nil {
		t.Fatalf("inventory digest is not hexadecimal: %v", err)
	}
	if got := string(stager.files["gocryptfs.conf"]); got != "synthetic encrypted configuration" {
		t.Fatalf("gocryptfs.conf contents = %q", got)
	}
	if got := stager.modes["gocryptfs.conf"]; got != fs.FileMode(0o444) {
		t.Fatalf("gocryptfs.conf mode = %04o, want 0444", got)
	}
	if got := string(stager.files["history/history.db"]); got != "synthetic ciphertext record" {
		t.Fatalf("history ciphertext = %q", got)
	}
	if got := stager.directories[""]; got != fs.FileMode(0o700) {
		t.Fatalf("cipher root mode = %04o, want private 0700", got)
	}
	if got := stager.directories["history"]; got != fs.FileMode(0o700) {
		t.Fatalf("restored history directory mode = %04o, want normalized private 0700", got)
	}
}

func TestRecoveryArchiveRecoversAuthenticatedSourceProfileWithAppIDOnly(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	stager := newRecoveryTestStager()
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID}, stager, RecoveryArchiveLimits{},
	)
	if err != nil {
		t.Fatalf("recover with only the known application identity: %v", err)
	}
	if result.AppID != input.AppID || result.ProfileID != input.ProfileID {
		t.Fatalf("authenticated source identity = (%q, %q), want (%q, %q)", result.AppID, result.ProfileID, input.AppID, input.ProfileID)
	}
	if !bytes.Equal(result.Manifest, input.Manifest) || !bytes.Equal(result.ProfileKey, input.ProfileKey) {
		t.Fatal("AppID-only recovery did not return the authenticated manifest and opaque key")
	}
	if len(stager.files) == 0 {
		t.Fatal("AppID-only recovery did not stage verified ciphertext")
	}
}

func TestRecoveryArchiveStillRequiresValidExpectedAndAuthenticatedSourceIdentities(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	for _, testCase := range []struct {
		name     string
		expected RecoveryArchiveIdentity
	}{
		{name: "unknown-app-id"},
		{name: "invalid-app-id", expected: RecoveryArchiveIdentity{AppID: "org.example.WhatsFeather\n"}},
		{name: "invalid-supplied-profile-id", expected: RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: "invalid\nprofile"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stager := newRecoveryTestStager()
			result, err := DecryptRecoveryArchive(
				context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase),
				testCase.expected, stager, RecoveryArchiveLimits{},
			)
			if !errors.Is(err, ErrRecoveryArchiveInvalid) {
				t.Fatalf("invalid expected identity returned %v, want ErrRecoveryArchiveInvalid", err)
			}
			if result.AppID != "" || result.ProfileID != "" || result.Manifest != nil || result.ProfileKey != nil || len(stager.files) != 0 || len(stager.directories) != 0 {
				t.Fatal("invalid expected identity returned data or wrote staged output")
			}
		})
	}

	invalidSource := mutateRecoveryTestArchive(t, recoveryTestInput(t, false), recoveryMetadataPath, func(body []byte) []byte {
		var metadata recoveryArchiveMetadata
		if err := json.Unmarshal(body, &metadata); err != nil {
			t.Fatal(err)
		}
		metadata.ProfileID = "invalid\nsource"
		updated, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		return updated
	})
	stager := newRecoveryTestStager()
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(invalidSource), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID}, stager, RecoveryArchiveLimits{},
	)
	if !errors.Is(err, ErrRecoveryArchiveInvalid) {
		t.Fatalf("invalid authenticated source profile ID returned %v, want ErrRecoveryArchiveInvalid", err)
	}
	if len(stager.files) == 0 {
		t.Fatal("test did not reach authenticated metadata validation with the optional expected profile ID")
	}
	if result.AppID != "" || result.ProfileID != "" || result.Manifest != nil || result.ProfileKey != nil {
		t.Fatal("invalid source identity returned authenticated recovery output")
	}
}

type recoveryZeroThenExtraReader struct {
	step int
}

func (r *recoveryZeroThenExtraReader) Read(p []byte) (int, error) {
	switch r.step {
	case 0:
		r.step++
		return copy(p, []byte("x")), nil
	case 1:
		r.step++
		return 0, nil
	default:
		return copy(p, []byte("extra")), nil
	}
}

func TestRecoveryArchiveRequiresSourceEOFAfterDeclaredSize(t *testing.T) {
	input := recoveryTestInput(t, false)
	input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
		Path: "z-extra-probe", Mode: fs.FileMode(0o600), Size: 1, Reader: &recoveryZeroThenExtraReader{},
	})
	var encrypted bytes.Buffer
	err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10)
	if err == nil {
		t.Fatal("encoder treated a transient zero-byte read as source EOF")
	}
	if !errors.Is(err, ErrRecoveryIO) && !errors.Is(err, ErrRecoveryArchiveInvalid) {
		t.Fatalf("unexpected source read error: %v", err)
	}
}

type recoveryReadErrorReader struct {
	err  error
	done bool
}

func (r *recoveryReadErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, []byte("x")), nil
}

type recoveryCancelReader struct {
	cancel context.CancelFunc
	done   bool
}

func (r *recoveryCancelReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, []byte("x"))
	r.cancel()
	return n, nil
}

type recoveryShortWriter struct{}

func (recoveryShortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestRecoveryArchivePropagatesSourceReadError(t *testing.T) {
	input := recoveryTestInput(t, false)
	input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
		Path: "z-reader-error", Mode: fs.FileMode(0o600), Size: 2,
		Reader: &recoveryReadErrorReader{err: errors.New("synthetic source read error")},
	})
	var encrypted bytes.Buffer
	err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10)
	if !errors.Is(err, ErrRecoveryIO) {
		t.Fatalf("source read failure returned %v, want ErrRecoveryIO", err)
	}
	for _, secret := range []string{recoveryTestPassphrase, string(input.ProfileKey)} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("source read error exposed passphrase or profile key")
		}
	}
}

type recoveryCloseErrorWriter struct {
	io.WriteCloser
	err error
}

func (w *recoveryCloseErrorWriter) Close() error {
	return errors.Join(w.WriteCloser.Close(), w.err)
}

type recoveryCloseErrorStager struct {
	*recoveryTestStager
	err error
}

func (s *recoveryCloseErrorStager) CreateCipherFile(path string, mode fs.FileMode) (io.WriteCloser, error) {
	writer, err := s.recoveryTestStager.CreateCipherFile(path, mode)
	if err != nil {
		return nil, err
	}
	return &recoveryCloseErrorWriter{WriteCloser: writer, err: s.err}, nil
}

func TestRecoveryArchivePropagatesStagedFileCloseError(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic archive: %v", err)
	}
	stager := &recoveryCloseErrorStager{recoveryTestStager: newRecoveryTestStager(), err: errors.New("synthetic staged close error")}
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, stager, RecoveryArchiveLimits{},
	)
	if !errors.Is(err, ErrRecoveryIO) {
		t.Fatalf("staged close failure returned %v, want ErrRecoveryIO", err)
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("staged close failure returned metadata or key")
	}
	if _, exists := stager.files["gocryptfs.conf"]; !exists {
		t.Fatal("test stager did not record partial private staging as expected")
	}
}

func TestRecoveryArchiveRejectsShortDestinationWrite(t *testing.T) {
	input := recoveryTestInput(t, false)
	err := encryptRecoveryArchiveWithWorkFactor(context.Background(), recoveryShortWriter{}, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10)
	if !errors.Is(err, ErrRecoveryIO) {
		t.Fatalf("short destination write returned %v, want ErrRecoveryIO", err)
	}
	for _, secret := range []string{recoveryTestPassphrase, string(input.ProfileKey)} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("short-write error exposed passphrase or profile key")
		}
	}
}

func TestRecoveryArchiveCancellationBeforeWorkDoesNotReturnResult(t *testing.T) {
	input := recoveryTestInput(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var encrypted bytes.Buffer
	err := encryptRecoveryArchiveWithWorkFactor(ctx, &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10)
	if !errors.Is(err, ErrRecoveryCanceled) {
		t.Fatalf("pre-canceled operation returned %v, want ErrRecoveryCanceled", err)
	}
	if encrypted.Len() != 0 {
		t.Fatal("pre-canceled operation wrote output")
	}
}

func TestRecoveryArchiveCancellationDuringSourceIOReturnsNoResult(t *testing.T) {
	input := recoveryTestInput(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
		Path: "z-cancel", Mode: fs.FileMode(0o600), Size: 1, Reader: &recoveryCancelReader{cancel: cancel},
	})
	var encrypted bytes.Buffer
	err := encryptRecoveryArchiveWithWorkFactor(ctx, &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10)
	if !errors.Is(err, ErrRecoveryCanceled) {
		t.Fatalf("canceled source read returned %v, want ErrRecoveryCanceled", err)
	}
}

func TestRecoveryArchiveEnforcesLoweredEncryptedAndPlaintextLimits(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic archive: %v", err)
	}
	identity := RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase), identity,
		newRecoveryTestStager(), RecoveryArchiveLimits{MaxEncryptedBytes: 16},
	)
	if !errors.Is(err, ErrRecoveryLimit) {
		t.Fatalf("encrypted byte quota returned %v, want ErrRecoveryLimit", err)
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("encrypted byte quota failure returned metadata or key")
	}
	plain := recoveryDecryptTestPayload(t, encrypted.Bytes(), recoveryTestPassphrase)
	limits := RecoveryArchiveLimits{MaxPlaintextBytes: int64(len(plain) - 1), MaxFileBytes: 4096}
	result, err = DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase), identity,
		newRecoveryTestStager(), limits,
	)
	if !errors.Is(err, ErrRecoveryLimit) {
		t.Fatalf("plaintext byte quota returned %v, want ErrRecoveryLimit", err)
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("plaintext byte quota failure returned metadata or key")
	}
}

func TestRecoveryArchiveBoundsManifestAndKeyBeforeCopy(t *testing.T) {
	input := recoveryTestInput(t, false)
	largeManifest := bytes.Repeat([]byte{'m'}, 8<<20)
	largeKey := bytes.Repeat([]byte{'k'}, 8<<20)
	cases := []struct {
		name       string
		input      RecoveryArchiveInput
		limits     RecoveryArchiveLimits
		want       error
		checkAlloc bool
	}{
		{name: "manifest-over-default-ceiling", input: func() RecoveryArchiveInput { in := input; in.Manifest = largeManifest; return in }(), want: ErrRecoveryLimit, checkAlloc: true},
		{name: "manifest-over-caller-file-limit", input: input, limits: RecoveryArchiveLimits{MaxFileBytes: int64(len(input.Manifest) - 1)}, want: ErrRecoveryLimit},
		{name: "opaque-key-wrong-length", input: func() RecoveryArchiveInput { in := input; in.ProfileKey = largeKey; return in }(), want: ErrRecoveryArchiveInvalid, checkAlloc: true},
		{name: "malformed-manifest", input: func() RecoveryArchiveInput { in := input; in.Manifest = []byte(`{"format":"invalid"}`); return in }(), want: ErrRecoveryArchiveInvalid},
		{name: "malformed-opaque-key-content", input: func() RecoveryArchiveInput { in := input; in.ProfileKey = bytes.Repeat([]byte{'!'}, 43); return in }(), want: ErrRecoveryArchiveInvalid},
		{name: "invalid-limits", input: input, limits: RecoveryArchiveLimits{MaxEncryptedBytes: -1}, want: ErrRecoveryArchiveInvalid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var encrypted bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			recoveryKDFGate <- struct{}{}
			err := encryptRecoveryArchiveWithWorkFactor(ctx, &encrypted, []byte(recoveryTestPassphrase), testCase.input, testCase.limits, 10)
			<-recoveryKDFGate
			runtime.ReadMemStats(&after)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("got %v, want %v", err, testCase.want)
			}
			if encrypted.Len() != 0 {
				t.Fatalf("preflight failure wrote %d encrypted bytes", encrypted.Len())
			}
			if testCase.checkAlloc {
				allocated := after.TotalAlloc - before.TotalAlloc
				if allocated > 1<<20 {
					t.Fatalf("preflight rejection allocated %d bytes for bounded caller input; want <= 1 MiB", allocated)
				}
			}
			for _, secret := range [][]byte{[]byte(recoveryTestPassphrase), testCase.input.ProfileKey} {
				if bytes.Contains([]byte(err.Error()), secret) {
					t.Fatal("input validation error exposed passphrase or opaque key")
				}
			}
		})
	}
}

type recoveryObservedReader struct {
	reads int
	src   io.Reader
}

func (r *recoveryObservedReader) Read(p []byte) (int, error) {
	r.reads++
	if r.src == nil {
		return 0, io.EOF
	}
	return r.src.Read(p)
}

func TestRecoveryArchiveRejectsInvalidDecodeLimitsBeforeReadOrStaging(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	for _, testCase := range []struct {
		name   string
		limits RecoveryArchiveLimits
	}{
		{name: "negative-limit", limits: RecoveryArchiveLimits{MaxEncryptedBytes: -1}},
		{name: "file-limit-above-ustar-maximum", limits: RecoveryArchiveLimits{MaxFileBytes: recoveryDefaultMaxFileBytes + 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			archiveInput := &recoveryObservedReader{src: bytes.NewReader(encrypted.Bytes())}
			stager := newRecoveryTestStager()
			result, err := DecryptRecoveryArchive(
				context.Background(), archiveInput, []byte(recoveryTestPassphrase),
				RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, stager, testCase.limits,
			)
			if !errors.Is(err, ErrRecoveryArchiveInvalid) {
				t.Fatalf("invalid decode limits returned %v, want ErrRecoveryArchiveInvalid", err)
			}
			if archiveInput.reads != 0 || len(stager.files) != 0 || len(stager.directories) != 0 {
				t.Fatal("invalid decode limits consumed input or wrote staging")
			}
			if result.AppID != "" || result.ProfileID != "" || result.Manifest != nil || result.ProfileKey != nil {
				t.Fatal("invalid decode limits returned recovery data")
			}
		})
	}
}

func TestRecoveryArchiveUsesUSTARSizeMaximumAndPathOnlyPAX(t *testing.T) {
	const ustarMaxFileBytes int64 = (1 << 33) - 1 // eleven octal digits in USTAR's 12-byte size field
	if recoveryMaxUSTARFileBytes != ustarMaxFileBytes || recoveryDefaultMaxFileBytes != ustarMaxFileBytes {
		t.Fatalf("codec per-file ceiling = %d/default=%d, want Go USTAR size maximum %d", recoveryMaxUSTARFileBytes, recoveryDefaultMaxFileBytes, ustarMaxFileBytes)
	}
	paths := []struct {
		name     string
		longPath bool
	}{
		{name: recoveryCipherPrefix + "small"},
		{name: recoveryCipherPrefix + strings.Repeat("p", 101), longPath: true},
	}
	for _, testCase := range paths {
		t.Run(testCase.name, func(t *testing.T) {
			var encoded bytes.Buffer
			writer := tar.NewWriter(&encoded)
			if err := writer.WriteHeader(recoveryTarHeader(testCase.name, 0o600, ustarMaxFileBytes, tar.TypeReg)); err != nil {
				t.Fatalf("Go archive/tar rejected exact USTAR size maximum: %v", err)
			}
			header, err := tar.NewReader(bytes.NewReader(encoded.Bytes())).Next()
			if err != nil {
				t.Fatalf("read maximum-size tar header: %v", err)
			}
			if header.Size != ustarMaxFileBytes {
				t.Fatalf("Go archive/tar header size = %d, want %d", header.Size, ustarMaxFileBytes)
			}
			if testCase.longPath {
				if header.Format != tar.FormatPAX || len(header.PAXRecords) != 1 || header.PAXRecords["path"] != testCase.name {
					t.Fatalf("maximum-size long path PAX records = %#v, want exactly one path record", header.PAXRecords)
				}
			} else if header.Format != tar.FormatUSTAR || len(header.PAXRecords) != 0 {
				t.Fatalf("short-path header format = %v records=%#v, want plain USTAR", header.Format, header.PAXRecords)
			}
		})
	}

	shortPath := recoveryCipherPrefix + "small"
	var shortEncoded bytes.Buffer
	if err := tar.NewWriter(&shortEncoded).WriteHeader(recoveryTarHeader(shortPath, 0o600, ustarMaxFileBytes+1, tar.TypeReg)); err == nil {
		t.Fatal("Go archive/tar encoded USTAR size maximum + 1 without PAX")
	}
	longPath := recoveryCipherPrefix + strings.Repeat("p", 101)
	var longEncoded bytes.Buffer
	if err := tar.NewWriter(&longEncoded).WriteHeader(recoveryTarHeader(longPath, 0o600, ustarMaxFileBytes+1, tar.TypeReg)); err != nil {
		t.Fatalf("Go archive/tar did not expose its PAX size-extension behavior: %v", err)
	}
	longHeader, err := tar.NewReader(bytes.NewReader(longEncoded.Bytes())).Next()
	if err != nil {
		t.Fatalf("read Go-generated PAX size extension: %v", err)
	}
	if longHeader.PAXRecords["size"] != "8589934592" || longHeader.PAXRecords["path"] != longPath || len(longHeader.PAXRecords) != 2 {
		t.Fatalf("Go archive/tar PAX records above USTAR maximum = %#v, want path plus size", longHeader.PAXRecords)
	}
}

func TestRecoveryArchiveRejectsAboveUSTARSizeMaximumBeforeKDFOrIO(t *testing.T) {
	const ustarMaxFileBytes int64 = (1 << 33) - 1
	for _, path := range []string{"small", strings.Repeat("p", 101)} {
		t.Run(map[bool]string{true: "long-pax-path", false: "short-ustar-path"}[!ustarPathFits(recoveryCipherPrefix+path)], func(t *testing.T) {
			input := recoveryTestInput(t, false)
			reader := &recoveryObservedReader{}
			input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
				Path: path, Mode: fs.FileMode(0o600), Size: ustarMaxFileBytes + 1, Reader: reader,
			})
			var encrypted bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			recoveryKDFGate <- struct{}{}
			err := encryptRecoveryArchiveWithWorkFactor(ctx, &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10)
			<-recoveryKDFGate
			if !errors.Is(err, ErrRecoveryLimit) {
				t.Fatalf("size above USTAR maximum returned %v, want ErrRecoveryLimit", err)
			}
			if encrypted.Len() != 0 {
				t.Fatalf("preflight rejection wrote %d encrypted bytes", encrypted.Len())
			}
			if reader.reads != 0 {
				t.Fatalf("preflight rejection read oversized source %d times", reader.reads)
			}
		})
	}
}

func TestRecoveryKDFGateSerializesAndAllowsWaitCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withRecoveryKDF(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first KDF did not acquire its gate")
	}
	secondEntered := make(chan struct{})
	secondCtx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- withRecoveryKDF(secondCtx, func() error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second KDF entered while the first held the gate")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrRecoveryCanceled) {
			t.Fatalf("waiting KDF returned %v, want ErrRecoveryCanceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting KDF did not observe cancellation")
	}
	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first KDF failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first KDF did not finish")
	}
}

func TestRecoveryArchiveDoesNotReturnMetadataOrKeyBeforeFinalAgeAuthentication(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	sealed := append([]byte(nil), encrypted.Bytes()...)
	sealed[len(sealed)-1] ^= 0x80
	stager := newRecoveryTestStager()
	archiveInput := &recoveryObservedReader{src: bytes.NewReader(sealed)}
	result, err := DecryptRecoveryArchive(
		context.Background(), archiveInput, []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID}, stager, RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("tampered final age authentication tag was accepted")
	}
	if archiveInput.reads == 0 {
		t.Fatal("optional expected profile ID was rejected before reading the encrypted archive")
	}
	if result.AppID != "" || result.ProfileID != "" || result.VaultID != "" || result.Manifest != nil || result.ProfileKey != nil || result.InventorySHA256 != "" {
		t.Fatal("metadata or opaque key was returned before age final authentication")
	}
	if bytes.Contains([]byte(err.Error()), []byte(recoveryTestPassphrase)) || bytes.Contains([]byte(err.Error()), input.ProfileKey) {
		t.Fatal("recovery error exposed passphrase or profile key")
	}
}

func TestRecoveryArchiveRejectsWrongPassphraseWithoutReturningKey(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte("different-synthetic-passphrase"),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("wrong passphrase was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("wrong passphrase returned recovered key or metadata")
	}
	if bytes.Contains([]byte(err.Error()), input.ProfileKey) || bytes.Contains([]byte(err.Error()), []byte(recoveryTestPassphrase)) {
		t.Fatal("wrong-passphrase error exposed key or passphrase")
	}
}

func TestRecoveryArchiveRejectsRawCiphertextAfterAuthenticatedAgeEOF(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	withTrailingCiphertext := append(append([]byte(nil), encrypted.Bytes()...), []byte("unframed trailing ciphertext")...)
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(withTrailingCiphertext), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("raw ciphertext appended after age EOF was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("raw trailing ciphertext returned recovered key or metadata")
	}
}

func TestRecoveryArchiveRejectsAuthenticatedPlaintextAfterTarEOF(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	plain := recoveryDecryptTestPayload(t, encrypted.Bytes(), recoveryTestPassphrase)
	plain = append(plain, []byte("authenticated bytes after tar EOF")...)
	withTrailingPlaintext := recoveryEncryptTestPayload(t, plain, recoveryTestPassphrase)
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(withTrailingPlaintext), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("authenticated plaintext after tar's two-block EOF was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("appended plaintext returned recovered key or metadata")
	}
}

func recoveryEncryptTestPayload(t *testing.T, plaintext []byte, passphrase string) []byte {
	t.Helper()
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		t.Fatal("create synthetic test recipient")
	}
	recipient.SetWorkFactor(10)
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		t.Fatal("encrypt synthetic test fixture")
	}
	if _, err := writer.Write(plaintext); err != nil {
		t.Fatal("write synthetic test fixture")
	}
	if err := writer.Close(); err != nil {
		t.Fatal("close synthetic test fixture")
	}
	return encrypted.Bytes()
}

func recoveryDecryptTestPayload(t *testing.T, ciphertext []byte, passphrase string) []byte {
	t.Helper()
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		t.Fatal("create synthetic test identity")
	}
	identity.SetMaxWorkFactor(18)
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	if err != nil {
		t.Fatal("decrypt synthetic test fixture")
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal("read synthetic test fixture")
	}
	return plaintext
}

func rewriteRecoveryTestTar(t *testing.T, plaintext []byte, mutate func(*tar.Header, []byte) ([]byte, bool)) []byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(plaintext))
	var rewritten bytes.Buffer
	writer := tar.NewWriter(&rewritten)
	changed := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read synthetic tar member: %v", err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read synthetic tar body: %v", err)
		}
		copyHeader := *header
		body, didChange := mutate(&copyHeader, body)
		changed = changed || didChange
		copyHeader.Size = int64(len(body))
		if err := writer.WriteHeader(&copyHeader); err != nil {
			t.Fatalf("rewrite synthetic tar header: %v", err)
		}
		if len(body) > 0 {
			if _, err := writer.Write(body); err != nil {
				t.Fatalf("rewrite synthetic tar body: %v", err)
			}
		}
	}
	if !changed {
		t.Fatal("synthetic tar mutation did not find its target")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close rewritten synthetic tar: %v", err)
	}
	return rewritten.Bytes()
}

func mutateRecoveryTestArchive(t *testing.T, input RecoveryArchiveInput, targetName string, mutateBody func([]byte) []byte) []byte {
	t.Helper()
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic source archive: %v", err)
	}
	plaintext := recoveryDecryptTestPayload(t, encrypted.Bytes(), recoveryTestPassphrase)
	rewritten := rewriteRecoveryTestTar(t, plaintext, func(header *tar.Header, body []byte) ([]byte, bool) {
		if header.Name != targetName {
			return body, false
		}
		return mutateBody(body), true
	})
	return recoveryEncryptTestPayload(t, rewritten, recoveryTestPassphrase)
}

func TestRecoveryArchiveRejectsManifestMetadataAndExpectedIdentityMismatches(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(t *testing.T, input RecoveryArchiveInput) []byte
		want       error
		expectedID *RecoveryArchiveIdentity
	}{
		{
			name: "unsupported-manifest-version", want: ErrRecoveryUnsupportedVersion,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryManifestPath, func(body []byte) []byte {
					var manifest vaultManifest
					if err := json.Unmarshal(body, &manifest); err != nil {
						t.Fatal(err)
					}
					manifest.Version = manifestVersion + 1
					updated, err := json.Marshal(manifest)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "manifest-vault-id-mismatch", want: ErrRecoveryArchiveInvalid,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryManifestPath, func(body []byte) []byte {
					var manifest vaultManifest
					if err := json.Unmarshal(body, &manifest); err != nil {
						t.Fatal(err)
					}
					manifest.VaultID = strings.Repeat("a", 32)
					updated, err := json.Marshal(manifest)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "migration-started-mismatch", want: ErrRecoveryArchiveInvalid,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryManifestPath, func(body []byte) []byte {
					var manifest vaultManifest
					if err := json.Unmarshal(body, &manifest); err != nil {
						t.Fatal(err)
					}
					manifest.MigrationStarted = true
					updated, err := json.Marshal(manifest)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "unsupported-metadata-version", want: ErrRecoveryUnsupportedVersion,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryMetadataPath, func(body []byte) []byte {
					var metadata recoveryArchiveMetadata
					if err := json.Unmarshal(body, &metadata); err != nil {
						t.Fatal(err)
					}
					metadata.Version = recoveryArchiveVersion + 1
					updated, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "metadata-app-id-mismatch", want: ErrRecoveryArchiveInvalid,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryMetadataPath, func(body []byte) []byte {
					var metadata recoveryArchiveMetadata
					if err := json.Unmarshal(body, &metadata); err != nil {
						t.Fatal(err)
					}
					metadata.AppID = "org.example.OtherFeather"
					updated, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "metadata-profile-id-mismatch", want: ErrRecoveryArchiveInvalid,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryMetadataPath, func(body []byte) []byte {
					var metadata recoveryArchiveMetadata
					if err := json.Unmarshal(body, &metadata); err != nil {
						t.Fatal(err)
					}
					metadata.ProfileID = "synthetic-profile-02"
					updated, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "metadata-vault-id-mismatch", want: ErrRecoveryArchiveInvalid,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryMetadataPath, func(body []byte) []byte {
					var metadata recoveryArchiveMetadata
					if err := json.Unmarshal(body, &metadata); err != nil {
						t.Fatal(err)
					}
					metadata.VaultID = strings.Repeat("b", 32)
					updated, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "metadata-migration-bit-mismatch", want: ErrRecoveryArchiveInvalid,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryMetadataPath, func(body []byte) []byte {
					var metadata recoveryArchiveMetadata
					if err := json.Unmarshal(body, &metadata); err != nil {
						t.Fatal(err)
					}
					metadata.MigrationStarted = true
					updated, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "metadata-count-mismatch", want: ErrRecoveryArchiveInvalid,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryMetadataPath, func(body []byte) []byte {
					var metadata recoveryArchiveMetadata
					if err := json.Unmarshal(body, &metadata); err != nil {
						t.Fatal(err)
					}
					metadata.EntryCount++
					updated, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "metadata-inventory-mismatch", want: ErrRecoveryArchiveInvalid,
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				return mutateRecoveryTestArchive(t, input, recoveryMetadataPath, func(body []byte) []byte {
					var metadata recoveryArchiveMetadata
					if err := json.Unmarshal(body, &metadata); err != nil {
						t.Fatal(err)
					}
					metadata.InventorySHA256 = strings.Repeat("0", 64)
					updated, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					return updated
				})
			},
		},
		{
			name: "expected-app-id-mismatch", want: ErrRecoveryArchiveInvalid,
			expectedID: &RecoveryArchiveIdentity{AppID: "org.example.OtherFeather", ProfileID: "synthetic-profile-01"},
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				var encrypted bytes.Buffer
				if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
					t.Fatal(err)
				}
				return encrypted.Bytes()
			},
		},
		{
			name: "expected-profile-id-mismatch", want: ErrRecoveryArchiveInvalid,
			expectedID: &RecoveryArchiveIdentity{AppID: "org.example.WhatsFeather", ProfileID: "synthetic-profile-02"},
			mutate: func(t *testing.T, input RecoveryArchiveInput) []byte {
				var encrypted bytes.Buffer
				if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
					t.Fatal(err)
				}
				return encrypted.Bytes()
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := recoveryTestInput(t, false)
			sealed := testCase.mutate(t, input)
			expected := RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}
			if testCase.expectedID != nil {
				expected = *testCase.expectedID
			}
			result, err := DecryptRecoveryArchive(context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase), expected, newRecoveryTestStager(), RecoveryArchiveLimits{})
			if !errors.Is(err, testCase.want) {
				t.Fatalf("got %v, want error %v", err, testCase.want)
			}
			if result.AppID != "" || result.ProfileID != "" || result.Manifest != nil || result.ProfileKey != nil {
				t.Fatal("inconsistent archive returned authenticated output")
			}
		})
	}
}

func TestRecoveryArchiveRejectsFirstAgePayloadChunkTamper(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	sealed := append([]byte(nil), encrypted.Bytes()...)
	footerStart := bytes.Index(sealed, []byte("\n--- "))
	if footerStart < 0 {
		t.Fatal("test fixture has no age header footer")
	}
	footerNewline := bytes.IndexByte(sealed[footerStart+1:], '\n')
	if footerNewline < 0 {
		t.Fatal("test fixture has no age header terminator")
	}
	payloadStart := footerStart + 1 + footerNewline + 1
	if payloadStart >= len(sealed) {
		t.Fatal("test fixture has no age payload")
	}
	sealed[payloadStart] ^= 0x01
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("tampered first age payload chunk was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("first-chunk tamper returned recovered key or metadata")
	}
}

func TestRecoveryArchiveRejectsTruncatedAgeFinalChunk(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt recovery archive: %v", err)
	}
	sealed := append([]byte(nil), encrypted.Bytes()...)
	sealed = sealed[:len(sealed)-1]
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("truncated final age chunk was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("truncated archive returned recovered key or metadata")
	}
}

func TestRecoveryArchiveRoundTripsLongSafeCipherPathWithPathOnlyPAX(t *testing.T) {
	input := recoveryTestInput(t, true)
	longName := strings.Repeat("x", 150)
	input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
		Path: longName, Mode: fs.FileMode(0o600), Size: int64(len("cipher")), Reader: bytes.NewReader([]byte("cipher")),
	})
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt long-path archive: %v", err)
	}
	stager := newRecoveryTestStager()
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, stager, RecoveryArchiveLimits{},
	)
	if err != nil {
		t.Fatalf("decrypt long-path archive: %v", err)
	}
	if got := string(stager.files[longName]); got != "cipher" {
		t.Fatalf("long PAX path contents = %q", got)
	}
	if result.InventorySHA256 == "" {
		t.Fatal("long-path inventory was not authenticated")
	}
}

func rewriteFirstRecoveryPAXBody(t *testing.T, plaintext []byte, mutate func([]byte) []byte) []byte {
	t.Helper()
	for offset := 0; offset+512 <= len(plaintext); {
		header := plaintext[offset : offset+512]
		if isZeroTarBlock(header) {
			break
		}
		size, err := parseRecoveryTarSize(header[124:136])
		if err != nil {
			t.Fatalf("parse synthetic tar member size: %v", err)
		}
		paddedSize := int((size + 511) / 512 * 512)
		if header[156] == tar.TypeXHeader {
			bodyStart := offset + 512
			body := plaintext[bodyStart : bodyStart+int(size)]
			updatedBody := mutate(append([]byte(nil), body...))
			patchedHeader := append([]byte(nil), header...)
			copy(patchedHeader[124:136], []byte(fmt.Sprintf("%011o\x00", len(updatedBody))))
			patchRecoveryTarTypeflag(patchedHeader[:512], 0, tar.TypeXHeader)
			newPadding := (512 - len(updatedBody)%512) % 512
			out := make([]byte, 0, len(plaintext)+len(updatedBody)-len(body)+newPadding)
			out = append(out, plaintext[:offset]...)
			out = append(out, patchedHeader...)
			out = append(out, updatedBody...)
			out = append(out, make([]byte, newPadding)...)
			out = append(out, plaintext[bodyStart+paddedSize:]...)
			return out
		}
		offset += 512 + paddedSize
	}
	t.Fatal("synthetic long-path archive has no PAX header")
	return nil
}

func duplicateFirstRecoveryPAXRecord(t *testing.T, plaintext []byte) []byte {
	t.Helper()
	return rewriteFirstRecoveryPAXBody(t, plaintext, func(body []byte) []byte {
		return append(body, body...)
	})
}

func recoveryTestPAXRecord(key, value string) []byte {
	body := key + "=" + value + "\n"
	length := len(body) + 2
	for {
		record := []byte(fmt.Sprintf("%d %s", length, body))
		if len(record) == length {
			return record
		}
		length = len(record)
	}
}

func TestRecoveryArchiveRejectsDuplicateHiddenPAXPathRecords(t *testing.T) {
	input := recoveryTestInput(t, false)
	longName := strings.Repeat("x", 150)
	input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
		Path: longName, Mode: fs.FileMode(0o600), Size: 1, Reader: bytes.NewReader([]byte("x")),
	})
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic long-path archive: %v", err)
	}
	plain := recoveryDecryptTestPayload(t, encrypted.Bytes(), recoveryTestPassphrase)
	malformed := duplicateFirstRecoveryPAXRecord(t, plain)
	sealed := recoveryEncryptTestPayload(t, malformed, recoveryTestPassphrase)
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("duplicate hidden PAX path records were accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("duplicate PAX records returned metadata or key")
	}
}

func TestRecoveryArchiveRejectsNonPathPAXRecord(t *testing.T) {
	input := recoveryTestInput(t, false)
	input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
		Path: strings.Repeat("y", 150), Mode: fs.FileMode(0o600), Size: 1, Reader: bytes.NewReader([]byte("y")),
	})
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic long-path archive: %v", err)
	}
	plain := recoveryDecryptTestPayload(t, encrypted.Bytes(), recoveryTestPassphrase)
	malformed := rewriteFirstRecoveryPAXBody(t, plain, func(body []byte) []byte {
		return append(body, recoveryTestPAXRecord("comment", "ignored")...)
	})
	sealed := recoveryEncryptTestPayload(t, malformed, recoveryTestPassphrase)
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("unsupported PAX key was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("unsupported PAX key returned metadata or key")
	}
}

func TestRecoveryArchiveRejectsNonzeroTarPadding(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic archive: %v", err)
	}
	plain := recoveryDecryptTestPayload(t, encrypted.Bytes(), recoveryTestPassphrase)
	manifestSize, err := parseRecoveryTarSize(plain[124:136])
	if err != nil {
		t.Fatal(err)
	}
	paddingStart := 512 + int(manifestSize)
	if paddingStart >= 512+((int(manifestSize)+511)/512)*512 {
		t.Fatal("synthetic manifest has no tar padding")
	}
	plain[paddingStart] = 0x7f
	sealed := recoveryEncryptTestPayload(t, plain, recoveryTestPassphrase)
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("nonzero tar padding was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("nonzero padding returned metadata or key")
	}
}

func TestRecoveryArchiveRejectsFullArchivePathBeyondLoweredLimit(t *testing.T) {
	input := recoveryTestInput(t, false)
	input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
		Path: strings.Repeat("z", 110), Mode: fs.FileMode(0o600), Size: 1, Reader: bytes.NewReader([]byte("x")),
	})
	limits := RecoveryArchiveLimits{MaxPathBytes: 120}
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, limits, 10); err == nil {
		t.Fatal("encoder accepted a relative path that exceeds the limit after the archive prefix")
	}
}

func TestRecoveryArchiveRejectsMetadataAboveLoweredPerFileLimit(t *testing.T) {
	input := recoveryTestInput(t, false)
	limits := RecoveryArchiveLimits{MaxFileBytes: int64(len(input.Manifest) + 50)}
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, limits, 10); err == nil {
		t.Fatal("encoder accepted metadata larger than the per-file limit")
	}
	if encrypted.Len() != 0 {
		t.Fatal("per-file limit was checked only after writing encrypted output")
	}
}

func TestRecoveryArchiveRejectsHighScryptWorkFactorBeforeKDFGate(t *testing.T) {
	recoveryKDFGate <- struct{}{}
	defer func() { <-recoveryKDFGate }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	maliciousHeader := []byte("age-encryption.org/v1\n-> scrypt AAAAAAAAAAAAAAAAAAAAAA 19\nAAAA\n--- AA\n")
	result, err := DecryptRecoveryArchive(
		ctx, bytes.NewReader(maliciousHeader), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: "org.example.WhatsFeather", ProfileID: "synthetic-profile-01"},
		newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil || errors.Is(err, ErrRecoveryCanceled) {
		t.Fatal("high scrypt work factor waited on or entered the KDF gate")
	}
	if !errors.Is(err, ErrRecoveryArchiveInvalid) {
		t.Fatal("high scrypt work factor did not fail as invalid input")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("high-work-factor header returned metadata or a key")
	}
}

type recoveryTestTarMember struct {
	header  tar.Header
	body    []byte
	rawName []byte
}

func recoveryTestTarWithAttack(t *testing.T, manifest []byte, attack recoveryTestTarMember) []byte {
	t.Helper()
	var plaintext bytes.Buffer
	writer := tar.NewWriter(&plaintext)
	if err := writer.WriteHeader(recoveryTarHeader(recoveryManifestPath, 0o600, int64(len(manifest)), tar.TypeReg)); err != nil {
		t.Fatalf("write synthetic tar manifest header: %v", err)
	}
	if _, err := writer.Write(manifest); err != nil {
		t.Fatalf("write synthetic tar manifest: %v", err)
	}
	if err := writer.WriteHeader(recoveryTarHeader(recoveryCipherRootPath, 0o700, 0, tar.TypeDir)); err != nil {
		t.Fatalf("write synthetic tar cipher root: %v", err)
	}
	attackType := attack.header.Typeflag
	patchRawType := attackType == tar.TypeXHeader || attackType == tar.TypeXGlobalHeader || attackType == tar.TypeGNULongName || attackType == tar.TypeGNULongLink
	if patchRawType {
		attack.header.Typeflag = tar.TypeReg
		attack.header.Format = tar.FormatUSTAR
	}
	attack.header.ModTime = time.Unix(0, 0).UTC()
	attackOffset := 512 + ((len(manifest)+511)/512)*512 + 512
	if err := writer.WriteHeader(&attack.header); err != nil {
		t.Fatalf("write synthetic attack header: %v", err)
	}
	if len(attack.body) > 0 {
		if _, err := writer.Write(attack.body); err != nil {
			t.Fatalf("write synthetic attack body: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close synthetic attack archive: %v", err)
	}
	if len(attack.rawName) > 0 {
		copy(plaintext.Bytes()[attackOffset:attackOffset+100], attack.rawName)
		patchRecoveryTarTypeflag(plaintext.Bytes(), attackOffset, attackType)
	}
	if patchRawType {
		patchRecoveryTarTypeflag(plaintext.Bytes(), attackOffset, attackType)
	}
	return plaintext.Bytes()
}

func patchRecoveryTarTypeflag(archive []byte, offset int, typeflag byte) {
	header := archive[offset : offset+512]
	header[156] = typeflag
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	var checksum int64
	for _, b := range header {
		checksum += int64(b)
	}
	copy(header[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
}

func patchFirstRecoveryTarName(t *testing.T, plaintext []byte, target, rawName string) []byte {
	t.Helper()
	for offset := 0; offset+512 <= len(plaintext); {
		header := plaintext[offset : offset+512]
		if isZeroTarBlock(header) {
			break
		}
		size, err := parseRecoveryTarSize(header[124:136])
		if err != nil {
			t.Fatalf("parse synthetic tar member size: %v", err)
		}
		name := string(bytes.TrimRight(header[:100], "\x00"))
		prefix := string(bytes.TrimRight(header[345:500], "\x00"))
		if prefix != "" {
			name = prefix + "/" + name
		}
		if name == target {
			if len(rawName) > 100 {
				t.Fatal("synthetic raw tar name exceeds field")
			}
			clear(header[:100])
			copy(header[:100], rawName)
			patchRecoveryTarTypeflag(plaintext, offset, header[156])
			return plaintext
		}
		offset += 512 + int((size+511)/512*512)
	}
	t.Fatalf("synthetic tar member not found: %s", target)
	return nil
}

func TestRecoveryArchiveRejectsNoncanonicalCipherPaths(t *testing.T) {
	input := recoveryTestInput(t, false)
	cases := []struct {
		name string
		path string
	}{
		{name: "absolute", path: "/vault/cipher/absolute"},
		{name: "parent", path: "vault/cipher/../escape"},
		{name: "dot-component", path: "vault/cipher/./escape"},
		{name: "empty-component", path: "vault/cipher//escape"},
		{name: "windows-separator", path: `vault\\cipher\\escape`},
		{name: "drive-colon", path: "vault/cipher/C:/escape"},
		{name: "trailing-dot", path: "vault/cipher/name."},
		{name: "trailing-space", path: "vault/cipher/name "},
		{name: "windows-device", path: "vault/cipher/CON.txt"},
		{name: "non-ascii", path: "vault/cipher/café"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plain := recoveryTestTarWithAttack(t, input.Manifest, recoveryTestTarMember{
				header: tar.Header{Name: testCase.path, Mode: 0o600, Typeflag: tar.TypeReg},
			})
			sealed := recoveryEncryptTestPayload(t, plain, recoveryTestPassphrase)
			result, err := DecryptRecoveryArchive(
				context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
				RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
			)
			if err == nil {
				t.Fatal("noncanonical archive path was accepted")
			}
			if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
				t.Fatal("noncanonical path returned metadata or key")
			}
		})
	}
}

func TestRecoveryArchiveRejectsNULAliasInRawTarName(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic archive: %v", err)
	}
	plain := recoveryDecryptTestPayload(t, encrypted.Bytes(), recoveryTestPassphrase)
	plain = patchFirstRecoveryTarName(t, plain, recoveryCipherPrefix+"gocryptfs.conf", recoveryCipherPrefix+"gocryptfs.conf\x00alias")
	sealed := recoveryEncryptTestPayload(t, plain, recoveryTestPassphrase)
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("noncanonical NUL-suffixed raw tar name was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("NUL-suffixed raw name returned metadata or key")
	}
}

func TestRecoveryArchiveRejectsDuplicateCipherPath(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic archive: %v", err)
	}
	plain := recoveryDecryptTestPayload(t, encrypted.Bytes(), recoveryTestPassphrase)
	malformed := rewriteRecoveryTestTar(t, plain, func(header *tar.Header, body []byte) ([]byte, bool) {
		if header.Name == recoveryCipherPrefix+"history/history.db" {
			header.Name = recoveryCipherPrefix + "gocryptfs.conf"
			return body, true
		}
		return body, false
	})
	sealed := recoveryEncryptTestPayload(t, malformed, recoveryTestPassphrase)
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
	)
	if err == nil {
		t.Fatal("duplicate cipher path was accepted")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("duplicate path returned metadata or key")
	}
}

func TestRecoveryArchiveRejectsLinksDevicesSparseAndUnknownTarMembers(t *testing.T) {
	cases := []struct {
		name     string
		typeflag byte
		format   tar.Format
		body     []byte
	}{
		{name: "symlink", typeflag: tar.TypeSymlink},
		{name: "hardlink", typeflag: tar.TypeLink},
		{name: "character-device", typeflag: tar.TypeChar},
		{name: "block-device", typeflag: tar.TypeBlock},
		{name: "fifo", typeflag: tar.TypeFifo},
		{name: "gnu-sparse", typeflag: tar.TypeGNUSparse, format: tar.FormatGNU},
		{name: "gnu-long-name", typeflag: tar.TypeGNULongName, format: tar.FormatGNU, body: []byte("vault/cipher/alias\x00")},
		{name: "global-pax", typeflag: tar.TypeXGlobalHeader, format: tar.FormatPAX, body: []byte("14 path=ignored\n")},
		{name: "unknown", typeflag: 'q'},
	}
	input := recoveryTestInput(t, false)
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			member := recoveryTestTarMember{
				header: tar.Header{
					Name: "vault/cipher/unsafe", Mode: 0o600, Size: int64(len(testCase.body)),
					Typeflag: testCase.typeflag, Linkname: "target", Format: testCase.format,
				},
				body: testCase.body,
			}
			plain := recoveryTestTarWithAttack(t, input.Manifest, member)
			sealed := recoveryEncryptTestPayload(t, plain, recoveryTestPassphrase)
			result, err := DecryptRecoveryArchive(
				context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
				RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), RecoveryArchiveLimits{},
			)
			if err == nil {
				t.Fatal("unsupported tar member was accepted")
			}
			if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
				t.Fatal("unsupported tar member returned metadata or key")
			}
		})
	}
}

func TestRecoveryArchiveRejectsHiddenPAXHeaderOverLoweredQuota(t *testing.T) {
	input := recoveryTestInput(t, false)
	body := bytes.Repeat([]byte{'x'}, 1025)
	plain := recoveryTestTarWithAttack(t, input.Manifest, recoveryTestTarMember{
		header: tar.Header{
			Name: "PaxHeaders.0/hidden", Mode: 0o600, Size: int64(len(body)),
			Typeflag: tar.TypeXHeader, Format: tar.FormatPAX,
		},
		body: body,
	})
	sealed := recoveryEncryptTestPayload(t, plain, recoveryTestPassphrase)
	limits := RecoveryArchiveLimits{MaxPAXHeaderBytes: 1024}
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(sealed), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), limits,
	)
	if !errors.Is(err, ErrRecoveryLimit) {
		t.Fatal("oversized hidden PAX header did not fail at the bounded raw-header guard")
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("oversized PAX header returned metadata or key")
	}
}

func TestRecoveryArchiveCountsHiddenPAXHeaderAgainstMemberQuota(t *testing.T) {
	input := recoveryTestInput(t, false)
	input.CipherEntries = append(input.CipherEntries, RecoveryCipherEntry{
		Path: strings.Repeat("q", 150), Mode: fs.FileMode(0o600), Size: 1, Reader: bytes.NewReader([]byte("q")),
	})
	limits := RecoveryArchiveLimits{MaxMembers: 8}
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, limits, 10); !errors.Is(err, ErrRecoveryLimit) {
		t.Fatalf("encoder did not count the physical PAX header against MaxMembers: %v", err)
	}
	if encrypted.Len() != 0 {
		t.Fatal("member quota was checked only after writing encrypted output")
	}
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt full synthetic archive: %v", err)
	}
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), limits,
	)
	if !errors.Is(err, ErrRecoveryLimit) {
		t.Fatalf("decoder did not count the physical PAX header against MaxMembers: %v", err)
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("member quota failure returned metadata or key")
	}
}

func TestRecoveryArchiveReportsMetadataQuotaAsLimit(t *testing.T) {
	input := recoveryTestInput(t, false)
	var encrypted bytes.Buffer
	if err := encryptRecoveryArchiveWithWorkFactor(context.Background(), &encrypted, []byte(recoveryTestPassphrase), input, RecoveryArchiveLimits{}, 10); err != nil {
		t.Fatalf("encrypt synthetic archive: %v", err)
	}
	limits := RecoveryArchiveLimits{MaxMetadataBytes: 128}
	result, err := DecryptRecoveryArchive(
		context.Background(), bytes.NewReader(encrypted.Bytes()), []byte(recoveryTestPassphrase),
		RecoveryArchiveIdentity{AppID: input.AppID, ProfileID: input.ProfileID}, newRecoveryTestStager(), limits,
	)
	if !errors.Is(err, ErrRecoveryLimit) {
		t.Fatalf("metadata size limit returned %v, want ErrRecoveryLimit", err)
	}
	if result.ProfileKey != nil || result.Manifest != nil || result.AppID != "" {
		t.Fatal("metadata quota failure returned metadata or key")
	}
}
