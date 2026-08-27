package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
