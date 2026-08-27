package logstream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var truncationNotice = []byte("\n[releasedock: command output truncated by profile limit]\n")

type Budget struct {
	maximum   int64
	used      atomic.Int64
	truncated atomic.Bool
}

func NewBudget(maximum int64) *Budget { return &Budget{maximum: maximum} }

func (b *Budget) reserve(requested int) int {
	for {
		used := b.used.Load()
		remaining := b.maximum - used
		if remaining <= 0 {
			return 0
		}
		allowed := int64(requested)
		if allowed > remaining {
			allowed = remaining
		}
		if b.used.CompareAndSwap(used, used+allowed) {
			return int(allowed)
		}
	}
}

type Writer struct {
	ctx       context.Context
	repo      Sink
	jobID     string
	stepID    int64
	stream    string
	chunkSize int
	budget    *Budget
	sequence  atomic.Int64
	errMu     sync.Mutex
	err       error
}

type Sink interface {
	AppendLog(context.Context, string, int64, string, int64, []byte) error
}

func NewWriter(ctx context.Context, repo Sink, jobID string, stepID int64, stream string, chunkSize int, budget *Budget) *Writer {
	return &Writer{ctx: ctx, repo: repo, jobID: jobID, stepID: stepID, stream: stream, chunkSize: chunkSize, budget: budget}
}

func (w *Writer) Write(payload []byte) (int, error) {
	originalLength := len(payload)
	allowed := w.budget.reserve(originalLength)
	if allowed > 0 {
		w.appendChunks(payload[:allowed])
	}
	if allowed < originalLength && w.budget.truncated.CompareAndSwap(false, true) {
		w.appendChunks(truncationNotice)
	}
	// Always report the complete input as consumed. Returning io.ErrShortWrite
	// would make os/exec abort the process instead of applying the configured
	// audit-log cap.
	return originalLength, nil
}

func (w *Writer) appendChunks(payload []byte) {
	for len(payload) > 0 {
		size := w.chunkSize
		if size > len(payload) {
			size = len(payload)
		}
		chunk := append([]byte(nil), payload[:size]...)
		sequence := w.sequence.Add(1)
		if err := w.repo.AppendLog(w.ctx, w.jobID, w.stepID, w.stream, sequence, chunk); err != nil {
			w.errMu.Lock()
			w.err = errors.Join(w.err, err)
			w.errMu.Unlock()
		}
		payload = payload[size:]
	}
}

func (w *Writer) Err() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}
