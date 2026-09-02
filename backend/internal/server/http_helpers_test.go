package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWriteErrorWritesOneJSONDocument(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeError(recorder, http.StatusBadRequest, "invalid_request", "request is invalid")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	decoder := json.NewDecoder(recorder.Body)
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("error response must contain exactly one JSON document, got %v", err)
	}
	errorBody, ok := body["error"].(map[string]any)
	if !ok || errorBody["code"] != "invalid_request" || errorBody["message"] != "request is invalid" {
		t.Fatalf("unexpected error body: %#v", body)
	}
}

func TestReadUploadBatchDefaultsToASingleLastRun(t *testing.T) {
	form := func(values map[string]string) *http.Request {
		body := url.Values{}
		for key, value := range values {
			body.Set(key, value)
		}
		r := httptest.NewRequest(http.MethodPost, "/simple/runs", strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}

	// A request that says nothing about a batch is one package on its own, so
	// the once-per-upload stages must still fire for it.
	batch := readUploadBatch(form(nil))
	if batch.ID != "" || !batch.Last {
		t.Fatalf("expected an unbatched last run, got %+v", batch)
	}

	batch = readUploadBatch(form(map[string]string{"batchId": "abc123", "batchLast": "false"}))
	if batch.ID != "abc123" || batch.Last {
		t.Fatalf("expected a non-final run of batch abc123, got %+v", batch)
	}

	// An identifier that is not an opaque token is dropped rather than stored.
	// Dropping it must not promote a middle run into the one that carries the
	// once-per-upload stages.
	batch = readUploadBatch(form(map[string]string{"batchId": "a/../b", "batchLast": "false"}))
	if batch.ID != "" || batch.Last {
		t.Fatalf("expected the identifier to be dropped but the marker kept, got %+v", batch)
	}
}
