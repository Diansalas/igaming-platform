// Package devfile is the devfile:// secret-store backend: provider
// credential secrets read from a local directory, for LOCAL DEVELOPMENT
// ONLY (ADR 0093 §6; security review 07-w2a-design-review-security.md
// §4.2).
//
// Never in staging or production. That is guaranteed in three places:
// New itself calls cfg.ValidateSecretBackendScheme("devfile") (APP_ENV must
// be EXPLICITLY "development"; a missing APP_ENV counts as production), the
// router build (secretstore.NewRouter) calls it again, and registration
// validates the ref's scheme against the same rule. The backend carries no
// providerkind.ProductionEligible marker, so the ADR 0085 production guard
// also refuses it if it were ever registered in production.
//
// Mapping: devfile://provider-creds/<tenant>/<domain>/<provider>/<name>?version=<v>
// reads <root>/provider-creds/<tenant>/<domain>/<provider>/<name>.v<v>.
// Files are opened only through os.OpenRoot(root), so traversal and symlink
// escape fail; every fetch re-checks the file is a regular, non-symlink
// file owned by the process euid with no group/other permission bits and a
// size of 1 B to 64 KiB. The secret is the exact file bytes (no trimming):
// an in-place edit changes the fingerprint and fails closed.
package devfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

// MaxFileBytes is the largest accepted secret file.
const MaxFileBytes = 64 << 10

var (
	domainSegment   = regexp.MustCompile(`^(payments|kyc|casino)$`)
	providerSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	nameSegment     = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)
)

// fileOwner reports a FileInfo's owner uid (injectable for tests).
type fileOwner func(fi fs.FileInfo) (uid uint32, ok bool)

// Store is the devfile:// backend. It deliberately implements neither
// providerkind.Synthetic nor providerkind.ProductionEligible.
type Store struct {
	root  *os.Root
	owner fileOwner
	euid  uint32
}

// New validates the environment and the root directory and opens it.
// It refuses: any environment other than explicit development, a
// non-Unix OS, a root that is not a directory (or is a symlink), a root
// not owned by the process euid, and a root with any group/other
// permission bit.
func New(cfg config.Config, root string) (*Store, error) {
	if err := cfg.ValidateSecretBackendScheme(config.SecretBackendDevFile); err != nil {
		return nil, fmt.Errorf("devfile: %w", err)
	}
	return newStore(root, platformOwner, platformEUID())
}

func newStore(root string, owner fileOwner, euid uint32) (*Store, error) {
	if !supported {
		return nil, errors.New("devfile: the devfile backend is supported on Unix only")
	}
	if root == "" {
		return nil, errors.New("devfile: empty root directory")
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return nil, errors.New("devfile: root directory is not accessible")
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return nil, errors.New("devfile: root must be a directory (not a symlink)")
	}
	if uid, ok := owner(fi); !ok || uid != euid {
		return nil, errors.New("devfile: root directory must be owned by the process user")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("devfile: root directory must have no group or other permission bits (0700)")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("devfile: cannot open root directory")
	}
	return &Store{root: r, owner: owner, euid: euid}, nil
}

// Scheme implements secretstore.Store.
func (s *Store) Scheme() string { return secretstore.SchemeDevFile }

// Close releases the root handle.
func (s *Store) Close() error { return s.root.Close() }

// relPath maps a devfile ref to its root-relative file path. The ref path
// must be exactly provider-creds/<tenant uuid>/<domain>/<provider>/<name>.
func relPath(ref secretstore.Ref) (string, bool) {
	if ref.Scheme() != secretstore.SchemeDevFile || ref.Version() == "" {
		return "", false
	}
	segs := strings.Split(ref.Path(), "/")
	if len(segs) != 5 || segs[0] != "provider-creds" {
		return "", false
	}
	if _, err := uuid.Parse(segs[1]); err != nil || segs[1] != strings.ToLower(segs[1]) {
		return "", false
	}
	if !domainSegment.MatchString(segs[2]) || !providerSegment.MatchString(segs[3]) || !nameSegment.MatchString(segs[4]) {
		return "", false
	}
	return strings.Join(segs[:4], "/") + "/" + segs[4] + ".v" + ref.Version(), true
}

// Get implements secretstore.Store.
func (s *Store) Get(ctx context.Context, ref secretstore.Ref) (secretstore.Secret, error) {
	if err := ctx.Err(); err != nil {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassUnavailable)
	}
	rel, ok := relPath(ref)
	if !ok {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassInvalidVersion)
	}
	fi, err := s.root.Lstat(rel)
	if err != nil {
		return secretstore.Secret{}, classifyOpenError(err)
	}
	if err := s.checkFile(fi); err != nil {
		return secretstore.Secret{}, err
	}
	f, err := s.root.OpenFile(rel, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return secretstore.Secret{}, classifyOpenError(err)
	}
	defer func() { _ = f.Close() }()
	// Re-check the opened file itself (closes the Lstat/Open race).
	ofi, err := f.Stat()
	if err != nil {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassStoreConfig)
	}
	if err := s.checkFile(ofi); err != nil {
		return secretstore.Secret{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil || len(b) == 0 || len(b) > MaxFileBytes {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassStoreConfig)
	}
	secret := secretstore.NewSecret(b)
	for i := range b {
		b[i] = 0
	}
	return secret, nil
}

// checkFile enforces the per-fetch file rules. Every violation is the
// closed class store_config.
func (s *Store) checkFile(fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() || fi.Mode()&fs.ModeSymlink != 0 {
		return secretstore.NewError(secretstore.ClassStoreConfig)
	}
	if uid, ok := s.owner(fi); !ok || uid != s.euid {
		return secretstore.NewError(secretstore.ClassStoreConfig)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return secretstore.NewError(secretstore.ClassStoreConfig)
	}
	if fi.Size() < 1 || fi.Size() > MaxFileBytes {
		return secretstore.NewError(secretstore.ClassStoreConfig)
	}
	return nil
}

func classifyOpenError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return secretstore.NewError(secretstore.ClassNotFound)
	case errors.Is(err, fs.ErrPermission):
		return secretstore.NewError(secretstore.ClassAccessDenied)
	default:
		// Escape attempts, symlink loops (O_NOFOLLOW) and anything else.
		return secretstore.NewError(secretstore.ClassStoreConfig)
	}
}
