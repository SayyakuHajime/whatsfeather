package profilevault

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
)

const (
	recoveryArchiveFormat        = "whatsfeather-profile-recovery"
	recoveryArchiveVersion       = 1
	recoveryProfileKeyPath       = "recovery/profile-key"
	recoveryMetadataPath         = "recovery/metadata.json"
	recoveryManifestPath         = "vault/.profilevault.json"
	recoveryCipherRootPath       = "vault/cipher"
	recoveryCipherPrefix         = recoveryCipherRootPath + "/"
	recoveryMaxPassphraseBytes   = 1024
	recoveryMaxAgeHeaderBytes    = 2 << 20
	recoveryMaxAgeStanzas        = 1024
	recoveryMaxAgeStanzaArgs     = 128
	recoveryMaxPAXPathRecordSize = 4096
	recoveryCopyBufferBytes      = 32 << 10
)

// recoveryKDFWorkFactor fixes the age scrypt work factor before KDF execution.
// At logN=18, r=8, x/crypto allocates a 256 MiB working array; an earlier
// synthetic probe observed 626748 KiB peak RSS. This is not a sub-256-MiB
// process bound, and the context-aware gate permits only one active KDF.
const recoveryKDFWorkFactor = 18

const (
	recoveryDefaultMaxEncryptedBytes int64 = 64 << 30
	recoveryDefaultMaxPlaintextBytes int64 = 64 << 30
	recoveryMaxUSTARFileBytes        int64 = (1 << 33) - 1
	recoveryDefaultMaxFileBytes      int64 = recoveryMaxUSTARFileBytes
	recoveryDefaultMaxMembers              = 100_000
	recoveryDefaultMaxPathBytes            = 1024
	recoveryDefaultMaxPathDepth            = 32
	recoveryDefaultMaxMetadataBytes  int64 = 64 << 10
	recoveryDefaultMaxPAXHeaderBytes int64 = recoveryMaxPAXPathRecordSize
)

var (
	// ErrRecoveryArchiveInvalid identifies an invalid, inconsistent, or unsupported archive.
	ErrRecoveryArchiveInvalid = errors.New("profilevault: recovery archive is invalid")
	// ErrRecoveryAuthentication identifies an age authentication or passphrase failure.
	ErrRecoveryAuthentication = errors.New("profilevault: recovery authentication failed")
	// ErrRecoveryLimit identifies a configured encrypted, plaintext, member, or path limit.
	ErrRecoveryLimit = errors.New("profilevault: recovery archive exceeds a safety limit")
	// ErrRecoveryIO identifies a source, destination, staging, or age stream I/O failure.
	ErrRecoveryIO = errors.New("profilevault: recovery archive I/O failed")
	// ErrRecoveryCanceled identifies a canceled recovery operation.
	ErrRecoveryCanceled = errors.New("profilevault: recovery operation canceled")
	// ErrRecoveryUnsupportedVersion identifies a recognized archive with an unsupported version.
	ErrRecoveryUnsupportedVersion = errors.New("profilevault: unsupported recovery archive version")

	recoveryKDFGate = make(chan struct{}, 1)
)

// RecoveryArchiveLimits can lower the codec's documented defaults, never raise them.
// Zero fields use these defaults: 64 GiB encrypted input, 64 GiB decrypted tar,
// 8,589,934,591 bytes (8 GiB minus one byte) per file, the USTAR octal size-field
// maximum; PAX is path-only and file-size extensions are not supported. The
// remaining defaults are 100,000 physical tar headers (including hidden PAX
// headers), 1,024-byte member paths, depth 32, 64 KiB metadata, and 4 KiB for one
// PAX path record. These are broad ceilings intended to leave headroom for
// substantial history/media-cache growth; real-profile sizes were not inspected
// to validate that target. Limits bound streams and parsing, not scrypt memory:
// logN=18 has a 256 MiB working-array allocation and a prior synthetic probe
// peaked at 626748 KiB RSS. The process gate allows only one active KDF.
type RecoveryArchiveLimits struct {
	MaxEncryptedBytes int64
	MaxPlaintextBytes int64
	MaxFileBytes      int64
	MaxMembers        int
	MaxPathBytes      int
	MaxPathDepth      int
	MaxMetadataBytes  int64
	MaxPAXHeaderBytes int64
}

// RecoveryArchiveInput describes a stable source snapshot. The caller owns and
// validates the source readers; each regular-file reader must yield exactly Size
// bytes and then EOF. Manifest and profile-key bytes are copied only in memory.
type RecoveryArchiveInput struct {
	AppID         string
	ProfileID     string
	Manifest      []byte
	ProfileKey    []byte
	CipherEntries []RecoveryCipherEntry
}

// RecoveryCipherEntry is a regular file or directory under the cipher root.
// Path is slash-separated and relative to cipher/. Mode contains permission bits
// only; file permission bits are preserved, including 0444 gocryptfs.conf files.
type RecoveryCipherEntry struct {
	Path      string
	Mode      fs.FileMode
	Size      int64
	Directory bool
	Reader    io.Reader
}

// RecoveryArchiveIdentity requires the expected source AppID and optionally the
// expected source ProfileID. An empty ProfileID means the caller does not know
// the old source ID; it does not relax validation of authenticated metadata.
// Destination identity is selected independently by the recovery owner.
type RecoveryArchiveIdentity struct {
	AppID     string
	ProfileID string
}

// RecoveryCipherStager writes only ciphertext files/directories to caller-owned
// private staging. The caller must keep all ancestor directories mode 0700, keep
// staging unpublished, and remove the entire staging tree on every codec error.
// The codec never commits, publishes, cleans up, writes a manifest, or writes a key.
type RecoveryCipherStager interface {
	MakeCipherDir(path string, mode fs.FileMode) error
	CreateCipherFile(path string, mode fs.FileMode) (io.WriteCloser, error)
}

// RecoveryArchiveResult is returned only after the full age stream, canonical tar
// terminator, schema, identity, manifest, and inventory have been verified.
// ProfileKey is opaque keyring data and is returned in memory only; callers should
// wipe it when no longer needed. Go strings and dependency-internal copies cannot
// be guaranteed erased by this codec.
type RecoveryArchiveResult struct {
	AppID            string
	ProfileID        string
	VaultID          string
	MigrationStarted bool
	EntryCount       int
	TotalBytes       int64
	InventorySHA256  string
	Manifest         []byte
	ProfileKey       []byte
}

type recoveryArchiveMetadata struct {
	Format           string `json:"format"`
	Version          int    `json:"version"`
	AppID            string `json:"app_id"`
	ProfileID        string `json:"profile_id"`
	VaultID          string `json:"vault_id"`
	MigrationStarted bool   `json:"migration_started"`
	EntryCount       int    `json:"entry_count"`
	TotalBytes       int64  `json:"total_bytes"`
	InventorySHA256  string `json:"inventory_sha256"`
}

type recoveryInventoryRecord struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   int64  `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type recoveryArchiveError struct {
	category  error
	operation string
	cause     error
}

func (e *recoveryArchiveError) Error() string {
	return "profilevault: recovery " + e.operation + " failed"
}

func (e *recoveryArchiveError) Unwrap() error {
	if e.cause == nil {
		return e.category
	}
	return errors.Join(e.category, e.cause)
}

func recoveryFailure(category error, operation string, cause error) error {
	return &recoveryArchiveError{category: category, operation: operation, cause: cause}
}

// EncryptRecoveryArchive streams a canonical archive through age v1 with a
// passphrase recipient. Production writes use scrypt logN=18. The destination
// may contain partial ciphertext on error; its owner must write to a private
// temporary artifact and discard it unless this function returns nil.
func EncryptRecoveryArchive(ctx context.Context, dst io.Writer, passphrase []byte, input RecoveryArchiveInput, limits RecoveryArchiveLimits) error {
	return encryptRecoveryArchiveWithWorkFactor(ctx, dst, passphrase, input, limits, recoveryKDFWorkFactor)
}

func encryptRecoveryArchiveWithWorkFactor(ctx context.Context, dst io.Writer, passphrase []byte, input RecoveryArchiveInput, requested RecoveryArchiveLimits, workFactor int) error {
	if ctx == nil || dst == nil || workFactor < 1 || workFactor > recoveryKDFWorkFactor || !validRecoveryPassphrase(passphrase) {
		return ErrRecoveryArchiveInvalid
	}
	if err := ctx.Err(); err != nil {
		return recoveryFailure(ErrRecoveryCanceled, "encrypt", err)
	}
	limits, err := recoveryLimits(requested)
	if err != nil {
		return err
	}
	manifestLength := int64(len(input.Manifest))
	if manifestLength > maxManifestBytes || manifestLength > limits.MaxFileBytes {
		return ErrRecoveryLimit
	}
	if len(input.ProfileKey) != 43 || validateIdentity(input.AppID, input.ProfileID) != nil || !validMasterKey(input.ProfileKey) {
		return ErrRecoveryArchiveInvalid
	}
	manifestBytes := append([]byte(nil), input.Manifest...)
	keyBytes := append([]byte(nil), input.ProfileKey...)
	defer wipe(keyBytes)
	manifest, err := parseRecoveryVaultManifest(manifestBytes)
	if err != nil {
		return err
	}
	entryCount, totalBytes, estimatedPlaintext, err := validateRecoveryInput(input, manifestBytes, keyBytes, limits)
	if err != nil {
		return err
	}
	if estimatedPlaintext > limits.MaxPlaintextBytes || estimatedPlaintext > limits.MaxEncryptedBytes {
		return ErrRecoveryLimit
	}

	limitedDst := &recoveryLimitedWriter{ctx: ctx, dst: dst, max: limits.MaxEncryptedBytes}
	var encrypted io.WriteCloser
	err = withRecoveryKDF(ctx, func() error {
		recipient, err := age.NewScryptRecipient(string(passphrase))
		if err != nil {
			return err
		}
		recipient.SetWorkFactor(workFactor)
		encrypted, err = age.Encrypt(limitedDst, recipient)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return recoveryFailure(ErrRecoveryCanceled, "encrypt", ctx.Err())
		}
		return recoveryFailure(ErrRecoveryAuthentication, "encrypt", err)
	}

	archive := tar.NewWriter(&recoveryCheckedWriter{ctx: ctx, dst: encrypted})
	inventory := sha256.New()
	archiveErr := writeRecoveryMember(archive, recoveryManifestPath, fs.FileMode(0o600), manifestBytes, inventory)
	if archiveErr == nil {
		archiveErr = writeRecoveryDirectory(archive, recoveryCipherRootPath, fs.FileMode(0o700), inventory)
	}
	for _, entry := range input.CipherEntries {
		if archiveErr != nil {
			break
		}
		name := recoveryCipherPrefix + entry.Path
		if entry.Directory {
			archiveErr = writeRecoveryDirectory(archive, name, entry.Mode, inventory)
			continue
		}
		archiveErr = writeRecoverySourceFile(ctx, archive, name, entry, inventory)
	}
	if archiveErr == nil {
		archiveErr = writeRecoveryMember(archive, recoveryProfileKeyPath, fs.FileMode(0o600), keyBytes, inventory)
	}
	if archiveErr == nil {
		metadata := recoveryArchiveMetadata{
			Format: recoveryArchiveFormat, Version: recoveryArchiveVersion,
			AppID: input.AppID, ProfileID: input.ProfileID, VaultID: manifest.VaultID,
			MigrationStarted: manifest.MigrationStarted, EntryCount: entryCount,
			TotalBytes: totalBytes, InventorySHA256: hex.EncodeToString(inventory.Sum(nil)),
		}
		var encoded []byte
		encoded, archiveErr = json.Marshal(metadata)
		if archiveErr == nil && (int64(len(encoded)) > limits.MaxMetadataBytes || int64(len(encoded)) > limits.MaxFileBytes) {
			archiveErr = ErrRecoveryLimit
		}
		if archiveErr == nil {
			archiveErr = writeRecoveryMember(archive, recoveryMetadataPath, fs.FileMode(0o600), encoded, nil)
		}
	}
	tarCloseErr := archive.Close()
	if archiveErr == nil && tarCloseErr != nil {
		archiveErr = tarCloseErr
	}
	ageCloseErr := encrypted.Close()
	if archiveErr == nil && ageCloseErr != nil {
		archiveErr = ageCloseErr
	}
	if archiveErr != nil {
		if errors.Is(archiveErr, ErrRecoveryLimit) {
			return ErrRecoveryLimit
		}
		if ctx.Err() != nil {
			return recoveryFailure(ErrRecoveryCanceled, "encrypt", ctx.Err())
		}
		return recoveryFailure(ErrRecoveryIO, "encrypt", archiveErr)
	}
	if ctx.Err() != nil {
		return recoveryFailure(ErrRecoveryCanceled, "encrypt", ctx.Err())
	}
	return nil
}

// DecryptRecoveryArchive authenticates and validates one age v1 archive, writes
// only cipher-tree files to private caller-owned staging, and returns the opaque
// profile key and metadata only after exact tar EOF and age final-tag verification.
// Partial ciphertext may exist in staging on any error; cleanup belongs to stager's
// owner. A recovered key is never passed to the stager or written to disk here.
func DecryptRecoveryArchive(ctx context.Context, encrypted io.Reader, passphrase []byte, expected RecoveryArchiveIdentity, stager RecoveryCipherStager, requested RecoveryArchiveLimits) (RecoveryArchiveResult, error) {
	if ctx == nil || encrypted == nil || stager == nil || !validRecoveryPassphrase(passphrase) || validateRecoveryExpectedIdentity(expected) != nil {
		return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
	}
	if err := ctx.Err(); err != nil {
		return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryCanceled, "decrypt", err)
	}
	limits, err := recoveryLimits(requested)
	if err != nil {
		return RecoveryArchiveResult{}, err
	}
	boundedInput := &recoveryLimitedReader{ctx: ctx, src: encrypted, max: limits.MaxEncryptedBytes}
	ageInput, err := preflightAgeHeader(boundedInput)
	if err != nil {
		if errors.Is(err, ErrRecoveryLimit) {
			return RecoveryArchiveResult{}, ErrRecoveryLimit
		}
		if errors.Is(err, ErrRecoveryCanceled) && ctx.Err() != nil {
			return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryCanceled, "age header", ctx.Err())
		}
		if errors.Is(err, ErrRecoveryIO) {
			return RecoveryArchiveResult{}, err
		}
		return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
	}
	identity, err := age.NewScryptIdentity(string(passphrase))
	if err != nil {
		return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
	}
	identity.SetMaxWorkFactor(recoveryKDFWorkFactor)
	var plaintext io.Reader
	err = withRecoveryKDF(ctx, func() error {
		plaintext, err = age.Decrypt(ageInput, identity)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryCanceled, "decrypt", ctx.Err())
		}
		return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryAuthentication, "decrypt", err)
	}
	if err := ctx.Err(); err != nil {
		return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryCanceled, "decrypt", err)
	}

	limitedPlaintext := &recoveryLimitedReader{ctx: ctx, src: plaintext, max: limits.MaxPlaintextBytes}
	guard := newRecoveryTarGuard(limitedPlaintext, limits)
	archive := tar.NewReader(guard)
	inventory := sha256.New()
	cipherDirectories := map[string]bool{"": true}
	var manifestBytes []byte
	var keyBytes []byte
	var metadata recoveryArchiveMetadata
	var logicalMembers int
	var totalBytes int64
	var previousCipherPath string
	state := 0
	for {
		if err := ctx.Err(); err != nil {
			wipe(keyBytes)
			return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryCanceled, "decrypt", err)
		}
		header, nextErr := archive.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			wipe(keyBytes)
			return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryArchiveInvalid, "parse", nextErr)
		}
		paxPath, hasPAXPath := guard.takeLastPAXPath()
		rawPath, hasRawPath := guard.takeLastRawPath()
		if !hasRawPath || (!hasPAXPath && rawPath != header.Name) ||
			hasPAXPath != (header.Format == tar.FormatPAX) || (hasPAXPath && header.Name != paxPath) {
			wipe(keyBytes)
			return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
		}
		if err := validateRecoveryHeader(header, limits); err != nil {
			wipe(keyBytes)
			return RecoveryArchiveResult{}, err
		}
		if state == 4 {
			wipe(keyBytes)
			return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
		}
		switch {
		case state == 0:
			if header.Name != recoveryManifestPath || header.Typeflag != tar.TypeReg || header.Size > maxManifestBytes || header.Mode != 0o600 {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
			}
			manifestBytes, err = readRecoveryMember(ctx, archive, header.Size, maxManifestBytes)
			if err != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, err
			}
			if err := addRecoveryInventory(inventory, header.Name, "file", fs.FileMode(header.Mode), header.Size, manifestBytes); err != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, err
			}
			logicalMembers++
			totalBytes, err = addRecoverySize(totalBytes, header.Size)
			if err != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, err
			}
			state = 1
		case state == 1:
			if header.Name != recoveryCipherRootPath || header.Typeflag != tar.TypeDir || header.Size != 0 || header.Mode != 0o700 {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
			}
			if err := stager.MakeCipherDir("", fs.FileMode(0o700)); err != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryIO, "stage", err)
			}
			if err := addRecoveryInventory(inventory, header.Name, "dir", fs.FileMode(header.Mode), 0, nil); err != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, err
			}
			logicalMembers++
			state = 2
		case state == 2 && strings.HasPrefix(header.Name, recoveryCipherPrefix):
			relative := strings.TrimPrefix(header.Name, recoveryCipherPrefix)
			if !validateRecoveryPath(relative, limits.MaxPathBytes, limits.MaxPathDepth) || (previousCipherPath != "" && relative <= previousCipherPath) {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
			}
			parent := path.Dir(relative)
			if parent != "." && !cipherDirectories[parent] {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
			}
			previousCipherPath = relative
			mode := fs.FileMode(header.Mode)
			if header.Typeflag == tar.TypeDir {
				if header.Size != 0 {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
				}
				cipherDirectories[relative] = true
				if err := stager.MakeCipherDir(relative, fs.FileMode(0o700)); err != nil {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryIO, "stage", err)
				}
				if err := addRecoveryInventory(inventory, header.Name, "dir", mode, 0, nil); err != nil {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, err
				}
			} else if header.Typeflag == tar.TypeReg {
				if header.Size > limits.MaxFileBytes {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, ErrRecoveryLimit
				}
				totalBytes, err = addRecoverySize(totalBytes, header.Size)
				if err != nil || totalBytes > limits.MaxPlaintextBytes {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, ErrRecoveryLimit
				}
				file, createErr := stager.CreateCipherFile(relative, mode)
				if createErr != nil {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryIO, "stage", createErr)
				}
				if file == nil {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, ErrRecoveryIO
				}
				digest := sha256.New()
				copyErr := copyRecoveryFile(ctx, file, io.TeeReader(archive, digest), header.Size)
				closeErr := file.Close()
				if copyErr != nil || closeErr != nil {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryIO, "stage", errors.Join(copyErr, closeErr))
				}
				if err := addRecoveryDigestRecord(inventory, header.Name, "file", mode, header.Size, hex.EncodeToString(digest.Sum(nil))); err != nil {
					wipe(keyBytes)
					return RecoveryArchiveResult{}, err
				}
			} else {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
			}
			logicalMembers++
		case state == 2:
			if header.Name != recoveryProfileKeyPath || header.Typeflag != tar.TypeReg || header.Mode != 0o600 || header.Size != 43 {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
			}
			keyBytes, err = readRecoveryMember(ctx, archive, header.Size, 43)
			if err != nil || !validMasterKey(keyBytes) {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
			}
			if err := addRecoveryInventory(inventory, header.Name, "file", fs.FileMode(header.Mode), header.Size, keyBytes); err != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, err
			}
			logicalMembers++
			totalBytes, err = addRecoverySize(totalBytes, header.Size)
			if err != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, err
			}
			state = 3
		case state == 3:
			if header.Name != recoveryMetadataPath || header.Typeflag != tar.TypeReg || header.Mode != 0o600 {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
			}
			if header.Size > limits.MaxMetadataBytes {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, ErrRecoveryLimit
			}
			metadataBytes, readErr := readRecoveryMember(ctx, archive, header.Size, limits.MaxMetadataBytes)
			if readErr != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, readErr
			}
			metadata, err = parseRecoveryMetadata(metadataBytes)
			if err != nil {
				wipe(keyBytes)
				return RecoveryArchiveResult{}, err
			}
			state = 4
		default:
			wipe(keyBytes)
			return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
		}
		if logicalMembers > limits.MaxMembers {
			wipe(keyBytes)
			return RecoveryArchiveResult{}, ErrRecoveryLimit
		}
	}
	if state != 4 {
		wipe(keyBytes)
		return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
	}
	trailingBytes, drainErr := io.Copy(io.Discard, &recoveryContextReader{ctx: ctx, src: guard})
	if drainErr != nil {
		wipe(keyBytes)
		if errors.Is(drainErr, ErrRecoveryLimit) {
			return RecoveryArchiveResult{}, ErrRecoveryLimit
		}
		if ctx.Err() != nil {
			return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryCanceled, "drain", ctx.Err())
		}
		return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryAuthentication, "drain", drainErr)
	}
	if trailingBytes != 0 {
		wipe(keyBytes)
		return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
	}
	if err := ctx.Err(); err != nil {
		wipe(keyBytes)
		return RecoveryArchiveResult{}, recoveryFailure(ErrRecoveryCanceled, "decrypt", err)
	}
	manifest, err := parseRecoveryVaultManifest(manifestBytes)
	if err != nil {
		wipe(keyBytes)
		return RecoveryArchiveResult{}, err
	}
	if metadata.AppID != expected.AppID || (expected.ProfileID != "" && metadata.ProfileID != expected.ProfileID) ||
		metadata.VaultID != manifest.VaultID || metadata.MigrationStarted != manifest.MigrationStarted ||
		metadata.EntryCount != logicalMembers || metadata.TotalBytes != totalBytes ||
		metadata.InventorySHA256 != hex.EncodeToString(inventory.Sum(nil)) {
		wipe(keyBytes)
		return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
	}
	if !validMasterKey(keyBytes) {
		wipe(keyBytes)
		return RecoveryArchiveResult{}, ErrRecoveryArchiveInvalid
	}
	result := RecoveryArchiveResult{
		AppID: metadata.AppID, ProfileID: metadata.ProfileID, VaultID: metadata.VaultID,
		MigrationStarted: metadata.MigrationStarted, EntryCount: metadata.EntryCount,
		TotalBytes: metadata.TotalBytes, InventorySHA256: metadata.InventorySHA256,
		Manifest: append([]byte(nil), manifestBytes...), ProfileKey: append([]byte(nil), keyBytes...),
	}
	wipe(keyBytes)
	return result, nil
}

func validateRecoveryExpectedIdentity(expected RecoveryArchiveIdentity) error {
	if expected.ProfileID == "" {
		// Reuse the shared strict identifier validator while keeping AppID mandatory.
		return validateIdentity(expected.AppID, expected.AppID)
	}
	return validateIdentity(expected.AppID, expected.ProfileID)
}

func validRecoveryPassphrase(passphrase []byte) bool {
	return len(passphrase) > 0 && len(passphrase) <= recoveryMaxPassphraseBytes
}

func recoveryLimits(requested RecoveryArchiveLimits) (RecoveryArchiveLimits, error) {
	limits := RecoveryArchiveLimits{
		MaxEncryptedBytes: recoveryDefaultMaxEncryptedBytes,
		MaxPlaintextBytes: recoveryDefaultMaxPlaintextBytes,
		MaxFileBytes:      recoveryDefaultMaxFileBytes,
		MaxMembers:        recoveryDefaultMaxMembers,
		MaxPathBytes:      recoveryDefaultMaxPathBytes,
		MaxPathDepth:      recoveryDefaultMaxPathDepth,
		MaxMetadataBytes:  recoveryDefaultMaxMetadataBytes,
		MaxPAXHeaderBytes: recoveryDefaultMaxPAXHeaderBytes,
	}
	pairs := []struct {
		requested int64
		def       int64
		set       func(int64)
	}{
		{requested.MaxEncryptedBytes, limits.MaxEncryptedBytes, func(v int64) { limits.MaxEncryptedBytes = v }},
		{requested.MaxPlaintextBytes, limits.MaxPlaintextBytes, func(v int64) { limits.MaxPlaintextBytes = v }},
		{requested.MaxFileBytes, limits.MaxFileBytes, func(v int64) { limits.MaxFileBytes = v }},
		{requested.MaxMetadataBytes, limits.MaxMetadataBytes, func(v int64) { limits.MaxMetadataBytes = v }},
		{requested.MaxPAXHeaderBytes, limits.MaxPAXHeaderBytes, func(v int64) { limits.MaxPAXHeaderBytes = v }},
	}
	for _, pair := range pairs {
		if pair.requested < 0 || pair.requested > pair.def {
			return RecoveryArchiveLimits{}, ErrRecoveryArchiveInvalid
		}
		if pair.requested > 0 {
			pair.set(pair.requested)
		}
	}
	intPairs := []struct {
		requested int
		def       int
		set       func(int)
	}{
		{requested.MaxMembers, limits.MaxMembers, func(v int) { limits.MaxMembers = v }},
		{requested.MaxPathBytes, limits.MaxPathBytes, func(v int) { limits.MaxPathBytes = v }},
		{requested.MaxPathDepth, limits.MaxPathDepth, func(v int) { limits.MaxPathDepth = v }},
	}
	for _, pair := range intPairs {
		if pair.requested < 0 || pair.requested > pair.def {
			return RecoveryArchiveLimits{}, ErrRecoveryArchiveInvalid
		}
		if pair.requested > 0 {
			pair.set(pair.requested)
		}
	}
	if limits.MaxEncryptedBytes == 0 || limits.MaxPlaintextBytes == 0 || limits.MaxFileBytes == 0 || limits.MaxMembers == 0 || limits.MaxPathBytes == 0 || limits.MaxPathDepth == 0 || limits.MaxMetadataBytes == 0 || limits.MaxPAXHeaderBytes == 0 || limits.MaxFileBytes > limits.MaxPlaintextBytes {
		return RecoveryArchiveLimits{}, ErrRecoveryArchiveInvalid
	}
	return limits, nil
}

func validateRecoveryInput(input RecoveryArchiveInput, manifest, key []byte, limits RecoveryArchiveLimits) (int, int64, int64, error) {
	logicalMembers := len(input.CipherEntries) + 3 // manifest, cipher root, and profile key
	physicalHeaders := logicalMembers + 1          // metadata
	if logicalMembers < 3 || physicalHeaders > limits.MaxMembers {
		return 0, 0, 0, ErrRecoveryLimit
	}
	if int64(len(key)) != 43 {
		return 0, 0, 0, ErrRecoveryArchiveInvalid
	}
	if int64(len(manifest)) > limits.MaxFileBytes || int64(len(key)) > limits.MaxFileBytes {
		return 0, 0, 0, ErrRecoveryLimit
	}
	if !validateRecoveryPath(recoveryManifestPath, limits.MaxPathBytes, limits.MaxPathDepth) ||
		!validateRecoveryPath(recoveryCipherRootPath, limits.MaxPathBytes, limits.MaxPathDepth) ||
		!validateRecoveryPath(recoveryProfileKeyPath, limits.MaxPathBytes, limits.MaxPathDepth) ||
		!validateRecoveryPath(recoveryMetadataPath, limits.MaxPathBytes, limits.MaxPathDepth) {
		return 0, 0, 0, ErrRecoveryArchiveInvalid
	}
	seen := map[string]bool{"": true}
	previous := ""
	var total int64
	var err error
	total, err = addRecoverySize(total, int64(len(manifest)))
	if err != nil {
		return 0, 0, 0, err
	}
	total, err = addRecoverySize(total, int64(len(key)))
	if err != nil {
		return 0, 0, 0, err
	}
	var plaintextSize int64 = 1024 // canonical two-block tar terminator
	physicalPAXHeaders := 0
	for _, fixedSize := range []int64{int64(len(manifest)), 0} {
		plaintextSize, err = addTarMemberEstimate(plaintextSize, fixedSize, false)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	for _, entry := range input.CipherEntries {
		if !validateRecoveryPath(entry.Path, limits.MaxPathBytes, limits.MaxPathDepth) || entry.Path <= previous {
			return 0, 0, 0, ErrRecoveryArchiveInvalid
		}
		previous = entry.Path
		if entry.Mode&^fs.FileMode(0o777) != 0 || entry.Mode < 0 {
			return 0, 0, 0, ErrRecoveryArchiveInvalid
		}
		parent := path.Dir(entry.Path)
		if parent != "." && !seen[parent] {
			return 0, 0, 0, ErrRecoveryArchiveInvalid
		}
		seen[entry.Path] = entry.Directory
		name := recoveryCipherPrefix + entry.Path
		if !validateRecoveryPath(name, limits.MaxPathBytes, limits.MaxPathDepth) {
			return 0, 0, 0, ErrRecoveryLimit
		}
		needsPAX := !ustarPathFits(name)
		if needsPAX {
			if recoveryPAXPathRecordSize(name) > limits.MaxPAXHeaderBytes {
				return 0, 0, 0, ErrRecoveryLimit
			}
			physicalPAXHeaders++
		}
		if entry.Directory {
			if entry.Size != 0 || entry.Reader != nil {
				return 0, 0, 0, ErrRecoveryArchiveInvalid
			}
		} else {
			if entry.Size < 0 || entry.Size > limits.MaxFileBytes || entry.Size > recoveryMaxUSTARFileBytes || entry.Reader == nil {
				return 0, 0, 0, ErrRecoveryLimit
			}
			var err error
			total, err = addRecoverySize(total, entry.Size)
			if err != nil || total > limits.MaxPlaintextBytes {
				return 0, 0, 0, ErrRecoveryLimit
			}
		}
		var err error
		plaintextSize, err = addTarMemberEstimate(plaintextSize, entry.Size, needsPAX)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	if physicalHeaders+physicalPAXHeaders > limits.MaxMembers {
		return 0, 0, 0, ErrRecoveryLimit
	}
	plaintextSize, err = addTarMemberEstimate(plaintextSize, int64(len(key)), false)
	if err != nil {
		return 0, 0, 0, err
	}
	placeholder := recoveryArchiveMetadata{
		Format: recoveryArchiveFormat, Version: recoveryArchiveVersion,
		AppID: input.AppID, ProfileID: input.ProfileID, VaultID: "0123456789abcdef0123456789abcdef",
		MigrationStarted: false, EntryCount: logicalMembers, TotalBytes: total,
		InventorySHA256: strings.Repeat("0", sha256.Size*2),
	}
	placeholderBytes, err := json.Marshal(placeholder)
	if err != nil || int64(len(placeholderBytes)) > limits.MaxMetadataBytes || int64(len(placeholderBytes)) > limits.MaxFileBytes {
		return 0, 0, 0, ErrRecoveryLimit
	}
	plaintextSize, err = addTarMemberEstimate(plaintextSize, int64(len(placeholderBytes)), false)
	if err != nil || plaintextSize > limits.MaxPlaintextBytes {
		return 0, 0, 0, ErrRecoveryLimit
	}
	if total > limits.MaxPlaintextBytes {
		return 0, 0, 0, ErrRecoveryLimit
	}
	return logicalMembers, total, plaintextSize, nil
}

func parseRecoveryVaultManifest(data []byte) (vaultManifest, error) {
	if len(data) == 0 || len(data) > maxManifestBytes {
		return vaultManifest{}, ErrRecoveryArchiveInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest vaultManifest
	if decoder.Decode(&manifest) != nil {
		return vaultManifest{}, ErrRecoveryArchiveInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return vaultManifest{}, ErrRecoveryArchiveInvalid
	}
	if manifest.Format != manifestFormat {
		return vaultManifest{}, ErrRecoveryArchiveInvalid
	}
	if manifest.Version != manifestVersion {
		return vaultManifest{}, ErrRecoveryUnsupportedVersion
	}
	vaultID, err := hex.DecodeString(manifest.VaultID)
	if err != nil || len(vaultID) != 16 || hex.EncodeToString(vaultID) != manifest.VaultID || manifest.State != manifestReadyState {
		return vaultManifest{}, ErrRecoveryArchiveInvalid
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(canonical, data) {
		return vaultManifest{}, ErrRecoveryArchiveInvalid
	}
	return manifest, nil
}

func parseRecoveryMetadata(data []byte) (recoveryArchiveMetadata, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var metadata recoveryArchiveMetadata
	if decoder.Decode(&metadata) != nil {
		return recoveryArchiveMetadata{}, ErrRecoveryArchiveInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return recoveryArchiveMetadata{}, ErrRecoveryArchiveInvalid
	}
	if metadata.Format != recoveryArchiveFormat {
		return recoveryArchiveMetadata{}, ErrRecoveryArchiveInvalid
	}
	if metadata.Version != recoveryArchiveVersion {
		return recoveryArchiveMetadata{}, ErrRecoveryUnsupportedVersion
	}
	if validateIdentity(metadata.AppID, metadata.ProfileID) != nil || metadata.EntryCount < 3 || metadata.TotalBytes < 0 || len(metadata.InventorySHA256) != sha256.Size*2 {
		return recoveryArchiveMetadata{}, ErrRecoveryArchiveInvalid
	}
	digest, err := hex.DecodeString(metadata.InventorySHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != metadata.InventorySHA256 {
		return recoveryArchiveMetadata{}, ErrRecoveryArchiveInvalid
	}
	canonical, err := json.Marshal(metadata)
	if err != nil || !bytes.Equal(canonical, data) {
		return recoveryArchiveMetadata{}, ErrRecoveryArchiveInvalid
	}
	return metadata, nil
}

func preflightAgeHeader(source io.Reader) (io.Reader, error) {
	buffered := bufio.NewReaderSize(source, 64*1024)
	var header bytes.Buffer
	readLine := func() ([]byte, error) {
		line, err := buffered.ReadSlice('\n')
		if err != nil {
			switch {
			case errors.Is(err, ErrRecoveryLimit), errors.Is(err, ErrRecoveryCanceled):
				return nil, err
			case errors.Is(err, bufio.ErrBufferFull):
				return nil, ErrRecoveryLimit
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				return nil, ErrRecoveryArchiveInvalid
			default:
				return nil, recoveryFailure(ErrRecoveryIO, "age header", err)
			}
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			return nil, ErrRecoveryArchiveInvalid
		}
		if len(line) > 64*1024 || header.Len()+len(line) > recoveryMaxAgeHeaderBytes {
			return nil, ErrRecoveryLimit
		}
		if bytes.Contains(line, []byte{'\r'}) {
			return nil, ErrRecoveryArchiveInvalid
		}
		_, _ = header.Write(line)
		return append([]byte(nil), line[:len(line)-1]...), nil
	}
	magic, err := readLine()
	if err != nil {
		return nil, err
	}
	if string(magic) != "age-encryption.org/v1" {
		return nil, ErrRecoveryArchiveInvalid
	}
	line, err := readLine()
	if err != nil {
		return nil, err
	}
	stanzas := 0
	for {
		if bytes.HasPrefix(line, []byte("-> ")) {
			stanzas++
			if stanzas > recoveryMaxAgeStanzas || stanzas != 1 {
				return nil, ErrRecoveryArchiveInvalid
			}
			fields := strings.Split(string(line[3:]), " ")
			if len(fields) > recoveryMaxAgeStanzaArgs || len(fields) != 3 || fields[0] != "scrypt" || fields[1] == "" {
				return nil, ErrRecoveryArchiveInvalid
			}
			workFactor, parseErr := strconv.Atoi(fields[2])
			if parseErr != nil || workFactor < 1 || workFactor > recoveryKDFWorkFactor {
				return nil, ErrRecoveryArchiveInvalid
			}
			bodyLines := 0
			for {
				line, err = readLine()
				if err != nil {
					return nil, err
				}
				if bytes.HasPrefix(line, []byte("-> ")) || bytes.HasPrefix(line, []byte("--- ")) {
					break
				}
				if len(line) == 0 {
					return nil, ErrRecoveryArchiveInvalid
				}
				for _, c := range line {
					if c < 33 || c > 126 {
						return nil, ErrRecoveryArchiveInvalid
					}
				}
				bodyLines++
			}
			if bodyLines == 0 {
				return nil, ErrRecoveryArchiveInvalid
			}
			if bytes.HasPrefix(line, []byte("--- ")) {
				if len(line) <= 4 || bytes.Contains(line[4:], []byte{' '}) || stanzas != 1 {
					return nil, ErrRecoveryArchiveInvalid
				}
				return io.MultiReader(bytes.NewReader(header.Bytes()), buffered), nil
			}
			continue
		}
		return nil, ErrRecoveryArchiveInvalid
	}
}

func withRecoveryKDF(ctx context.Context, run func() error) error {
	select {
	case recoveryKDFGate <- struct{}{}:
	case <-ctx.Done():
		return recoveryFailure(ErrRecoveryCanceled, "kdf", ctx.Err())
	}
	defer func() { <-recoveryKDFGate }()
	if err := ctx.Err(); err != nil {
		return recoveryFailure(ErrRecoveryCanceled, "kdf", err)
	}
	err := run()
	if ctx.Err() != nil {
		return recoveryFailure(ErrRecoveryCanceled, "kdf", ctx.Err())
	}
	return err
}

func writeRecoveryMember(archive *tar.Writer, name string, mode fs.FileMode, body []byte, inventory hash.Hash) error {
	if err := archive.WriteHeader(recoveryTarHeader(name, int64(mode), int64(len(body)), tar.TypeReg)); err != nil {
		return err
	}
	if err := writeAll(archive, body); err != nil {
		return err
	}
	if inventory != nil {
		return addRecoveryInventory(inventory, name, "file", mode, int64(len(body)), body)
	}
	return nil
}

func writeRecoveryDirectory(archive *tar.Writer, name string, mode fs.FileMode, inventory hash.Hash) error {
	if err := archive.WriteHeader(recoveryTarHeader(name, int64(mode), 0, tar.TypeDir)); err != nil {
		return err
	}
	return addRecoveryInventory(inventory, name, "dir", mode, 0, nil)
}

func writeRecoverySourceFile(ctx context.Context, archive *tar.Writer, name string, entry RecoveryCipherEntry, inventory hash.Hash) error {
	if err := archive.WriteHeader(recoveryTarHeader(name, int64(entry.Mode), entry.Size, tar.TypeReg)); err != nil {
		return err
	}
	digest := sha256.New()
	reader := &recoveryContextReader{ctx: ctx, src: entry.Reader}
	if err := copyRecoveryFile(ctx, archive, io.TeeReader(reader, digest), entry.Size); err != nil {
		return err
	}
	var extra [1]byte
	n, err := io.ReadFull(reader, extra[:])
	if ctx.Err() != nil {
		return recoveryFailure(ErrRecoveryCanceled, "source", ctx.Err())
	}
	if n != 0 {
		return ErrRecoveryArchiveInvalid
	}
	if err == io.EOF {
		return addRecoveryDigestRecord(inventory, name, "file", entry.Mode, entry.Size, hex.EncodeToString(digest.Sum(nil)))
	}
	if err != nil {
		return recoveryFailure(ErrRecoveryIO, "source EOF", err)
	}
	return ErrRecoveryArchiveInvalid
}

func recoveryTarHeader(name string, mode, size int64, typeflag byte) *tar.Header {
	format := tar.FormatUSTAR
	if !ustarPathFits(name) {
		format = tar.FormatPAX
	}
	return &tar.Header{
		Name: name, Mode: mode, Size: size, Typeflag: typeflag,
		ModTime: time.Unix(0, 0).UTC(), Format: format,
	}
}

func addRecoveryInventory(inventory hash.Hash, name, kind string, mode fs.FileMode, size int64, body []byte) error {
	digest := ""
	if kind == "file" {
		digestBytes := sha256.Sum256(body)
		digest = hex.EncodeToString(digestBytes[:])
	}
	return addRecoveryDigestRecord(inventory, name, kind, mode, size, digest)
}

func addRecoveryDigestRecord(inventory hash.Hash, name, kind string, mode fs.FileMode, size int64, digest string) error {
	record, err := json.Marshal(recoveryInventoryRecord{Path: name, Kind: kind, Mode: int64(mode.Perm()), Size: size, SHA256: digest})
	if err != nil {
		return ErrRecoveryArchiveInvalid
	}
	if _, err := inventory.Write(record); err != nil {
		return recoveryFailure(ErrRecoveryIO, "inventory", err)
	}
	if _, err := inventory.Write([]byte{'\n'}); err != nil {
		return recoveryFailure(ErrRecoveryIO, "inventory", err)
	}
	return nil
}

func readRecoveryMember(ctx context.Context, archive *tar.Reader, size, max int64) ([]byte, error) {
	if size < 0 || size > max || size > int64(int(^uint(0)>>1)) {
		return nil, ErrRecoveryLimit
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(&recoveryContextReader{ctx: ctx, src: archive}, body); err != nil {
		if ctx.Err() != nil {
			return nil, recoveryFailure(ErrRecoveryCanceled, "read", ctx.Err())
		}
		return nil, recoveryFailure(ErrRecoveryArchiveInvalid, "read", err)
	}
	return body, nil
}

func copyRecoveryFile(ctx context.Context, dst io.Writer, src io.Reader, size int64) error {
	if size < 0 {
		return ErrRecoveryArchiveInvalid
	}
	checked := &recoveryCheckedWriter{ctx: ctx, dst: dst}
	buffer := make([]byte, recoveryCopyBufferBytes)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return recoveryFailure(ErrRecoveryCanceled, "copy", err)
		}
		want := int64(len(buffer))
		if remaining < want {
			want = remaining
		}
		n, readErr := io.ReadFull(src, buffer[:int(want)])
		if n > 0 {
			written, writeErr := checked.Write(buffer[:n])
			remaining -= int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return recoveryFailure(ErrRecoveryIO, "short write", io.ErrShortWrite)
			}
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return recoveryFailure(ErrRecoveryCanceled, "copy", ctx.Err())
			}
			return recoveryFailure(ErrRecoveryIO, "copy", readErr)
		}
	}
	return nil
}

func writeAll(dst io.Writer, body []byte) error {
	for len(body) > 0 {
		n, err := dst.Write(body)
		if n < 0 || n > len(body) {
			return recoveryFailure(ErrRecoveryIO, "write", io.ErrShortWrite)
		}
		body = body[n:]
		if err != nil {
			return recoveryFailure(ErrRecoveryIO, "write", err)
		}
		if n == 0 {
			return recoveryFailure(ErrRecoveryIO, "short write", io.ErrShortWrite)
		}
	}
	return nil
}

func validateRecoveryHeader(header *tar.Header, limits RecoveryArchiveLimits) error {
	if header == nil || header.Name == "" || header.Linkname != "" || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || header.Devmajor != 0 || header.Devminor != 0 || len(header.Xattrs) != 0 || !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() || header.ModTime.Unix() != 0 || header.ModTime.Nanosecond() != 0 {
		return ErrRecoveryArchiveInvalid
	}
	if header.Mode < 0 || header.Mode > 0o777 || header.Size < 0 {
		return ErrRecoveryArchiveInvalid
	}
	if header.Size > limits.MaxFileBytes {
		return ErrRecoveryLimit
	}
	if !validateRecoveryPath(header.Name, limits.MaxPathBytes, limits.MaxPathDepth) {
		return ErrRecoveryArchiveInvalid
	}
	switch header.Format {
	case tar.FormatUSTAR:
		if len(header.PAXRecords) != 0 || !ustarPathFits(header.Name) {
			return ErrRecoveryArchiveInvalid
		}
	case tar.FormatPAX:
		if len(header.PAXRecords) != 1 || header.PAXRecords["path"] != header.Name || ustarPathFits(header.Name) {
			return ErrRecoveryArchiveInvalid
		}
	default:
		return ErrRecoveryArchiveInvalid
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
		return ErrRecoveryArchiveInvalid
	}
	if header.Typeflag == tar.TypeDir && header.Size != 0 {
		return ErrRecoveryArchiveInvalid
	}
	return nil
}

func validateRecoveryPath(name string, maxBytes, maxDepth int) bool {
	if name == "" || len(name) > maxBytes || !utf8.ValidString(name) || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, "\\") || strings.Contains(name, ":") || strings.HasSuffix(name, "/") {
		return false
	}
	depth := 0
	for _, component := range strings.Split(name, "/") {
		depth++
		if component == "" || component == "." || component == ".." || len(component) > 255 || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return false
		}
		for _, c := range component {
			if c < 0x20 || c > 0x7e {
				return false
			}
		}
		if recoveryWindowsReservedComponent(component) {
			return false
		}
	}
	return depth <= maxDepth
}

func recoveryWindowsReservedComponent(component string) bool {
	base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func ustarPathFits(name string) bool {
	if len(name) <= 100 {
		return true
	}
	for separator := strings.LastIndexByte(name, '/'); separator >= 0; separator = strings.LastIndexByte(name[:separator], '/') {
		if len(name[separator+1:]) <= 100 && len(name[:separator]) <= 155 {
			return true
		}
	}
	return false
}

func recoveryPAXPathRecordSize(name string) int64 {
	base := int64(len("path=") + len(name) + 1) // key/value and terminating newline
	length := base + 2                          // one decimal digit and the separating space
	for {
		next := base + 1 + int64(len(strconv.FormatInt(length, 10)))
		if next == length {
			return length
		}
		length = next
	}
}

func addTarMemberEstimate(current, size int64, pax bool) (int64, error) {
	if size < 0 || size > int64(^uint64(0)>>1)-511 {
		return 0, ErrRecoveryLimit
	}
	padded := ((size + 511) / 512) * 512
	added := int64(512) + padded
	if pax {
		paxSize := int64(512) + ((int64(recoveryMaxPAXPathRecordSize) + 511) / 512 * 512)
		if added > int64(^uint64(0)>>1)-paxSize {
			return 0, ErrRecoveryLimit
		}
		added += paxSize
	}
	if current > int64(^uint64(0)>>1)-added {
		return 0, ErrRecoveryLimit
	}
	return current + added, nil
}

func addRecoverySize(current, add int64) (int64, error) {
	if current < 0 || add < 0 || current > int64(^uint64(0)>>1)-add {
		return 0, ErrRecoveryLimit
	}
	return current + add, nil
}

type recoveryCheckedWriter struct {
	ctx context.Context
	dst io.Writer
}

func (w *recoveryCheckedWriter) Write(p []byte) (int, error) {
	if w.ctx != nil {
		if err := w.ctx.Err(); err != nil {
			return 0, recoveryFailure(ErrRecoveryCanceled, "write", err)
		}
	}
	n, err := w.dst.Write(p)
	if n < 0 || n > len(p) {
		return 0, recoveryFailure(ErrRecoveryIO, "write", io.ErrShortWrite)
	}
	if err == nil && n != len(p) {
		return n, recoveryFailure(ErrRecoveryIO, "short write", io.ErrShortWrite)
	}
	return n, err
}

type recoveryLimitedWriter struct {
	ctx context.Context
	dst io.Writer
	max int64
	n   int64
}

func (w *recoveryLimitedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, recoveryFailure(ErrRecoveryCanceled, "write", err)
	}
	remaining := w.max - w.n
	if remaining < 0 {
		return 0, ErrRecoveryLimit
	}
	truncated := int64(len(p)) > remaining
	if truncated {
		p = p[:int(remaining)]
	}
	checked := &recoveryCheckedWriter{ctx: w.ctx, dst: w.dst}
	n, err := checked.Write(p)
	w.n += int64(n)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, recoveryFailure(ErrRecoveryIO, "short write", io.ErrShortWrite)
	}
	if truncated {
		return n, ErrRecoveryLimit
	}
	return n, nil
}

type recoveryLimitedReader struct {
	ctx context.Context
	src io.Reader
	max int64
	n   int64
}

func (r *recoveryLimitedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, recoveryFailure(ErrRecoveryCanceled, "read", err)
	}
	if len(p) == 0 {
		return 0, nil
	}
	remaining := r.max - r.n
	if remaining == 0 {
		var probe [1]byte
		n, err := r.src.Read(probe[:])
		if n > 0 {
			return 0, ErrRecoveryLimit
		}
		if r.ctx.Err() != nil {
			return 0, recoveryFailure(ErrRecoveryCanceled, "read", r.ctx.Err())
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:int(remaining)]
	}
	n, err := r.src.Read(p)
	r.n += int64(n)
	if r.ctx.Err() != nil {
		return n, recoveryFailure(ErrRecoveryCanceled, "read", r.ctx.Err())
	}
	return n, err
}

type recoveryContextReader struct {
	ctx context.Context
	src io.Reader
}

func (r *recoveryContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, recoveryFailure(ErrRecoveryCanceled, "read", err)
	}
	n, err := r.src.Read(p)
	if r.ctx.Err() != nil {
		return n, recoveryFailure(ErrRecoveryCanceled, "read", r.ctx.Err())
	}
	return n, err
}

type recoveryTarGuard struct {
	source           io.Reader
	limits           RecoveryArchiveLimits
	block            [512]byte
	blockOffset      int
	bodyRemaining    int64
	paddingRemaining int64
	padding          [512]byte
	paddingOffset    int
	paxBody          [recoveryMaxPAXPathRecordSize]byte
	paxBodySize      int
	paxBodyOffset    int
	paxPadding       int
	state            uint8
	zeroBlocks       int
	headerCount      int
	headerType       byte
	pendingPAX       bool
	pendingPAXPath   string
	lastPAXPath      string
	hasLastPAXPath   bool
	lastRawPath      string
	hasLastRawPath   bool
}

const (
	recoveryTarReadHeader uint8 = iota
	recoveryTarEmitHeader
	recoveryTarReadBody
	recoveryTarReadPadding
	recoveryTarEmitPadding
	recoveryTarEnded
	recoveryTarReadPAXBody
	recoveryTarReadPAXPadding
	recoveryTarEmitPAXBody
	recoveryTarEmitPAXPadding
)

func newRecoveryTarGuard(source io.Reader, limits RecoveryArchiveLimits) *recoveryTarGuard {
	return &recoveryTarGuard{source: source, limits: limits, state: recoveryTarReadHeader}
}

func parseRecoveryPAXPathRecord(body []byte, limits RecoveryArchiveLimits) (string, error) {
	if len(body) < len("1 path=\n")+1 || body[len(body)-1] != '\n' {
		return "", ErrRecoveryArchiveInvalid
	}
	space := bytes.IndexByte(body, ' ')
	if space <= 0 || (space > 1 && body[0] == '0') {
		return "", ErrRecoveryArchiveInvalid
	}
	for _, digit := range body[:space] {
		if digit < '0' || digit > '9' {
			return "", ErrRecoveryArchiveInvalid
		}
	}
	declared, err := strconv.ParseInt(string(body[:space]), 10, 32)
	if err != nil || declared != int64(len(body)) || !bytes.HasPrefix(body[space+1:], []byte("path=")) {
		return "", ErrRecoveryArchiveInvalid
	}
	valueStart := space + 1 + len("path=")
	if valueStart >= len(body)-1 {
		return "", ErrRecoveryArchiveInvalid
	}
	name := string(body[valueStart : len(body)-1])
	if recoveryPAXPathRecordSize(name) != int64(len(body)) || !validateRecoveryPath(name, limits.MaxPathBytes, limits.MaxPathDepth) || ustarPathFits(name) {
		return "", ErrRecoveryArchiveInvalid
	}
	return name, nil
}

func rawRecoveryTarString(field []byte) (string, bool) {
	nul := bytes.IndexByte(field, 0)
	if nul < 0 {
		return string(field), true
	}
	if !isZeroTarBlock(field[nul:]) {
		return "", false
	}
	return string(field[:nul]), true
}

func rawRecoveryTarText(text string) bool {
	for _, character := range text {
		if character < 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}

func recoveryRawMemberPath(block []byte) (string, bool) {
	if len(block) != 512 || !bytes.Equal(block[257:265], []byte("ustar\x0000")) || !isZeroTarBlock(block[500:512]) {
		return "", false
	}
	mode, modeErr := parseRecoveryTarSize(block[100:108])
	uid, uidErr := parseRecoveryTarSize(block[108:116])
	gid, gidErr := parseRecoveryTarSize(block[116:124])
	size, sizeErr := parseRecoveryTarSize(block[124:136])
	mtime, mtimeErr := parseRecoveryTarSize(block[136:148])
	devMajor, majorErr := parseRecoveryTarSize(block[329:337])
	devMinor, minorErr := parseRecoveryTarSize(block[337:345])
	if modeErr != nil || mode < 0 || mode > 0o777 || uidErr != nil || uid != 0 || gidErr != nil || gid != 0 ||
		sizeErr != nil || size < 0 || mtimeErr != nil || mtime != 0 || majorErr != nil || devMajor != 0 || minorErr != nil || devMinor != 0 {
		return "", false
	}
	name, nameOK := rawRecoveryTarString(block[:100])
	linkname, linkOK := rawRecoveryTarString(block[157:257])
	uname, unameOK := rawRecoveryTarString(block[265:297])
	gname, gnameOK := rawRecoveryTarString(block[297:329])
	prefix, prefixOK := rawRecoveryTarString(block[345:500])
	if !nameOK || name == "" || !linkOK || linkname != "" || !unameOK || uname != "" || !gnameOK || gname != "" || !prefixOK ||
		!rawRecoveryTarText(name) || !rawRecoveryTarText(prefix) {
		return "", false
	}
	if prefix != "" {
		return prefix + "/" + name, true
	}
	return name, true
}

func validRawRecoveryPAXHeader(block []byte) bool {
	if len(block) != 512 || !bytes.Equal(block[257:265], []byte("ustar\x0000")) ||
		!isZeroTarBlock(block[265:500]) || !isZeroTarBlock(block[500:512]) {
		return false
	}
	for _, field := range [][]byte{block[100:108], block[108:116], block[116:124], block[136:148], block[329:345]} {
		value, err := parseRecoveryTarSize(field)
		if err != nil || value != 0 {
			return false
		}
	}
	nameField := block[:100]
	nameBytes := bytes.TrimRight(nameField, "\x00")
	if bytes.IndexByte(nameBytes, 0) >= 0 {
		return false
	}
	name := string(nameBytes)
	if name == "" || !utf8.ValidString(name) || path.Clean(name) != name || strings.HasSuffix(name, "/") || strings.ContainsAny(name, "\\:") {
		return false
	}
	hasPAXDirectory := false
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		if component == "PaxHeaders.0" {
			hasPAXDirectory = true
		}
		for _, character := range component {
			if character < 0x20 || character > 0x7e {
				return false
			}
		}
	}
	return hasPAXDirectory
}

func (r *recoveryTarGuard) takeLastPAXPath() (string, bool) {
	name, ok := r.lastPAXPath, r.hasLastPAXPath
	r.lastPAXPath = ""
	r.hasLastPAXPath = false
	return name, ok
}

func (r *recoveryTarGuard) takeLastRawPath() (string, bool) {
	name, ok := r.lastRawPath, r.hasLastRawPath
	r.lastRawPath = ""
	r.hasLastRawPath = false
	return name, ok
}

func (r *recoveryTarGuard) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		switch r.state {
		case recoveryTarReadHeader:
			if _, err := io.ReadFull(r.source, r.block[:]); err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					return 0, io.ErrUnexpectedEOF
				}
				return 0, err
			}
			if isZeroTarBlock(r.block[:]) {
				if r.pendingPAX || r.zeroBlocks >= 2 {
					return 0, ErrRecoveryArchiveInvalid
				}
				r.zeroBlocks++
				r.headerType = 0
				r.hasLastPAXPath = false
				r.hasLastRawPath = false
				r.state = recoveryTarEmitHeader
				r.blockOffset = 0
				continue
			}
			if r.zeroBlocks != 0 {
				return 0, ErrRecoveryArchiveInvalid
			}
			r.headerCount++
			if r.headerCount > r.limits.MaxMembers {
				return 0, ErrRecoveryLimit
			}
			typeflag := r.block[156]
			size, err := parseRecoveryTarSize(r.block[124:136])
			if err != nil {
				return 0, ErrRecoveryArchiveInvalid
			}
			r.headerType = typeflag
			r.lastRawPath = ""
			r.hasLastRawPath = false
			if typeflag == tar.TypeReg || typeflag == tar.TypeDir {
				r.lastRawPath, r.hasLastRawPath = recoveryRawMemberPath(r.block[:])
				if !r.hasLastRawPath {
					return 0, ErrRecoveryArchiveInvalid
				}
			}
			switch typeflag {
			case tar.TypeXHeader:
				if r.pendingPAX || size == 0 {
					return 0, ErrRecoveryArchiveInvalid
				}
				if size > r.limits.MaxPAXHeaderBytes || size > int64(len(r.paxBody)) {
					return 0, ErrRecoveryLimit
				}
				if !validRawRecoveryPAXHeader(r.block[:]) {
					return 0, ErrRecoveryArchiveInvalid
				}
				r.pendingPAX = true
				r.paxBodySize = int(size)
				r.paxBodyOffset = 0
				r.bodyRemaining = size
				r.paddingRemaining = (512 - size%512) % 512
				r.state = recoveryTarReadPAXBody
				continue
			case tar.TypeReg:
				if size > r.limits.MaxFileBytes {
					return 0, ErrRecoveryLimit
				}
				if r.pendingPAX {
					r.lastPAXPath = r.pendingPAXPath
					r.hasLastPAXPath = true
					r.pendingPAXPath = ""
				} else {
					r.hasLastPAXPath = false
				}
				r.pendingPAX = false
			case tar.TypeDir:
				if size != 0 {
					return 0, ErrRecoveryArchiveInvalid
				}
				if r.pendingPAX {
					r.lastPAXPath = r.pendingPAXPath
					r.hasLastPAXPath = true
					r.pendingPAXPath = ""
				} else {
					r.hasLastPAXPath = false
				}
				r.pendingPAX = false
			default:
				return 0, ErrRecoveryArchiveInvalid
			}
			r.bodyRemaining = size
			r.paddingRemaining = (512 - size%512) % 512
			r.state = recoveryTarEmitHeader
			r.blockOffset = 0
		case recoveryTarReadPAXBody:
			if _, err := io.ReadFull(r.source, r.paxBody[:r.paxBodySize]); err != nil {
				return 0, io.ErrUnexpectedEOF
			}
			paxPath, err := parseRecoveryPAXPathRecord(r.paxBody[:r.paxBodySize], r.limits)
			if err != nil {
				return 0, err
			}
			r.pendingPAXPath = paxPath
			r.bodyRemaining = 0
			if r.paddingRemaining > 0 {
				r.state = recoveryTarReadPAXPadding
			} else {
				r.state = recoveryTarEmitHeader
				r.blockOffset = 0
			}
		case recoveryTarReadPAXPadding:
			padding := int(r.paddingRemaining)
			if _, err := io.ReadFull(r.source, r.padding[:padding]); err != nil {
				return 0, io.ErrUnexpectedEOF
			}
			if !isZeroTarBlock(r.padding[:padding]) {
				return 0, ErrRecoveryArchiveInvalid
			}
			r.paddingRemaining = 0
			r.paxPadding = padding
			r.state = recoveryTarEmitHeader
			r.blockOffset = 0
		case recoveryTarEmitHeader:
			n := copy(p, r.block[r.blockOffset:])
			r.blockOffset += n
			if r.blockOffset == len(r.block) {
				r.blockOffset = 0
				if r.zeroBlocks == 2 {
					r.state = recoveryTarEnded
				} else if r.headerType == tar.TypeXHeader {
					r.state = recoveryTarEmitPAXBody
				} else if r.bodyRemaining > 0 {
					r.state = recoveryTarReadBody
				} else if r.paddingRemaining > 0 {
					r.state = recoveryTarReadPadding
				} else {
					r.state = recoveryTarReadHeader
				}
			}
			return n, nil
		case recoveryTarEmitPAXBody:
			n := copy(p, r.paxBody[r.paxBodyOffset:r.paxBodySize])
			r.paxBodyOffset += n
			if r.paxBodyOffset == r.paxBodySize {
				if r.paxPadding > 0 {
					r.state = recoveryTarEmitPAXPadding
				} else {
					r.state = recoveryTarReadHeader
				}
			}
			return n, nil
		case recoveryTarEmitPAXPadding:
			n := len(p)
			if n > r.paxPadding {
				n = r.paxPadding
			}
			clear(p[:n])
			r.paxPadding -= n
			if r.paxPadding == 0 {
				r.state = recoveryTarReadHeader
			}
			return n, nil
		case recoveryTarReadBody:
			want := int64(len(p))
			if want > r.bodyRemaining {
				want = r.bodyRemaining
			}
			n, err := r.source.Read(p[:int(want)])
			r.bodyRemaining -= int64(n)
			if r.bodyRemaining == 0 {
				if r.paddingRemaining > 0 {
					r.state = recoveryTarReadPadding
				} else {
					r.state = recoveryTarReadHeader
				}
			}
			if err == io.EOF && r.bodyRemaining > 0 {
				return n, io.ErrUnexpectedEOF
			}
			return n, err
		case recoveryTarReadPadding:
			padding := int(r.paddingRemaining)
			if padding > len(r.padding) {
				return 0, ErrRecoveryArchiveInvalid
			}
			if _, err := io.ReadFull(r.source, r.padding[:padding]); err != nil {
				return 0, io.ErrUnexpectedEOF
			}
			if !isZeroTarBlock(r.padding[:padding]) {
				return 0, ErrRecoveryArchiveInvalid
			}
			r.paddingOffset = 0
			r.state = recoveryTarEmitPadding
		case recoveryTarEmitPadding:
			n := copy(p, r.padding[r.paddingOffset:int(r.paddingRemaining)])
			r.paddingOffset += n
			if r.paddingOffset == int(r.paddingRemaining) {
				r.paddingOffset = 0
				r.paddingRemaining = 0
				r.state = recoveryTarReadHeader
			}
			return n, nil
		case recoveryTarEnded:
			return r.source.Read(p)
		default:
			return 0, ErrRecoveryArchiveInvalid
		}
	}
}

func isZeroTarBlock(block []byte) bool {
	for _, b := range block {
		if b != 0 {
			return false
		}
	}
	return true
}

func parseRecoveryTarSize(field []byte) (int64, error) {
	if len(field) == 0 || field[0]&0x80 != 0 {
		return 0, ErrRecoveryArchiveInvalid
	}
	value := bytes.Trim(field, " \x00")
	if len(value) == 0 {
		return 0, nil
	}
	for _, b := range value {
		if b < '0' || b > '7' {
			return 0, ErrRecoveryArchiveInvalid
		}
	}
	parsed, err := strconv.ParseUint(string(value), 8, 63)
	if err != nil {
		return 0, ErrRecoveryArchiveInvalid
	}
	return int64(parsed), nil
}
