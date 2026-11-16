package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/backup"
)

func writeEmptyBackup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	m := backup.Manifest{
		Format:        backup.FormatName,
		FormatVersion: backup.FormatVersion,
		CreatedAt:     time.Now().UTC(),
		SnapshotTS:    77,
		ChunkKeys:     backup.DefaultChunkKeys,
		ChunkBytes:    backup.DefaultChunkBytes,
		Ranges:        []backup.Range{{ID: 1, Version: 1}},
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func TestRunVerify(t *testing.T) {
	dir := writeEmptyBackup(t)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"verify", "-in", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("verify exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "verify ok") || !strings.Contains(stdout.String(), "snapshot_ts=77") {
		t.Fatalf("verify output = %q", stdout.String())
	}

	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), []byte("{bad"), 0o644); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{"verify", "-in", dir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("corrupt verify exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid manifest") {
		t.Fatalf("corrupt verify stderr = %q", stderr.String())
	}
}

func TestRunUsageErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"unknown"},
		{"backup"},
		{"restore", "-in", "x"},
		{"verify"},
	}
	for i, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 2 {
			t.Fatalf("case %d args %v: exit = %d, want 2", i, args, code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "kvbackup backup") {
		t.Fatalf("help output = %q", stdout.String())
	}
}
