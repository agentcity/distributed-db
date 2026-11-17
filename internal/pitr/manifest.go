package pitr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LoadManifest reads and validates dir/manifest.json.
func LoadManifest(dir string) (*Manifest, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: empty archive directory", ErrManifest)
	}
	p := filepath.Join(dir, ManifestFile)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s does not exist", ErrManifest, p)
		}
		return nil, fmt.Errorf("%w: read %s: %v", ErrManifest, p, err)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrManifest, p, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %s has trailing data", ErrManifest, p)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// loadManifestIfExists is the Writer's strict open path. A missing manifest is
// reported separately so a fresh archive can be initialized.
func loadManifestIfExists(dir string) (*Manifest, bool, error) {
	if dir == "" {
		return nil, false, fmt.Errorf("%w: empty archive directory", ErrManifest)
	}
	p := filepath.Join(dir, ManifestFile)
	_, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%w: stat %s: %v", ErrManifest, p, err)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// writeManifestDir atomically replaces dir/manifest.json: bytes go to a temp
// file in the same directory, are fsynced, renamed over the manifest and the
// directory is fsynced. A crash can therefore leave the old manifest or the
// new one, never a torn mixture.
func writeManifestDir(dir string, m *Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode manifest: %v", ErrManifest, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pitr: create archive directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("pitr: create manifest temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("pitr: chmod manifest temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("pitr: write manifest temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("pitr: sync manifest temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pitr: close manifest temp: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, ManifestFile)); err != nil {
		return fmt.Errorf("pitr: publish manifest: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("pitr: sync archive directory: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
