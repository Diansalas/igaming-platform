package devfile

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

func devConfig() config.Config {
	return config.Config{Environment: "development", EnvironmentExplicit: true}
}

func privateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// write creates <root>/provider-creds/<tenant>/<domain>/<provider>/<name>.v<version>
// with mode and content, returning the matching ref.
func write(t *testing.T, root string, tenant uuid.UUID, name, version string, mode os.FileMode, content []byte) secretstore.Ref {
	t.Helper()
	dir := filepath.Join(root, "provider-creds", tenant.String(), "casino", "acme")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".v"+version)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return mustRef(t, fmt.Sprintf("devfile://provider-creds/%s/casino/acme/%s?version=%s", tenant, name, version))
}

func mustRef(t *testing.T, raw string) secretstore.Ref {
	t.Helper()
	ref, err := secretstore.ParseRef(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return ref
}

func randSecret(t *testing.T) []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDevFile_RefusedUnlessExplicitDevelopment(t *testing.T) {
	root := privateRoot(t)
	for name, cfg := range map[string]config.Config{
		"APP_ENV absent": {Environment: "development", EnvironmentExplicit: false},
		"staging":        {Environment: "staging", EnvironmentExplicit: true},
		"production":     {Environment: "production", EnvironmentExplicit: true},
	} {
		if _, err := New(cfg, root); err == nil {
			t.Fatalf("%s: the devfile backend must be refused", name)
		}
	}
	s, err := New(devConfig(), root)
	if err != nil {
		t.Fatalf("explicit development must be accepted: %v", err)
	}
	_ = s.Close()
}

func TestDevFile_RootPermissions(t *testing.T) {
	euid := platformEUID()
	t.Run("group/other bits refused", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := New(devConfig(), root); err == nil {
			t.Fatal("a 0750 root must be refused")
		}
	})
	t.Run("wrong owner refused", func(t *testing.T) {
		if _, err := newStore(privateRoot(t), platformOwner, euid+1); err == nil {
			t.Fatal("a root not owned by the euid must be refused")
		}
	})
	t.Run("not a directory refused", func(t *testing.T) {
		file := filepath.Join(privateRoot(t), "f")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(devConfig(), file); err == nil {
			t.Fatal("a file root must be refused")
		}
	})
	t.Run("symlink root refused", func(t *testing.T) {
		base := privateRoot(t)
		link := filepath.Join(base, "link")
		if err := os.Symlink(privateRoot(t), link); err != nil {
			t.Fatal(err)
		}
		if _, err := New(devConfig(), link); err == nil {
			t.Fatal("a symlink root must be refused")
		}
	})
	t.Run("missing root refused", func(t *testing.T) {
		if _, err := New(devConfig(), filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("a missing root must be refused")
		}
	})
}

func TestDevFile_FilePermissions(t *testing.T) {
	root := privateRoot(t)
	s, err := New(devConfig(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	tenant := uuid.New()
	ctx := context.Background()

	secret := randSecret(t)
	good := write(t, root, tenant, "good", "1", 0o600, secret)
	got, err := s.Get(ctx, good)
	if err != nil || !bytes.Equal(got.Bytes(), secret) {
		t.Fatalf("a 0600 file must be read exactly: %v", err)
	}
	ro := write(t, root, tenant, "readonly", "1", 0o400, secret)
	if _, err := s.Get(ctx, ro); err != nil {
		t.Fatalf("a 0400 file must be read: %v", err)
	}
	// Exact bytes, no trimming.
	ws := append(randSecret(t), '\n', ' ')
	wsRef := write(t, root, tenant, "whitespace", "1", 0o600, ws)
	if got, err := s.Get(ctx, wsRef); err != nil || !bytes.Equal(got.Bytes(), ws) {
		t.Fatal("the secret must be the exact file bytes (no trimming)")
	}

	storeConfig := func(name string, ref secretstore.Ref) {
		t.Helper()
		if _, err := s.Get(ctx, ref); secretstore.ClassOf(err) != secretstore.ClassStoreConfig {
			t.Fatalf("%s: want store_config, got %v", name, err)
		}
	}
	storeConfig("group-readable", write(t, root, tenant, "group", "1", 0o640, secret))
	storeConfig("world-readable", write(t, root, tenant, "world", "1", 0o604, secret))
	storeConfig("empty", write(t, root, tenant, "empty", "1", 0o600, nil))
	storeConfig("oversize", write(t, root, tenant, "big", "1", 0o600, bytes.Repeat([]byte{'a'}, MaxFileBytes+1)))

	// Directory in place of the file.
	dir := filepath.Join(root, "provider-creds", tenant.String(), "casino", "acme", "adir.v1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	storeConfig("directory", mustRef(t, fmt.Sprintf("devfile://provider-creds/%s/casino/acme/adir?version=1", tenant)))

	// Symlink to a valid file inside the root.
	link := filepath.Join(root, "provider-creds", tenant.String(), "casino", "acme", "link.v1")
	if err := os.Symlink(filepath.Join(root, "provider-creds", tenant.String(), "casino", "acme", "good.v1"), link); err != nil {
		t.Fatal(err)
	}
	storeConfig("symlink", mustRef(t, fmt.Sprintf("devfile://provider-creds/%s/casino/acme/link?version=1", tenant)))

	// Wrong owner (injectable owner check).
	wrongOwner := &Store{root: s.root, owner: platformOwner, euid: platformEUID() + 1}
	if _, err := wrongOwner.Get(ctx, good); secretstore.ClassOf(err) != secretstore.ClassStoreConfig {
		t.Fatalf("wrong owner: want store_config, got %v", err)
	}
	noOwner := &Store{root: s.root, owner: func(fs.FileInfo) (uint32, bool) { return 0, false }, euid: platformEUID()}
	if _, err := noOwner.Get(ctx, good); secretstore.ClassOf(err) != secretstore.ClassStoreConfig {
		t.Fatalf("unknown owner: want store_config, got %v", err)
	}

	if _, err := s.Get(ctx, mustRef(t, fmt.Sprintf("devfile://provider-creds/%s/casino/acme/absent?version=1", tenant))); secretstore.ClassOf(err) != secretstore.ClassNotFound {
		t.Fatalf("absent: want not_found, got %v", err)
	}
}

func TestDevFile_TraversalRefused(t *testing.T) {
	base := privateRoot(t)
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := New(devConfig(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	tenant := uuid.New()
	// A secret OUTSIDE the root, and a symlinked directory inside the root
	// pointing at it.
	outside := filepath.Join(base, "outside", "provider-creds", tenant.String(), "casino", "acme")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "n.v1"), randSecret(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside", "provider-creds"), filepath.Join(root, "provider-creds")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Get(ctx, mustRef(t, fmt.Sprintf("devfile://provider-creds/%s/casino/acme/n?version=1", tenant))); err == nil {
		t.Fatal("a symlink escaping the root must be refused")
	}
	for _, raw := range []string{
		fmt.Sprintf("devfile://provider-creds/%s/casino/acme/..?version=1", tenant),
		fmt.Sprintf("devfile://provider-creds/../casino/acme/n?version=1"),
		fmt.Sprintf("devfile://x/provider-creds/%s/casino/acme/n?version=1", tenant),
		fmt.Sprintf("devfile://provider-creds/%s/casino/acme/a/n?version=1", tenant),
		fmt.Sprintf("devfile://provider-creds/%s/files/acme/n?version=1", tenant),
		fmt.Sprintf("devfile://provider-creds/%s/casino/acme/n?version=../../x", tenant),
	} {
		ref, err := secretstore.ParseRef(raw)
		if err != nil {
			continue // refused at parse
		}
		if _, err := s.Get(ctx, ref); err == nil {
			t.Fatalf("%q must be refused", raw)
		}
	}
}

// TestDevFile_RootIgnoredByGitAndDocker: the default devfile root never
// reaches a commit or a container image.
func TestDevFile_RootIgnoredByGitAndDocker(t *testing.T) {
	for _, file := range []string{"../../../.gitignore", "../../../.dockerignore"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		found := false
		for _, line := range strings.Split(string(raw), "\n") {
			if l := strings.TrimSpace(line); l == ".secrets/" || l == ".secrets" || l == "/.secrets/" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s must ignore .secrets/", file)
		}
	}
}
