package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Diansalas/igaming-platform/internal/stagingseed"
)

// writeCreds writes the credentials file with mode 0600 (it must live on the task's tmpfs). The content is never printed: only the
// number of accounts is reported.
func writeCreds(path string, c stagingseed.Creds, out io.Writer) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("credentials file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "SEEDWORLD|credentials_written|%d account(s) to the 0600 credentials file (not printed)\n", len(c.Actors))
	return nil
}

// readSecretFile reads a credentials file, refusing one that is readable by group or others.
func readSecretFile(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("credentials file: %w", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("credentials file %s must be a regular file with mode 0600 or 0400", path)
	}
	return os.ReadFile(path)
}
