package profilevault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretToolTransportsKeyOnlyOverStdinWithExactOpaqueAttributes(t *testing.T) {
	base := t.TempDir()
	audit := filepath.Join(base, "arguments")
	input := filepath.Join(base, "stdin")
	envAudit := filepath.Join(base, "environment")
	tool := writeExecutable(t, filepath.Join(base, "secret-tool"), `#!/bin/sh
printf '%s\n' "$@" > "$AUDIT"
env > "$ENV_CAPTURE"
case "$1" in
  store) cat > "$CAPTURE"; printf 'stored\n' ;;
  lookup) printf 'synthetic-master-key\n' ;;
  *) exit 23 ;;
esac
`)
	t.Setenv("AUDIT", audit)
	t.Setenv("CAPTURE", input)
	t.Setenv("ENV_CAPTURE", envAudit)
	key := []byte("synthetic-master-key-not-an-argument")
	keyring := &SecretToolKeyring{Executable: tool, Timeout: time.Second, MaxOutputBytes: 1024}

	if err := keyring.Store(context.Background(), "app.synthetic", "profile.synthetic", key); err != nil {
		t.Fatalf("Store: %v", err)
	}
	gotInput, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotInput) != string(key)+"\n" {
		t.Fatalf("store stdin = %q, want key plus one newline", gotInput)
	}
	storeArgs, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storeArgs), string(key)) {
		t.Fatal("master key appeared in secret-tool arguments")
	}
	childEnv, err := os.ReadFile(envAudit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(childEnv), string(key)) {
		t.Fatal("master key appeared in secret-tool environment")
	}
	if got, want := string(storeArgs), "store\n--label=WhatsFeather encrypted profile key\napplication\napp.synthetic\nprofile\nprofile.synthetic\n"; got != want {
		t.Fatalf("store arguments = %q, want %q", got, want)
	}

	gotKey, err := keyring.Lookup(context.Background(), "app.synthetic", "profile.synthetic")
	if err != nil || string(gotKey) != "synthetic-master-key" {
		t.Fatalf("Lookup = %q, %v", gotKey, err)
	}
	lookupArgs, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(lookupArgs), "lookup\napplication\napp.synthetic\nprofile\nprofile.synthetic\n"; got != want {
		t.Fatalf("lookup arguments = %q, want %q", got, want)
	}
}

func TestSecretToolClassifiesMissingAndRedactsSubprocessOutput(t *testing.T) {
	base := t.TempDir()
	tool := writeExecutable(t, filepath.Join(base, "secret-tool"), `#!/bin/sh
case "$MODE" in
  missing) exit 1 ;;
  leak) printf 'synthetic-secret-value\n' >&2; exit 7 ;;
  *) exit 0 ;;
esac
`)
	keyring := &SecretToolKeyring{Executable: tool, Timeout: time.Second, MaxOutputBytes: 128}
	t.Setenv("MODE", "missing")
	if _, err := keyring.Lookup(context.Background(), "app.synthetic", "profile.synthetic"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("missing lookup error = %v, want ErrKeyNotFound", err)
	}
	t.Setenv("MODE", "leak")
	_, err := keyring.Lookup(context.Background(), "app.synthetic", "profile.synthetic")
	if err == nil || strings.Contains(err.Error(), "synthetic-secret-value") {
		t.Fatalf("subprocess error was absent or leaked stderr: %v", err)
	}
}

func TestSecretToolTimeoutIsBounded(t *testing.T) {
	tool := writeExecutable(t, filepath.Join(t.TempDir(), "secret-tool"), "#!/bin/sh\nexec sleep 4\n")
	keyring := &SecretToolKeyring{Executable: tool, Timeout: 40 * time.Millisecond, MaxOutputBytes: 128}
	started := time.Now()
	_, err := keyring.Lookup(context.Background(), "app.synthetic", "profile.synthetic")
	elapsed := time.Since(started)
	if err == nil || errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("timed lookup error = %v, want generic subprocess failure", err)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("lookup exceeded bounded timeout: %v", elapsed)
	}
}

func TestSecretToolRejectsOversizedOutput(t *testing.T) {
	tool := writeExecutable(t, filepath.Join(t.TempDir(), "secret-tool"), "#!/bin/sh\nprintf '1234567890'\n")
	keyring := &SecretToolKeyring{Executable: tool, Timeout: time.Second, MaxOutputBytes: 4}
	_, err := keyring.Lookup(context.Background(), "app.synthetic", "profile.synthetic")
	if !errors.Is(err, ErrToolOutputTooLarge) {
		t.Fatalf("oversized output error = %v, want ErrToolOutputTooLarge", err)
	}
}
