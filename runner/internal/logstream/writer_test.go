package logstream

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

type memorySink struct {
	mu     sync.Mutex
	chunks [][]byte
}

func (s *memorySink) AppendLog(_ context.Context, _ string, _ int64, _ string, _ int64, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, append([]byte(nil), payload...))
	return nil
}

func (s *memorySink) content() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Join(s.chunks, nil)
}

func TestWriterChunksAndCapsSharedOutput(t *testing.T) {
	sink := &memorySink{}
	budget := NewBudget(6)
	stdout := NewWriter(context.Background(), sink, "job", 1, "stdout", 2, budget)
	stderr := NewWriter(context.Background(), sink, "job", 1, "stderr", 2, budget)
	if written, err := stdout.Write([]byte("abcd")); err != nil || written != 4 {
		t.Fatalf("stdout write = %d, %v", written, err)
	}
	if written, err := stderr.Write([]byte("EFGHIJ")); err != nil || written != 6 {
		t.Fatalf("stderr write = %d, %v", written, err)
	}
	content := sink.content()
	if !bytes.Contains(content, []byte("abcdef")) && !bytes.Contains(content, []byte("abcdEF")) {
		// Writes are sequential in this test; keep the message useful if stream
		// handling changes later.
		t.Fatalf("capped content missing: %q", content)
	}
	if bytes.Count(content, truncationNotice) != 1 {
		t.Fatalf("expected one truncation notice: %q", content)
	}
}
