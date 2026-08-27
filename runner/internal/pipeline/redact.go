package pipeline

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

var targetCredentialRedaction = []byte("[REDACTED TARGET CREDENTIAL]")

// exactSecretRedactor retains enough trailing bytes to find a plaintext secret
// split across arbitrary socket/log chunks. It only promises exact-byte
// redaction; approved scripts remain trusted because they can transform or
// transmit credentials required to reach their deployment target.
type exactSecretRedactor struct {
	mu          sync.Mutex
	destination io.Writer
	secret      []byte
	pending     []byte
	err         error
}

func newExactSecretRedactor(destination io.Writer, secret []byte) *exactSecretRedactor {
	return &exactSecretRedactor{destination: destination, secret: append([]byte(nil), secret...)}
}

func (w *exactSecretRedactor) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.pending = append(w.pending, value...)
	if err := w.drain(false); err != nil {
		w.err = err
		return 0, err
	}
	return len(value), nil
}

func (w *exactSecretRedactor) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = w.drain(true)
	}
	clear(w.secret)
	w.secret = nil
	clear(w.pending)
	w.pending = nil
	return w.err
}

func (w *exactSecretRedactor) drain(final bool) error {
	if len(w.pending) == 0 {
		return nil
	}
	if len(w.secret) == 0 {
		return errors.New("secret redactor requires a non-empty pattern")
	}
	limit := len(w.pending) - len(w.secret) + 1
	if final {
		limit = len(w.pending) + 1
	} else if limit <= 0 {
		return nil
	}
	cursor := 0
	for cursor < len(w.pending) {
		relative := bytes.Index(w.pending[cursor:], w.secret)
		if relative < 0 {
			break
		}
		index := cursor + relative
		if !final && index >= limit {
			break
		}
		if err := writeRedactedBytes(w.destination, w.pending[cursor:index]); err != nil {
			return err
		}
		if err := writeRedactedBytes(w.destination, targetCredentialRedaction); err != nil {
			return err
		}
		cursor = index + len(w.secret)
	}
	flushThrough := len(w.pending)
	if !final {
		flushThrough = limit
		if cursor > flushThrough {
			flushThrough = cursor
		}
	}
	if err := writeRedactedBytes(w.destination, w.pending[cursor:flushThrough]); err != nil {
		return err
	}
	w.pending = append(w.pending[:0], w.pending[flushThrough:]...)
	return nil
}

func writeRedactedBytes(destination io.Writer, value []byte) error {
	if destination == nil || len(value) == 0 {
		return nil
	}
	for len(value) > 0 {
		written, err := destination.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
