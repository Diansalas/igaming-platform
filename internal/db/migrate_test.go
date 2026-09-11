package db

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
}

func TestLoadMigrations_PairsUpAndDownFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "0002_second.up.sql", "-- up 2")
	writeFile(t, dir, "0002_second.down.sql", "-- down 2")
	writeFile(t, dir, "0001_first.up.sql", "-- up 1")
	writeFile(t, dir, "0001_first.down.sql", "-- down 1")
	// A non-matching file should be ignored rather than breaking parsing.
	writeFile(t, dir, "README.md", "not a migration")

	migrations, err := LoadMigrations(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(migrations) != 2 {
		t.Fatalf("expected 2 migrations, got %d", len(migrations))
	}
	// Must be sorted ascending by version regardless of file creation/read order.
	if migrations[0].Version != 1 || migrations[1].Version != 2 {
		t.Errorf("expected versions [1, 2] in order, got [%d, %d]", migrations[0].Version, migrations[1].Version)
	}
	if migrations[0].Description != "first" || migrations[1].Description != "second" {
		t.Errorf("unexpected descriptions: %+v", migrations)
	}
}

func TestLoadMigrations_MissingDownFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "0001_first.up.sql", "-- up only, no down")

	if _, err := LoadMigrations(dir); err == nil {
		t.Fatal("expected an error for a migration with an up file but no down file, got nil")
	}
}

func TestLoadMigrations_MissingUpFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "0001_first.down.sql", "-- down only, no up")

	if _, err := LoadMigrations(dir); err == nil {
		t.Fatal("expected an error for a migration with a down file but no up file, got nil")
	}
}

func TestLoadMigrations_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	migrations, err := LoadMigrations(dir)
	if err != nil {
		t.Fatalf("unexpected error for an empty directory: %v", err)
	}
	if len(migrations) != 0 {
		t.Errorf("expected no migrations, got %d", len(migrations))
	}
}

func TestLoadMigrations_NonexistentDirectory(t *testing.T) {
	if _, err := LoadMigrations(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected an error for a nonexistent directory, got nil")
	}
}

func TestDescribeMigrations_FormatsVersionAndDescription(t *testing.T) {
	migrations := []migration{
		{Version: 1, Description: "create_tenants"},
		{Version: 12, Description: "add_index"},
	}
	got := DescribeMigrations(migrations)
	want := "0001_create_tenants, 0012_add_index"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}
