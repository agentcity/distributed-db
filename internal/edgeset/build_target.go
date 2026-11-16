package edgeset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// buildTarget stages a store in a sibling directory and only publishes it with
// a rename after every segment and the manifest have been written. That keeps a
// failed Build from leaving a half-written store at the requested path.
type buildTarget struct {
	dir     string // final store directory
	staging string // where Build writes
	owns    bool   // true when SegmentDir was empty and dir is our temp dir
}

func newBuildTarget(requested string) (*buildTarget, error) {
	if requested == "" {
		dir, err := os.MkdirTemp("", "edgeset-store-*")
		if err != nil {
			return nil, fmt.Errorf("edgeset: create temporary store: %w", err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("edgeset: chmod temporary store: %w", err)
		}
		return &buildTarget{dir: dir, staging: dir, owns: true}, nil
	}

	clean := filepath.Clean(requested)
	if clean == "." || clean == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: refusing to use %q as a store directory", ErrInvalidOption, requested)
	}

	info, err := os.Lstat(clean)
	switch {
	case err == nil:
		if !info.IsDir() {
			return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalidOption, clean)
		}
		entries, readErr := os.ReadDir(clean)
		if readErr != nil {
			return nil, fmt.Errorf("edgeset: inspect store directory: %w", readErr)
		}
		if len(entries) != 0 {
			return nil, fmt.Errorf("%w: store directory %s is not empty", ErrInvalidOption, clean)
		}
	case errors.Is(err, os.ErrNotExist):
		// Created below.
	default:
		return nil, fmt.Errorf("edgeset: inspect store directory: %w", err)
	}

	parent := filepath.Dir(clean)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("edgeset: create store parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".edgeset-build-*")
	if err != nil {
		return nil, fmt.Errorf("edgeset: create staging directory: %w", err)
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		_ = os.RemoveAll(staging)
		return nil, fmt.Errorf("edgeset: chmod staging directory: %w", err)
	}
	return &buildTarget{dir: clean, staging: staging}, nil
}

func (t *buildTarget) abort() {
	if t == nil {
		return
	}
	if t.owns {
		_ = os.RemoveAll(t.dir)
		return
	}
	_ = os.RemoveAll(t.staging)
}

func (t *buildTarget) commit() error {
	if t.owns {
		return nil
	}

	// Recheck the destination to avoid deleting anything that appeared while
	// the store was being built.
	info, err := os.Lstat(t.dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%w: %s is no longer a directory", ErrInvalidOption, t.dir)
		}
		entries, readErr := os.ReadDir(t.dir)
		if readErr != nil {
			return fmt.Errorf("edgeset: inspect store directory: %w", readErr)
		}
		if len(entries) != 0 {
			return fmt.Errorf("%w: store directory %s is no longer empty", ErrInvalidOption, t.dir)
		}
		if err := os.Remove(t.dir); err != nil {
			return fmt.Errorf("edgeset: replace empty store directory: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		// Fine.
	default:
		return fmt.Errorf("edgeset: inspect store directory: %w", err)
	}

	if err := os.Rename(t.staging, t.dir); err != nil {
		return fmt.Errorf("edgeset: publish store: %w", err)
	}
	return nil
}
