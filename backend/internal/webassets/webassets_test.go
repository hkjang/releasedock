package webassets

import (
	"io/fs"
	"strings"
	"testing"
)

// The embed directory is empty in a plain development build and populated by
// scripts/build.sh for a release, so this asserts the invariant that holds
// either way: FS() is non-nil exactly when a usable index.html is embedded.
func TestFSReflectsWhetherThePortalIsEmbedded(t *testing.T) {
	root, err := fs.Sub(embedded, "assets")
	if err != nil {
		t.Fatalf("embedded tree is unusable: %v", err)
	}
	_, statErr := fs.Stat(root, "index.html")
	got := FS()
	if statErr == nil && got == nil {
		t.Fatal("index.html is embedded but FS() returned nil")
	}
	if statErr != nil && got != nil {
		t.Fatal("index.html is not embedded but FS() returned a filesystem")
	}
	if got == nil {
		t.Log("built without the portal; the server falls back to assets on disk")
		return
	}

	// A release build must serve a real Vite bundle, not a stray placeholder.
	index, err := fs.ReadFile(got, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(index), "/assets/") && !strings.Contains(string(index), "./assets/") {
		t.Fatalf("index.html does not reference built assets: %q", string(index))
	}
	entries, err := fs.ReadDir(got, "assets")
	if err != nil {
		t.Fatalf("read assets directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("assets directory is embedded but empty")
	}
	t.Logf("portal embedded: index.html plus %d asset files", len(entries))
}

// The placeholder must never be mistaken for a portal.
func TestGitkeepAloneDoesNotCount(t *testing.T) {
	if _, err := fs.Stat(embedded, "assets/.gitkeep"); err != nil {
		t.Skip("placeholder absent in this build")
	}
	if _, err := fs.Stat(embedded, "assets/index.html"); err != nil && FS() != nil {
		t.Fatal("FS() must be nil when only the placeholder is embedded")
	}
}
