package profilevault

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

var (
	ErrToolOutputTooLarge = errors.New("profilevault: secret-tool output exceeded its limit")
	ErrSecretToolFailed   = errors.New("profilevault: secret-tool operation failed")
)

const (
	defaultSecretToolTimeout = 5 * time.Second
	maxSecretToolTimeout     = 30 * time.Second
	defaultToolOutputLimit   = 4096
	maxToolOutputLimit       = 64 * 1024
	maxKeyBytes              = 4096
)

// SecretToolKeyring adapts the secret-tool Secret Service CLI. Key material is
// sent only over stdin/stdout; it is never placed in arguments or environment.
type SecretToolKeyring struct {
	Executable     string
	Timeout        time.Duration
	MaxOutputBytes int
}

func (k *SecretToolKeyring) Lookup(ctx context.Context, appID, profileID string) ([]byte, error) {
	if err := validateIdentity(appID, profileID); err != nil {
		return nil, err
	}
	output, stderr, runErr := k.run(ctx, []string{"lookup", "application", appID, "profile", profileID}, nil)
	defer wipe(output)
	defer wipe(stderr)
	if len(output) == 0 && len(stderr) == 0 && exitCode(runErr) == 1 {
		return nil, ErrKeyNotFound
	}
	if errors.Is(runErr, ErrToolOutputTooLarge) {
		return nil, ErrToolOutputTooLarge
	}
	if runErr != nil {
		return nil, ErrSecretToolFailed
	}
	secret := output
	if len(secret) > 0 && secret[len(secret)-1] == '\n' {
		secret = secret[:len(secret)-1]
	}
	if len(secret) == 0 || len(secret) > maxKeyBytes || bytes.IndexAny(secret, "\r\n\x00") >= 0 {
		return nil, ErrSecretToolFailed
	}
	return append([]byte(nil), secret...), nil
}

func (k *SecretToolKeyring) Store(ctx context.Context, appID, profileID string, key []byte) error {
	if err := validateIdentity(appID, profileID); err != nil {
		return err
	}
	if len(key) == 0 || len(key) > maxKeyBytes || bytes.IndexAny(key, "\r\n\x00") >= 0 {
		return ErrSecretToolFailed
	}
	payload := make([]byte, len(key)+1)
	copy(payload, key)
	payload[len(key)] = '\n'
	defer wipe(payload)
	output, stderr, err := k.run(ctx, []string{"store", "--label=WhatsFeather encrypted profile key", "application", appID, "profile", profileID}, payload)
	wipe(output)
	wipe(stderr)
	if errors.Is(err, ErrToolOutputTooLarge) {
		return ErrToolOutputTooLarge
	}
	if err != nil {
		return ErrSecretToolFailed
	}
	return nil
}

func (k *SecretToolKeyring) run(parent context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	if k == nil || k.Executable == "" || parent == nil {
		return nil, nil, ErrSecretToolFailed
	}
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = defaultSecretToolTimeout
	}
	if timeout > maxSecretToolTimeout {
		timeout = maxSecretToolTimeout
	}
	limit := k.MaxOutputBytes
	if limit <= 0 {
		limit = defaultToolOutputLimit
	}
	if limit > maxToolOutputLimit {
		limit = maxToolOutputLimit
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.Executable, args...)
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := &limitedOutput{limit: limit}
	stderr := &limitedOutput{limit: limit}
	defer func() {
		wipe(stdout.Bytes())
		wipe(stderr.Bytes())
	}()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if stdout.overflow || stderr.overflow {
		return nil, nil, ErrToolOutputTooLarge
	}
	if ctx.Err() != nil {
		return nil, nil, ErrSecretToolFailed
	}
	return append([]byte(nil), stdout.Bytes()...), append([]byte(nil), stderr.Bytes()...), err
}

type limitedOutput struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (w *limitedOutput) Bytes() []byte { return w.buf.Bytes() }

func (w *limitedOutput) Len() int { return w.buf.Len() }

func (w *limitedOutput) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.buf.Write(p[:remaining])
	}
	if remaining < len(p) {
		w.overflow = true
	}
	return len(p), nil
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func validateIdentity(appID, profileID string) error {
	for _, value := range []string{appID, profileID} {
		if value == "" || len(value) > 256 || strings.IndexFunc(value, func(r rune) bool { return r == 0 || r == '\n' || r == '\r' || r < 0x20 || r == 0x7f }) >= 0 {
			return ErrSecretToolFailed
		}
	}
	return nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
