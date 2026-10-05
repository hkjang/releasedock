package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hkjang/releasedock/backend/internal/secure"
)

// The route has to be registered on the real mux next to the stream it sits
// beside, and it has to be behind authentication: an unregistered path falls
// through to the catch-all, which answers a 404 that would look like a release
// that does not exist. Building the handler also proves the two patterns do not
// collide, because a conflicting registration panics here.
func TestReleaseLogRoutesAreRegisteredAndGuarded(t *testing.T) {
	vault, _ := secure.NewVault([]byte("0123456789abcdef0123456789abcdef"))
	s := New(nil, vault, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{}, "")
	handler := s.Handler()
	for _, path := range []string{
		"/api/v1/releases/0f8b2a11/logs",
		"/api/v1/releases/0f8b2a11/logs?format=text",
		"/api/v1/releases/0f8b2a11/logs/stream",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "API route not found") {
			t.Fatalf("%s reached the catch-all instead of a release handler", path)
		}
	}
}

// The full-mode release log download is the plain text of the deployment:
// command output as the runner wrote it, stderr marked so the reader can tell
// the two apart. Until this existed the only way to read a release log was the
// SSE stream, which cannot reach a line the display limit already dropped.
func TestReleaseLogDownloadRendersEveryStream(t *testing.T) {
	rec := httptest.NewRecorder()
	writeReleaseLogDownload(rec, &fakeLogRows{lines: [][2]string{
		{"stdout", "이미지를 불러옵니다"},
		{"stderr", "경고"},
		{streamSystem, "exit=0"},
	}}, "payments-api-1.4.2", "FAILED", "0f8b2a11")
	if rec.Code != 200 {
		t.Fatalf("the download must answer 200, got %d", rec.Code)
	}
	want := "이미지를 불러옵니다\n[stderr] 경고\nexit=0\n"
	if rec.Body.String() != want {
		t.Fatalf("unexpected download body %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), simpleRunLogTruncatedNotice) {
		t.Fatal("a log that was read to the end must not be marked as truncated")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("unexpected content type %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("the download must not be sniffed, got %q", got)
	}
}

// A row that cannot be read stops the download, but 200 and the headers are
// already sent, so the only way to tell the operator is a line in the file. A
// release log is what an audit reads to decide whether a deployment ran, so a
// file that stops mid-run must not look complete.
func TestReleaseLogDownloadMarksARowThatCouldNotBeRead(t *testing.T) {
	rec := httptest.NewRecorder()
	writeReleaseLogDownload(rec, &fakeLogRows{
		lines:   [][2]string{{"stdout", "첫 줄"}, {"stdout", "둘째 줄"}},
		scanAt:  2,
		scanErr: errors.New("connection reset"),
	}, "payments-api-1.4.2", "RUNNING", "0f8b2a11")
	if !strings.HasPrefix(rec.Body.String(), "첫 줄\n") {
		t.Fatalf("the lines read before the failure must be kept, got %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), simpleRunLogTruncatedNotice+"connection reset") {
		t.Fatalf("the download must say it is truncated, got %q", rec.Body.String())
	}
}

// The result set can also end early without any row failing; only Err tells
// that apart from a log that simply ended.
func TestReleaseLogDownloadMarksAResultSetThatEndedEarly(t *testing.T) {
	rec := httptest.NewRecorder()
	writeReleaseLogDownload(rec, &fakeLogRows{
		lines: [][2]string{{"stdout", "첫 줄"}},
		err:   errors.New("unexpected EOF"),
	}, "payments-api-1.4.2", "SUCCESS", "0f8b2a11")
	if !strings.Contains(rec.Body.String(), simpleRunLogTruncatedNotice+"unexpected EOF") {
		t.Fatalf("the download must say it is truncated, got %q", rec.Body.String())
	}
}

// The saved file names the application and the version, not only the release
// id: an operator working through a failed deployment downloads the log of
// several releases of the same application and could not tell the files apart
// by a uuid alone.
func TestReleaseLogDownloadNameCarriesTheReleaseIdentity(t *testing.T) {
	rec := httptest.NewRecorder()
	writeReleaseLogDownload(rec, &fakeLogRows{}, releaseLogLabel("payments-api", "1.4.2"), "FAILED", "0f8b2a11-4c3d-4a1e-9b77-2f1d5c8e6a90")
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, `filename="releasedock-payments-api-1.4.2-FAILED-0f8b2a11-4c3d-4a1e-9b77-2f1d5c8e6a90.log"`) {
		t.Fatalf("unexpected download name in %q", disposition)
	}
}

// An application named in Korean would leave nothing to recognise in the
// conservative ASCII name, so the exact label travels in the extended form -
// the same split the simple mode download already uses.
func TestReleaseLogLabelKeepsAKoreanApplicationNameInTheExtendedForm(t *testing.T) {
	rec := httptest.NewRecorder()
	writeReleaseLogDownload(rec, &fakeLogRows{}, releaseLogLabel("결제", "1.4.2"), "SUCCESS", "0f8b2a11")
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, `filename="releasedock-1.4.2-SUCCESS-0f8b2a11.log"`) {
		t.Fatalf("unexpected plain name in %q", disposition)
	}
	if !strings.HasSuffix(disposition, "filename*=UTF-8''"+encodeExtendedHeaderValue("releasedock-결제-1.4.2-SUCCESS-0f8b2a11.log")) {
		t.Fatalf("unexpected extended name in %q", disposition)
	}
}

// A release can be stored without a version while it is being prepared. The
// label then drops the empty part instead of leaving a dangling separator that
// would read as a missing name.
func TestReleaseLogLabelSkipsAMissingPart(t *testing.T) {
	if got := releaseLogLabel("payments-api", "  "); got != "payments-api" {
		t.Fatalf("a blank version must be dropped, got %q", got)
	}
	if got := releaseLogLabel("", "1.4.2"); got != "1.4.2" {
		t.Fatalf("a blank application name must be dropped, got %q", got)
	}
	if got := releaseLogLabel("", ""); got != "" {
		t.Fatalf("an unidentifiable release must fall back to the id alone, got %q", got)
	}
}
