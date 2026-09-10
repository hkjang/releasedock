package server

import (
	"errors"
	"strings"
	"testing"
)

// The command's output and the server's progress lines must not compete for the
// same bytes. A deployment script that prints an image load or a build log can
// reach the cap on its own, and if that also silenced the server's lines the
// stored log would end mid-output with no "exit=" line and no record of whether
// replication and the application deployment ran.
func TestLogBudgetKeepsAReserveForServerLines(t *testing.T) {
	budget := newLogBudget()
	allowed, store, exhausted := budget.take("stdout", maxSimpleRunLogBytes)
	if allowed != maxSimpleRunLogBytes || !store || !exhausted {
		t.Fatalf("the command must be able to use its whole budget, got %d store=%v exhausted=%v", allowed, store, exhausted)
	}
	if allowed, store, _ := budget.take("stderr", 16); allowed != 0 || store {
		t.Fatalf("command output past the cap must be dropped, got %d store=%v", allowed, store)
	}
	if allowed, store, _ := budget.take(streamSystem, 64); allowed != 64 || !store {
		t.Fatalf("a server line must still be stored after the command filled the cap, got %d store=%v", allowed, store)
	}
}

// The notice that later output is dropped is written once, on the payload that
// uses up the last of the command budget, so a long run does not repeat it on
// every line it discards afterwards.
func TestLogBudgetReportsExhaustionOnce(t *testing.T) {
	budget := logBudget{command: 10, system: 10}
	allowed, store, exhausted := budget.take("stdout", 4)
	if allowed != 4 || !store || exhausted {
		t.Fatalf("a payload within the budget must not report exhaustion, got %d %v %v", allowed, store, exhausted)
	}
	// The payload that crosses the cap is stored up to the cap and reports it.
	allowed, store, exhausted = budget.take("stdout", 9)
	if allowed != 6 || !store || !exhausted {
		t.Fatalf("the crossing payload must be truncated and report exhaustion, got %d %v %v", allowed, store, exhausted)
	}
	if allowed, store, exhausted = budget.take("stdout", 3); allowed != 0 || store || exhausted {
		t.Fatalf("later output must be dropped silently, got %d %v %v", allowed, store, exhausted)
	}
}

// The reserve is a bound too: a stage that logged pathologically much cannot
// grow the run log without limit, and filling it never claims the command cap
// was reached.
func TestLogBudgetBoundsTheReserveWithoutReportingExhaustion(t *testing.T) {
	budget := logBudget{command: 10, system: 5}
	allowed, store, exhausted := budget.take(streamSystem, 12)
	if allowed != 5 || !store || exhausted {
		t.Fatalf("a server line must be truncated to the reserve, got %d %v %v", allowed, store, exhausted)
	}
	if allowed, store, _ := budget.take(streamSystem, 1); allowed != 0 || store {
		t.Fatalf("the reserve must not be exceeded, got %d %v", allowed, store)
	}
	if allowed, store, _ := budget.take("stdout", 10); allowed != 10 || !store {
		t.Fatal("a full reserve must not consume the command budget")
	}
}

// A blank line is output: deployment scripts use them to separate the sections
// an operator reads the log by, so one has to survive as a row of its own
// rather than be closed up against the line before it.
func TestLogBudgetStoresBlankLines(t *testing.T) {
	budget := logBudget{command: 10, system: 4}
	allowed, store, exhausted := budget.take("stdout", 0)
	if allowed != 0 || !store || exhausted {
		t.Fatalf("a blank line must be stored, got %d %v %v", allowed, store, exhausted)
	}
	if budget.command != 9 {
		t.Fatalf("a blank line must be charged, command = %d", budget.command)
	}
}

// Charging blank lines is what bounds them: a command that prints nothing but
// newlines runs into the same cap as one that prints text, instead of adding
// rows for as long as it runs.
func TestLogBudgetBoundsBlankLines(t *testing.T) {
	budget := logBudget{command: 2, system: 4}
	for range 2 {
		if _, store, _ := budget.take("stdout", 0); !store {
			t.Fatal("a blank line within the budget must be stored")
		}
	}
	if _, store, _ := budget.take("stdout", 0); store {
		t.Fatal("blank lines past the cap must be dropped like any other output")
	}
}

// Nothing is stored once the budget is gone, blank line or not, and reaching
// that point is reported only once.
func TestLogBudgetDropsEverythingOnceExhausted(t *testing.T) {
	budget := logBudget{command: 0, system: 4}
	if allowed, store, exhausted := budget.take("stdout", 0); allowed != 0 || store || exhausted {
		t.Fatalf("an exhausted budget must store nothing, got %d %v %v", allowed, store, exhausted)
	}
	if budget.system != 4 {
		t.Fatalf("a dropped command line must not charge the reserve, system = %d", budget.system)
	}
}

// fakeLogRows stands in for pgx.Rows so the download can be rendered - and made
// to fail part way through - without a database.
type fakeLogRows struct {
	lines   [][2]string
	index   int
	scanAt  int
	scanErr error
	err     error
}

func (r *fakeLogRows) Next() bool {
	r.index++
	return r.index <= len(r.lines)
}

func (r *fakeLogRows) Scan(dest ...any) error {
	if r.scanErr != nil && r.index == r.scanAt {
		return r.scanErr
	}
	line := r.lines[r.index-1]
	*(dest[0].(*string)) = line[0]
	*(dest[1].(*[]byte)) = []byte(line[1])
	return nil
}

func (r *fakeLogRows) Err() error { return r.err }

// The download is the plain text of the run: command output as it was written,
// stderr marked so the reader can tell the two apart.
func TestSimpleRunLogDownloadRendersEveryStream(t *testing.T) {
	var out strings.Builder
	writeSimpleRunLog(&out, &fakeLogRows{lines: [][2]string{
		{"stdout", "이미지를 불러옵니다"},
		{"stderr", "경고"},
		{streamSystem, "exit=0"},
	}})
	want := "이미지를 불러옵니다\n[stderr] 경고\nexit=0\n"
	if out.String() != want {
		t.Fatalf("unexpected download body %q", out.String())
	}
	if strings.Contains(out.String(), simpleRunLogTruncatedNotice) {
		t.Fatal("a log that was read to the end must not be marked as truncated")
	}
}

// A row that cannot be read stops the download, but 200 and the headers are
// already sent, so the only way to tell the operator is a line in the file. A
// silent return would save a log that ends mid-run and looks complete.
func TestSimpleRunLogDownloadMarksARowThatCouldNotBeRead(t *testing.T) {
	var out strings.Builder
	writeSimpleRunLog(&out, &fakeLogRows{
		lines:   [][2]string{{"stdout", "첫 줄"}, {"stdout", "둘째 줄"}},
		scanAt:  2,
		scanErr: errors.New("connection reset"),
	})
	if !strings.HasPrefix(out.String(), "첫 줄\n") {
		t.Fatalf("the lines read before the failure must be kept, got %q", out.String())
	}
	if !strings.Contains(out.String(), simpleRunLogTruncatedNotice+"connection reset") {
		t.Fatalf("the download must say it is truncated, got %q", out.String())
	}
}

// The result set can also end early without any row failing; only Err tells
// that apart from a log that simply ended.
func TestSimpleRunLogDownloadMarksAResultSetThatEndedEarly(t *testing.T) {
	var out strings.Builder
	writeSimpleRunLog(&out, &fakeLogRows{
		lines: [][2]string{{"stdout", "첫 줄"}},
		err:   errors.New("unexpected EOF"),
	})
	if !strings.Contains(out.String(), simpleRunLogTruncatedNotice+"unexpected EOF") {
		t.Fatalf("the download must say it is truncated, got %q", out.String())
	}
}

// The saved file names the package and how the run ended, because an operator
// working through a failed upload downloads several logs and could not tell
// them apart by run id alone.
func TestRunLogDownloadNameCarriesThePackageAndOutcome(t *testing.T) {
	name := runLogDownloadName("payments-api-1.4.2.tar.gz", "FAILED", "0f8b2a11-4c3d-4a1e-9b77-2f1d5c8e6a90", false)
	if name != "releasedock-payments-api-1.4.2.tar.gz-FAILED-0f8b2a11-4c3d-4a1e-9b77-2f1d5c8e6a90.log" {
		t.Fatalf("unexpected download name %q", name)
	}
}

// The name goes into a response header, so nothing the uploader chose may end
// up as a quote, a newline or a path of its own.
func TestRunLogDownloadNameNeverEscapesTheHeader(t *testing.T) {
	disposition := runLogDisposition("\"; rm -rf /\r\n../../etc/passwd", "SUCCESS", "0f8b2a11")
	if strings.ContainsAny(disposition, "\r\n") {
		t.Fatalf("the header must not carry a line break: %q", disposition)
	}
	if strings.Count(disposition, `"`) != 2 || strings.Contains(disposition, "/") {
		t.Fatalf("the file name must not carry a quote or a path separator: %q", disposition)
	}
	// An empty run id would leave a name that ends in a dash, so it falls back.
	if name := runLogDownloadName("", "", "", false); name != "releasedock-run.log" {
		t.Fatalf("unexpected fallback name %q", name)
	}
}

// Package names here are often Korean. Reducing them to ASCII would leave
// nothing to recognise, so the exact name travels in the extended form while
// the plain one stays conservative.
func TestRunLogDispositionKeepsAKoreanPackageNameInTheExtendedForm(t *testing.T) {
	disposition := runLogDisposition("결제-1.4.2.tar", "SUCCESS", "0f8b2a11")
	if !strings.Contains(disposition, `filename="releasedock-1.4.2.tar-SUCCESS-0f8b2a11.log"`) {
		t.Fatalf("unexpected plain name in %q", disposition)
	}
	encoded := encodeExtendedHeaderValue("releasedock-결제-1.4.2.tar-SUCCESS-0f8b2a11.log")
	if !strings.HasSuffix(disposition, "filename*=UTF-8''"+encoded) {
		t.Fatalf("unexpected extended name in %q", disposition)
	}
	if strings.ContainsAny(encoded, "결제") {
		t.Fatalf("the extended form must be percent-encoded, got %q", encoded)
	}
}

// A package name long enough to stretch the header is cut, and the cut never
// leaves a dangling separator.
func TestRunLogDownloadNameBoundsThePackageName(t *testing.T) {
	name := runLogDownloadName(strings.Repeat("a", 200)+"...", "SUCCESS", "0f8b2a11", false)
	if len(name) > 128 {
		t.Fatalf("the name must stay bounded, got %d chars: %q", len(name), name)
	}
	if strings.Contains(name, "--") || strings.Contains(name, "-.log") {
		t.Fatalf("the cut must not leave a dangling separator: %q", name)
	}
}
