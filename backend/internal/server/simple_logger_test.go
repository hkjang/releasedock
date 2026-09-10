package server

import (
	"context"
	"testing"
)

// storedLogLines returns the run's stored output in the order a reader sees it.
func storedLogLines(t *testing.T, s *Server, runID string) [][2]string {
	t.Helper()
	rows, err := s.store.Pool.Query(t.Context(),
		`SELECT stream,payload FROM simple_run_logs WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		t.Fatalf("read the stored log: %v", err)
	}
	defer rows.Close()
	lines := [][2]string{}
	for rows.Next() {
		var stream string
		var payload []byte
		if err := rows.Scan(&stream, &payload); err != nil {
			t.Fatalf("scan a stored line: %v", err)
		}
		lines = append(lines, [2]string{stream, string(payload)})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored log: %v", err)
	}
	return lines
}

func storedLogBytes(t *testing.T, s *Server, runID string) int {
	t.Helper()
	var bytes int
	if err := s.store.Pool.QueryRow(t.Context(),
		`SELECT log_bytes FROM simple_runs WHERE id=$1`, runID).Scan(&bytes); err != nil {
		t.Fatalf("read log_bytes: %v", err)
	}
	return bytes
}

// A deployment script separates its steps with blank lines, and the stored log
// is what an operator reads afterwards to decide whether the deployment ran. A
// log that closes those lines up no longer matches the output it recorded, so
// the reader has to rebuild the step boundaries from memory.
func TestSimpleRunLoggerKeepsBlankLines(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000001"
	seedSimpleRun(t, s, targetID, runID, "", "RUNNING", true)
	logs := &simpleRunLogger{server: s, ctx: context.Background(), runID: runID, budget: newLogBudget()}

	logs.write("stdout", []byte("이미지 로드\n\n서비스 재시작\n"))
	logs.flush()

	want := [][2]string{{"stdout", "이미지 로드"}, {"stdout", ""}, {"stdout", "서비스 재시작"}}
	got := storedLogLines(t, s, runID)
	if len(got) != len(want) {
		t.Fatalf("stored %d lines, want %d: %q", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("line %d = %q, want %q", index, got[index], want[index])
		}
	}
	// The blank line stores no bytes, so the counter that reports how much of
	// the cap a run used must not claim it did.
	if stored := storedLogBytes(t, s, runID); stored != len("이미지 로드")+len("서비스 재시작") {
		t.Fatalf("log_bytes = %d, want the bytes actually stored", stored)
	}
}

// Output that ends without a trailing newline is still one line, and an ending
// that is exactly a newline must not add an empty line after it.
func TestSimpleRunLoggerDoesNotInventATrailingBlankLine(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000002"
	seedSimpleRun(t, s, targetID, runID, "", "RUNNING", true)
	logs := &simpleRunLogger{server: s, ctx: context.Background(), runID: runID, budget: newLogBudget()}

	logs.write("stdout", []byte("완료\n"))
	logs.flush()

	got := storedLogLines(t, s, runID)
	if len(got) != 1 || got[0] != [2]string{"stdout", "완료"} {
		t.Fatalf("stored %q, want the one line the command printed", got)
	}
}

// The notice that the cap was reached is a stored line like any other: it is
// charged to the reserve the server's own lines draw on, and counted in the
// run's log_bytes. Left outside that accounting it is the only row in the log
// nothing accounts for.
func TestSimpleRunLoggerChargesTheCapNotice(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	const runID = "cc000000-0000-4000-8000-000000000003"
	seedSimpleRun(t, s, targetID, runID, "", "RUNNING", true)
	logs := &simpleRunLogger{server: s, ctx: context.Background(), runID: runID,
		budget: logBudget{command: 4, system: maxSimpleRunSystemLogBytes}}

	logs.write("stdout", []byte("abcdefg\n"))
	logs.flush()

	want := [][2]string{{"stdout", "abcd"}, {streamSystem, logCapReachedNotice}}
	got := storedLogLines(t, s, runID)
	if len(got) != len(want) {
		t.Fatalf("stored %d lines, want %d: %q", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("line %d = %q, want %q", index, got[index], want[index])
		}
	}
	if remaining := logs.budget.system; remaining != maxSimpleRunSystemLogBytes-len(logCapReachedNotice) {
		t.Fatalf("system reserve = %d, want the notice charged to it", remaining)
	}
	if stored := storedLogBytes(t, s, runID); stored != 4+len(logCapReachedNotice) {
		t.Fatalf("log_bytes = %d, want the notice counted with the output", stored)
	}
}
