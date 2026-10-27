package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilemigration"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilevault"
)

func runOfflineMigration(ctx context.Context, sourceRoot, destinationRoot, runtimeDir string, deps bootstrapDependencies) error {
	if ctx == nil || sourceRoot == "" || destinationRoot == "" || runtimeDir == "" ||
		!filepath.IsAbs(sourceRoot) || filepath.Clean(sourceRoot) != sourceRoot ||
		!filepath.IsAbs(destinationRoot) || filepath.Clean(destinationRoot) != destinationRoot ||
		!filepath.IsAbs(runtimeDir) || filepath.Clean(runtimeDir) != runtimeDir {
		return fmt.Errorf("offline migration requires clean absolute source, destination and runtime paths")
	}
	if deps.keyring == nil || deps.openVault == nil || deps.gocryptfsPath == "" || deps.fuseUnmountPath == "" {
		return fmt.Errorf("encrypted offline migration dependencies are incomplete")
	}
	if pathContains(sourceRoot, runtimeDir) || pathContains(runtimeDir, sourceRoot) ||
		pathContains(destinationRoot, runtimeDir) || pathContains(runtimeDir, destinationRoot) {
		return fmt.Errorf("offline source, encrypted destination and runtime paths must be separate")
	}
	if err := validateNoSymlinkComponents(runtimeDir); err != nil {
		return err
	}
	plan, err := profilemigration.PrepareContext(ctx, sourceRoot, destinationRoot)
	if err != nil {
		return migrationRetryHint(err)
	}
	defer plan.Close()
	if err := ensureDirectory(runtimeDir, true); err != nil {
		return fmt.Errorf("secure encrypted-profile runtime directory")
	}
	vault, err := deps.openVault(ctx, profilevault.Options{
		RootDir: destinationRoot, RuntimeDir: runtimeDir,
		AppID: vaultAppID, ProfileID: profileKeyringIdentity(destinationRoot),
		Keyring: deps.keyring, GocryptfsPath: deps.gocryptfsPath,
		FuseUnmountPath: deps.fuseUnmountPath,
	})
	if err != nil {
		return fmt.Errorf("open encrypted migration destination: %w", err)
	}
	if vault == nil {
		return fmt.Errorf("open encrypted migration destination: empty vault owner")
	}
	mountPath := vault.MountPath()
	if !filepath.IsAbs(mountPath) || filepath.Clean(mountPath) != mountPath || filepath.Dir(mountPath) != runtimeDir {
		return closeMigrationVault(vault, plan, fmt.Errorf("encrypted migration mount is outside the private runtime directory"))
	}
	migrationErr := plan.MigrateWithIntent(ctx, mountPath, profilemigration.Options{}, vault.MarkMigrationStarted)
	return closeMigrationVault(vault, plan, migrationRetryHint(migrationErr))
}

func migrationRetryHint(err error) error {
	if !errors.Is(err, profilemigration.ErrDestinationNotEmpty) {
		return err
	}
	return fmt.Errorf("%w: interrupted migration is not resumable; keep the plaintext source and this encrypted destination unchanged, then retry with a different new --data-dir path. Do not delete the source or reset an existing vault", err)
}

func closeMigrationVault(vault vaultLifecycle, plan *profilemigration.Plan, cause error) error {
	closeCtx, cancel := context.WithTimeout(context.Background(), shutdownCloseBudget)
	defer cancel()
	var vaultErr error
	if vault != nil {
		vaultErr = vault.Close(closeCtx)
	}
	var planErr error
	if plan != nil {
		planErr = plan.Close()
	}
	return errors.Join(cause, vaultErr, planErr)
}
