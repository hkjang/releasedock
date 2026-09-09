package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hkjang/releasedock/backend/internal/store"
)

// countingReader reports how much of a request body has actually been read,
// which is what tells a streamed upload from a buffered one.
type countingReader struct {
	inner io.Reader
	read  int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.read += n
	return n, err
}

// uploadBody builds a multipart request body. The package is written where the
// caller asks for it, because a browser sends it first while another client may
// name the batch first, and both have to work.
func uploadBody(t *testing.T, filename string, artifact []byte, fields map[string]string, artifactFirst bool) (io.Reader, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	writeArtifact := func() {
		if filename == "" {
			return
		}
		part, err := writer.CreateFormFile("artifact", filename)
		if err != nil {
			t.Fatalf("create artifact part: %v", err)
		}
		if _, err := part.Write(artifact); err != nil {
			t.Fatalf("write artifact part: %v", err)
		}
	}
	if artifactFirst {
		writeArtifact()
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write field %s: %v", key, err)
		}
	}
	if !artifactFirst {
		writeArtifact()
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, writer.Boundary()
}

// The whole point of walking the parts by hand: the package is handed to the
// caller before it is read, so it goes straight into the target directory
// instead of being buffered into a temporary file first.
func TestNextSimpleArtifactPartDoesNotBufferThePackage(t *testing.T) {
	artifact := bytes.Repeat([]byte("releasedock"), 400_000) // ~4 MiB
	body, boundary := uploadBody(t, "app-v1.tar.gz", artifact, map[string]string{"batchId": "abc123"}, false)
	counter := &countingReader{inner: body}
	fields := simpleUploadFields{}

	part, err := nextSimpleArtifactPart(multipart.NewReader(counter, boundary), fields)
	if err != nil {
		t.Fatalf("next artifact part: %v", err)
	}
	defer part.Close() //nolint:errcheck
	if counter.read >= len(artifact) {
		t.Fatalf("the package was read before the caller asked for it: %d of %d bytes", counter.read, len(artifact))
	}
	if part.FileName() != "app-v1.tar.gz" {
		t.Fatalf("file name = %q", part.FileName())
	}
	// A field sent before the package is already available, and the body still
	// reads back whole.
	if fields.value("batchId") != "abc123" {
		t.Fatalf("field before the package = %q", fields.value("batchId"))
	}
	stored, err := io.ReadAll(part)
	if err != nil {
		t.Fatalf("read the package: %v", err)
	}
	if !bytes.Equal(stored, artifact) {
		t.Fatalf("stored %d bytes, want %d", len(stored), len(artifact))
	}
}

// The browser sends the package first, so the batch fields arrive after it and
// are read once the upload is on disk.
func TestReadSimpleUploadFieldsCollectsWhatFollowsThePackage(t *testing.T) {
	body, boundary := uploadBody(t, "app-v1.tar.gz", []byte("payload"),
		map[string]string{"batchId": "abc123", "batchLast": "false"}, true)
	parts := multipart.NewReader(body, boundary)
	fields := simpleUploadFields{}

	part, err := nextSimpleArtifactPart(parts, fields)
	if err != nil {
		t.Fatalf("next artifact part: %v", err)
	}
	if _, err := io.Copy(io.Discard, part); err != nil {
		t.Fatalf("read the package: %v", err)
	}
	if len(fields) != 0 {
		t.Fatalf("no field precedes the package here, got %v", fields)
	}

	readSimpleUploadFields(parts, fields)
	batch := readUploadBatch(fields.value)
	if batch.ID != "abc123" || batch.Last {
		t.Fatalf("batch = %+v", batch)
	}
}

// A request with no package at all is a different answer to the caller than a
// stream that could not be read, so it gets its own error.
func TestNextSimpleArtifactPartReportsAMissingPackage(t *testing.T) {
	body, boundary := uploadBody(t, "", nil, map[string]string{"batchId": "abc123"}, true)
	fields := simpleUploadFields{}

	if _, err := nextSimpleArtifactPart(multipart.NewReader(body, boundary), fields); !errors.Is(err, errMissingArtifact) {
		t.Fatalf("error = %v, want errMissingArtifact", err)
	}
	// The fields are still collected, which is what makes the loop cheap: it
	// stops at the package or at the end of the request, never in between.
	if fields.value("batchId") != "abc123" {
		t.Fatalf("field = %q", fields.value("batchId"))
	}
}

// A request body that fails part way through - the size limit tripping, a
// connection lost while the fields were still arriving - must not be reported
// as a request that carried no package: the caller tells the two apart in what
// it answers the user.
func TestNextSimpleArtifactPartReportsABrokenStream(t *testing.T) {
	body, boundary := uploadBody(t, "app-v1.tar.gz", []byte("payload"), nil, true)
	broken := io.MultiReader(io.LimitReader(body, 20), &failingReader{})

	_, err := nextSimpleArtifactPart(multipart.NewReader(broken, boundary), simpleUploadFields{})
	if err == nil || errors.Is(err, errMissingArtifact) {
		t.Fatalf("error = %v, want the stream failure", err)
	}
}

// failingReader stands in for a body that stops being readable.
type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// Fields are held in memory, so both how many and how large they may be are
// bounded. A second file part is not a field and is never buffered.
func TestSimpleUploadFieldsAreBounded(t *testing.T) {
	long := strings.Repeat("x", maxSimpleUploadFieldBytes+500)
	values := map[string]string{"batchId": long}
	for index := range maxSimpleUploadFields + 4 {
		values["extra"+string(rune('a'+index))] = "value"
	}
	body, boundary := uploadBody(t, "", nil, values, true)
	fields := simpleUploadFields{}

	readSimpleUploadFields(multipart.NewReader(body, boundary), fields)
	if len(fields) > maxSimpleUploadFields {
		t.Fatalf("kept %d fields, want at most %d", len(fields), maxSimpleUploadFields)
	}
	if stored, ok := fields["batchId"]; ok && len(stored) != maxSimpleUploadFieldBytes {
		t.Fatalf("field length = %d, want it cut to %d", len(stored), maxSimpleUploadFieldBytes)
	}
}

// The endpoint itself, exercised the way the browser sends it: the package
// first and the batch fields after it. Both have to survive a stream that is
// walked part by part, and the package has to land in the target directory
// under its own name.
func TestCreateSimpleRunStoresThePackageAndTheBatchFieldsSentAfterIt(t *testing.T) {
	s, targetID := newSimpleBatchFixture(t)
	uploadDir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatalf("create the upload directory: %v", err)
	}
	command := filepath.Join(t.TempDir(), "deploy.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write the deployment command: %v", err)
	}
	if _, err := s.store.Pool.Exec(t.Context(),
		`UPDATE simple_targets SET upload_dir=$2,command_path=$3 WHERE id=$1`, targetID, uploadDir, command); err != nil {
		t.Fatalf("point the target at the test directories: %v", err)
	}

	artifact := bytes.Repeat([]byte("package"), 1000)
	body, boundary := uploadBody(t, "app-v1.tar.gz", artifact,
		map[string]string{"batchId": "abc123", "batchLast": "false"}, true)
	request := httptest.NewRequest(http.MethodPost, "/simple/targets/"+targetID+"/runs", body)
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	request.SetPathValue("id", targetID)
	request = request.WithContext(context.WithValue(t.Context(), principalKey, store.Principal{UserID: "batch-actor"}))
	recorder := httptest.NewRecorder()

	s.createSimpleRun(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ID        string `json:"id"`
		Filename  string `json:"filename"`
		SizeBytes int64  `json:"sizeBytes"`
		BatchID   string `json:"batchId"`
		BatchLast bool   `json:"batchLast"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	if response.Filename != "app-v1.tar.gz" || response.SizeBytes != int64(len(artifact)) {
		t.Fatalf("unexpected package: %+v", response)
	}
	if response.BatchID != "abc123" || response.BatchLast {
		t.Fatalf("the batch fields sent after the package were lost: %+v", response)
	}
	stored, err := os.ReadFile(filepath.Join(uploadDir, "app-v1.tar.gz")) //nolint:gosec
	if err != nil {
		t.Fatalf("read the stored package: %v", err)
	}
	if !bytes.Equal(stored, artifact) {
		t.Fatalf("stored %d bytes, want %d", len(stored), len(artifact))
	}

	// The command runs in a goroutine of its own, so the run is waited out
	// rather than left to write into a schema the cleanup is dropping.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var status string
		if err := s.store.Pool.QueryRow(t.Context(),
			`SELECT status FROM simple_runs WHERE id=$1`, response.ID).Scan(&status); err != nil {
			t.Fatalf("read the run: %v", err)
		}
		if status == "SUCCESS" {
			break
		}
		if status == "FAILED" || status == "TIMEOUT" || time.Now().After(deadline) {
			t.Fatalf("run ended as %s", status)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var batchID string
	var batchLast bool
	if err := s.store.Pool.QueryRow(t.Context(),
		`SELECT batch_id,batch_last FROM simple_runs WHERE id=$1`, response.ID).Scan(&batchID, &batchLast); err != nil {
		t.Fatalf("read the stored batch: %v", err)
	}
	if batchID != "abc123" || batchLast {
		t.Fatalf("stored batch = %q/%v", batchID, batchLast)
	}
}

func TestSimpleUploadFieldsIgnoresFileParts(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("other", "notes.txt")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write([]byte("contents")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	fields := simpleUploadFields{}

	readSimpleUploadFields(multipart.NewReader(body, writer.Boundary()), fields)
	if len(fields) != 0 {
		t.Fatalf("a file part must not be kept as a field, got %v", fields)
	}
}
