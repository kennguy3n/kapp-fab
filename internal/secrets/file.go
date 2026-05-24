package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileProvider resolves secrets from files on disk. It exists
// for the Docker / Kubernetes secret-mount pattern, where the
// orchestrator drops each secret at a path like
// /run/secrets/<name> and the application reads the file at
// startup.
//
// The mapping is: key -> filepath.Join(rootDir, normalisedKey),
// where the normalisation replaces "/" with the OS path
// separator and disallows "." segments so a malicious caller
// can't escape rootDir via traversal. Empty rootDir is rejected
// at construction time -- an unprefixed file:// lookup would
// let any process-readable file shadow a Kapp secret.
//
// Versioning: the SecretValue.Version field is the file's
// modification time in unix nanoseconds. This is sufficient
// for the keyring's rotation-detection contract: distinct
// versions of the same secret will have distinct mtimes
// because the orchestrator re-writes the file on rotation.
// Operators who atomically swap a file (mv newfile path)
// preserve the inode but get a fresh mtime; operators who
// use a symlink swap (ln -sfn newfile path) also get a fresh
// mtime on lstat. Both are accommodated.
type FileProvider struct {
	rootDir string
}

// NewFileProvider returns a FileProvider rooted at the supplied
// directory. The directory does not need to exist at
// construction time -- the orchestrator may mount it post-boot
// -- but GetSecret will surface ErrSecretNotFound until it does.
//
// rootDir is cleaned via filepath.Clean to normalise away
// trailing slashes and "." segments, so the path-traversal
// guard in GetSecret can use a simple HasPrefix check against
// the cleaned root.
func NewFileProvider(rootDir string) (*FileProvider, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("%w: file provider rootDir empty", ErrProviderNotConfigured)
	}
	abs, err := filepath.Abs(filepath.Clean(rootDir))
	if err != nil {
		return nil, fmt.Errorf("secrets: resolve file rootDir: %w", err)
	}
	return &FileProvider{rootDir: abs}, nil
}

// Name returns the literal "file".
func (*FileProvider) Name() string { return "file" }

// GetSecret reads the file at rootDir/<normalised key>. Returns
// ErrSecretNotFound when the file is missing or empty; returns
// a non-sentinel error for I/O failures (permission, EIO).
// Trailing whitespace is trimmed from the file content because
// many orchestrators append a newline when writing secrets via
// shell redirection.
func (p *FileProvider) GetSecret(_ context.Context, key string) (SecretValue, error) {
	normalised, err := normaliseFileKey(key)
	if err != nil {
		return SecretValue{}, err
	}
	path := filepath.Join(p.rootDir, normalised)
	// Defence in depth: re-clean after join to collapse any
	// ".." sequence the orchestrator-substituted segment may
	// have introduced, then assert it still starts with the
	// configured root. Anything outside is a refusal.
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean+string(filepath.Separator), p.rootDir+string(filepath.Separator)) && clean != p.rootDir {
		return SecretValue{}, fmt.Errorf("secrets: file key %q escapes root", key)
	}
	// Open + Stat against the same file descriptor so the
	// version (mtime) corresponds to the bytes we actually read.
	// The earlier shape stat'd then ReadFile'd via path, which
	// left a microsecond window where a concurrent atomic-rename
	// (e.g. K8s secret rotation via symlink swap) could produce
	// a (content, version) tuple from two different file
	// generations.
	f, err := os.Open(clean) //nolint:gosec // G304 path is checked above against the configured root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SecretValue{}, fmt.Errorf("%w: file %s missing", ErrSecretNotFound, clean)
		}
		return SecretValue{}, fmt.Errorf("%w: open %s: %w", ErrProviderUnavailable, clean, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return SecretValue{}, fmt.Errorf("%w: stat %s: %w", ErrProviderUnavailable, clean, err)
	}
	if info.IsDir() {
		return SecretValue{}, fmt.Errorf("%w: file %s is a directory", ErrSecretNotFound, clean)
	}

	// Bound the read to a generous 1 MiB. Secrets in this
	// provider are JWT keys / tokens / passwords — never bigger
	// than a few KiB — so a multi-megabyte file is almost
	// certainly a misconfigured mount and not a legitimate value.
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return SecretValue{}, fmt.Errorf("%w: read %s: %w", ErrProviderUnavailable, clean, err)
	}
	trimmed := []byte(strings.TrimRight(string(raw), "\r\n\t "))
	if len(trimmed) == 0 {
		return SecretValue{}, fmt.Errorf("%w: file %s empty", ErrSecretNotFound, clean)
	}
	return SecretValue{
		Bytes:   trimmed,
		Version: strconv.FormatInt(info.ModTime().UnixNano(), 10),
	}, nil
}

// normaliseFileKey converts a Provider key to a relative path
// segment. Empty keys and keys containing ".." are rejected at
// this layer (the post-join check is a defence-in-depth, not the
// primary gate).
func normaliseFileKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("%w: file key empty", ErrSecretNotFound)
	}
	if strings.Contains(key, "..") {
		return "", fmt.Errorf("secrets: file key %q contains '..'", key)
	}
	// Replace forward slashes with the OS separator; leave
	// other characters alone so an operator who uses dotted
	// keys ("jwt.primary") gets a file named "jwt.primary"
	// rather than "jwt/primary".
	return filepath.FromSlash(strings.TrimPrefix(key, "/")), nil
}
