package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/webassets"
)

// Proves a single executable serves the portal with no companion directory:
// New is given no web root, and the SPA still answers a deep link.
func TestEmbeddedPortalServesWithoutADiskRoot(t *testing.T) {
	if webassets.FS() == nil {
		t.Skip("built without the embedded portal")
	}
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, "")
	if s.WebOrigin() != "embedded" {
		t.Fatalf("expected the embedded portal to be selected, got %q", s.WebOrigin())
	}
	request := httptest.NewRequest(http.MethodGet, "/admin/roles", nil)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("deep link status=%d", recorder.Code)
	}
	if recorder.Header().Get("Location") != "" {
		t.Fatalf("deep link redirected to %q", recorder.Header().Get("Location"))
	}
	if !strings.Contains(recorder.Body.String(), "<div id=\"root\"") && !strings.Contains(recorder.Body.String(), "/assets/") {
		t.Fatalf("did not serve the built index.html: %q", recorder.Body.String())
	}
}

// An explicit disk root must still win, which is what makes the override
// usable for patching assets without a rebuild.
func TestDiskRootOverridesEmbeddedPortal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<!doctype html><title>Override</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, root)
	if s.WebOrigin() != root {
		t.Fatalf("expected the disk root to win, got %q", s.WebOrigin())
	}
	request := httptest.NewRequest(http.MethodGet, "/anything", nil)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), "Override") {
		t.Fatalf("disk override was not served: %q", recorder.Body.String())
	}
}
