package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagingServer() *Server {
	return &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}
	return string(content)
}

// entries reports what is left in the upload directory, which is how a
// discarded upload proves it took nothing with it and left nothing behind.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("could not list %s: %v", dir, err)
	}
	names := []string{}
	for _, item := range items {
		names = append(names, item.Name())
	}
	return names
}

// A run that is still executing was handed the package at its own path, so an
// upload the server has not accepted yet must not be sitting there. Staging is
// what makes the rejection paths - a target that already has a run in flight,
// an identifier that could not be allocated - harmless to that command.
func TestStagedArtifactLeavesTheExistingPackageAlone(t *testing.T) {
	dir := t.TempDir()
	deployed := filepath.Join(dir, "app.tar.gz")
	if err := os.WriteFile(deployed, []byte("the package a run is using"), 0o640); err != nil {
		t.Fatalf("could not seed the upload directory: %v", err)
	}

	server := stagingServer()
	staged, err := server.stageSimpleArtifact(dir, "app.tar.gz", strings.NewReader("a newer package"), 1<<20)
	if err != nil {
		t.Fatalf("stageSimpleArtifact: %v", err)
	}
	if got := readFile(t, deployed); got != "the package a run is using" {
		t.Fatalf("staging replaced the package in place: %q", got)
	}
	if staged.path != deployed {
		t.Fatalf("staged path = %q, want %q", staged.path, deployed)
	}
	if staged.size != int64(len("a newer package")) {
		t.Fatalf("staged size = %d, want %d", staged.size, len("a newer package"))
	}
	sum := sha256.Sum256([]byte("a newer package"))
	if staged.checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("staged checksum = %q, want %q", staged.checksum, hex.EncodeToString(sum[:]))
	}

	staged.discard()
	if got := readFile(t, deployed); got != "the package a run is using" {
		t.Fatalf("discarding removed the package in place: %q", got)
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != "app.tar.gz" {
		t.Fatalf("a discarded upload must leave nothing behind, directory holds %v", names)
	}
}

// Committing is still the ordinary redeploy: the package takes its own name and
// replaces whatever was there.
func TestStagedArtifactCommitPublishesThePackage(t *testing.T) {
	dir := t.TempDir()
	deployed := filepath.Join(dir, "app.tar.gz")
	if err := os.WriteFile(deployed, []byte("previous"), 0o640); err != nil {
		t.Fatalf("could not seed the upload directory: %v", err)
	}

	server := stagingServer()
	staged, err := server.stageSimpleArtifact(dir, "app.tar.gz", strings.NewReader("current"), 1<<20)
	if err != nil {
		t.Fatalf("stageSimpleArtifact: %v", err)
	}
	if err := staged.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := readFile(t, deployed); got != "current" {
		t.Fatalf("committed package = %q, want %q", got, "current")
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != "app.tar.gz" {
		t.Fatalf("a committed upload must leave no staging file, directory holds %v", names)
	}
	// The caller's cleanup runs on every path it did not take, and after a
	// commit it must not delete the package it just published.
	staged.discard()
	if got := readFile(t, deployed); got != "current" {
		t.Fatalf("discard after commit removed the package: %q", got)
	}
}

// An upload rejected for its size is written before the size is known, so the
// bytes it did write must not stay in the directory either.
func TestStageSimpleArtifactCleansUpAnOversizePackage(t *testing.T) {
	dir := t.TempDir()
	server := stagingServer()
	if _, err := server.stageSimpleArtifact(dir, "app.tar.gz", strings.NewReader("too many bytes"), 4); err == nil {
		t.Fatal("an oversize package must be rejected")
	}
	if names := entries(t, dir); len(names) != 0 {
		t.Fatalf("a rejected upload must leave nothing behind, directory holds %v", names)
	}
}
