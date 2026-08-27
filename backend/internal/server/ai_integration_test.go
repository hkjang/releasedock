package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSessionCookie = "releasedock_session"
	testCSRFCookie    = "releasedock_csrf"
)

type aiUpstreamCapture struct {
	mu            sync.Mutex
	mode          string
	requests      []map[string]any
	authorization []string
	streamStarted chan struct{}
	streamRelease chan struct{}
	streamOnce    sync.Once
	redirectURL   string
}

func (capture *aiUpstreamCapture) handler(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	capture.mu.Lock()
	capture.requests = append(capture.requests, payload)
	capture.authorization = append(capture.authorization, r.Header.Get("Authorization"))
	mode := capture.mode
	redirectURL := capture.redirectURL
	capture.mu.Unlock()
	switch mode {
	case "error":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota"}}`))
	case "redirect":
		http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
	default:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		capture.streamOnce.Do(func() { close(capture.streamStarted) })
		<-capture.streamRelease
		_, _ = w.Write([]byte("data: second\n\ndata: [DONE]\n\n"))
	}
}

func (capture *aiUpstreamCapture) snapshot() ([]map[string]any, []string) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	requests := append([]map[string]any(nil), capture.requests...)
	authorization := append([]string(nil), capture.authorization...)
	return requests, authorization
}

func waitForAIIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.aiMu.Lock()
		idle := s.aiTotal == 0 && len(s.aiActive) == 0
		s.aiMu.Unlock()
		if idle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("AI concurrency accounting did not return to zero")
}

func waitForAIAudit(t *testing.T, fixture rollbackRetryFixture, minimumID int64) (int64, string, map[string]any) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var id int64
		var outcome string
		var details []byte
		err := fixture.store.Pool.QueryRow(t.Context(), `SELECT id,outcome,details FROM audit_logs WHERE action='ai.chat.completions' AND id>$1 ORDER BY id DESC LIMIT 1`, minimumID).Scan(&id, &outcome, &details)
		if err == nil {
			var decoded map[string]any
			if json.Unmarshal(details, &decoded) != nil {
				t.Fatalf("decode AI audit details: %s", details)
			}
			return id, outcome, decoded
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("AI request audit was not persisted")
	return 0, "", nil
}

func TestAIHTTPStreamingPolicyAuditAndCleanupIntegration(t *testing.T) {
	fixture := newRollbackRetryFixture(t)
	const adminPassword = "AI-E2E-Administrator-Password!"
	if err := fixture.store.BootstrapAdmin(t.Context(), "ai-admin", adminPassword); err != nil {
		t.Fatalf("bootstrap AI test administrator: %v", err)
	}

	var redirectDestinationCalled atomic.Bool
	redirectDestination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectDestinationCalled.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectDestination.Close()
	capture := &aiUpstreamCapture{
		streamStarted: make(chan struct{}),
		streamRelease: make(chan struct{}),
		redirectURL:   redirectDestination.URL,
	}
	upstream := httptest.NewServer(http.HandlerFunc(capture.handler))
	defer upstream.Close()
	application := httptest.NewServer(fixture.server.Handler())
	defer application.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	loginRequest, err := http.NewRequest(http.MethodPost, application.URL+"/api/v1/auth/login", strings.NewReader(`{"username":"ai-admin","password":"`+adminPassword+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	loginRequest.Header.Set("Content-Type", "application/json")
	loginResponse, err := client.Do(loginRequest)
	if err != nil {
		t.Fatalf("login AI test administrator: %v", err)
	}
	loginPayload, err := io.ReadAll(loginResponse.Body)
	loginResponse.Body.Close()
	if err != nil || loginResponse.StatusCode != http.StatusOK {
		t.Fatalf("login AI test administrator status=%d body=%s err=%v", loginResponse.StatusCode, loginPayload, err)
	}
	var loginBody struct {
		CSRFToken string `json:"csrfToken"`
	}
	if json.Unmarshal(loginPayload, &loginBody) != nil || loginBody.CSRFToken == "" {
		t.Fatalf("login did not return a CSRF token: %s", loginPayload)
	}
	csrfToken := loginBody.CSRFToken
	var sessionToken string
	for _, cookie := range loginResponse.Cookies() {
		if cookie.Name == testSessionCookie {
			sessionToken = cookie.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("login did not return a session cookie")
	}

	do := func(method, path, body string) (*http.Response, []byte) {
		t.Helper()
		request, err := http.NewRequest(method, application.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", csrfToken)
		request.AddCookie(&http.Cookie{Name: testSessionCookie, Value: sessionToken})
		request.AddCookie(&http.Cookie{Name: testCSRFCookie, Value: csrfToken})
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		payload, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return response, payload
	}

	settingsBody, _ := json.Marshal(map[string]any{
		"enabled": true, "baseUrl": upstream.URL + "/v1", "model": "admin-model",
		"apiKey": "upstream-secret", "maxTokens": absoluteMaxTokens,
	})
	settingsResponse, settingsPayload := do(http.MethodPut, "/api/v1/admin/settings/ai", string(settingsBody))
	if settingsResponse.StatusCode != http.StatusOK || bytes.Contains(settingsPayload, []byte("upstream-secret")) || !bytes.Contains(settingsPayload, []byte(`"keyConfigured":true`)) {
		t.Fatalf("save AI settings status=%d body=%s", settingsResponse.StatusCode, settingsPayload)
	}
	var encrypted string
	if err := fixture.store.Pool.QueryRow(t.Context(), `SELECT api_key_enc FROM ai_settings WHERE id='default'`).Scan(&encrypted); err != nil || encrypted == "" || encrypted == "upstream-secret" {
		t.Fatalf("AI credential was not encrypted: value=%q err=%v", encrypted, err)
	}

	streamRequest, err := http.NewRequest(http.MethodPost, application.URL+"/api/v1/ai/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"audit-secret-prompt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	streamRequest.Header.Set("Content-Type", "application/json")
	streamRequest.Header.Set("X-CSRF-Token", csrfToken)
	streamRequest.AddCookie(&http.Cookie{Name: testSessionCookie, Value: sessionToken})
	streamRequest.AddCookie(&http.Cookie{Name: testCSRFCookie, Value: csrfToken})
	type streamResult struct {
		response *http.Response
		err      error
	}
	responseReady := make(chan streamResult, 1)
	go func() {
		response, requestErr := client.Do(streamRequest)
		responseReady <- streamResult{response: response, err: requestErr}
	}()
	var streamed *http.Response
	select {
	case result := <-responseReady:
		if result.err != nil {
			close(capture.streamRelease)
			t.Fatalf("streaming AI call: %v", result.err)
		}
		streamed = result.response
	case <-time.After(2 * time.Second):
		close(capture.streamRelease)
		t.Fatal("AI proxy buffered the response instead of exposing the first SSE chunk")
	}
	reader := bufio.NewReader(streamed.Body)
	firstLine, err := reader.ReadString('\n')
	if err != nil || firstLine != "data: first\n" {
		close(capture.streamRelease)
		streamed.Body.Close()
		t.Fatalf("first streamed line=%q err=%v", firstLine, err)
	}
	select {
	case <-capture.streamStarted:
	default:
		close(capture.streamRelease)
		streamed.Body.Close()
		t.Fatal("client received data before the upstream flush marker")
	}
	close(capture.streamRelease)
	remainder, err := io.ReadAll(reader)
	streamed.Body.Close()
	if err != nil || streamed.StatusCode != http.StatusOK || !strings.Contains(string(remainder), "data: second") || !strings.Contains(string(remainder), "data: [DONE]") {
		t.Fatalf("stream completion status=%d remainder=%q err=%v", streamed.StatusCode, remainder, err)
	}
	requests, authorization := capture.snapshot()
	if len(requests) != 1 || requests[0]["model"] != "admin-model" || requests[0]["stream"] != true || requests[0]["max_tokens"] != float64(absoluteMaxTokens) || len(authorization) != 1 || authorization[0] != "Bearer upstream-secret" {
		t.Fatalf("upstream policy mismatch: requests=%v authorization=%v", requests, authorization)
	}
	waitForAIIdle(t, fixture.server)
	auditID, outcome, details := waitForAIAudit(t, fixture, 0)
	if outcome != "success" || details["model"] != "admin-model" || details["stream"] != true || details["maxTokens"] != float64(absoluteMaxTokens) || details["upstreamStatus"] != float64(http.StatusOK) {
		t.Fatalf("success audit mismatch: outcome=%s details=%v", outcome, details)
	}
	encodedDetails, _ := json.Marshal(details)
	if bytes.Contains(encodedDetails, []byte("audit-secret-prompt")) {
		t.Fatalf("AI prompt leaked into audit: %s", encodedDetails)
	}

	requestsBefore, _ := capture.snapshot()
	for _, invalid := range []string{
		`{"model":"caller-model","messages":[]}`,
		`{"max_tokens":262145,"messages":[]}`,
	} {
		response, payload := do(http.MethodPost, "/api/v1/ai/chat/completions", invalid)
		if response.StatusCode != http.StatusBadRequest || !bytes.Contains(payload, []byte("invalid_ai_request")) {
			t.Fatalf("invalid AI policy status=%d body=%s", response.StatusCode, payload)
		}
	}
	requestsAfter, _ := capture.snapshot()
	if len(requestsAfter) != len(requestsBefore) {
		t.Fatal("policy-rejected request reached the upstream")
	}
	waitForAIIdle(t, fixture.server)

	capture.mu.Lock()
	capture.mode = "error"
	capture.mu.Unlock()
	errorResponse, errorPayload := do(http.MethodPost, "/api/v1/ai/chat/completions", `{"messages":[]}`)
	if errorResponse.StatusCode != http.StatusTooManyRequests || !bytes.Contains(errorPayload, []byte("quota")) {
		t.Fatalf("upstream error status=%d body=%s", errorResponse.StatusCode, errorPayload)
	}
	waitForAIIdle(t, fixture.server)
	auditID, outcome, details = waitForAIAudit(t, fixture, auditID)
	if outcome != "failure" || details["upstreamStatus"] != float64(http.StatusTooManyRequests) {
		t.Fatalf("upstream error audit mismatch: outcome=%s details=%v", outcome, details)
	}

	capture.mu.Lock()
	capture.mode = "redirect"
	capture.mu.Unlock()
	redirectResponse, redirectPayload := do(http.MethodPost, "/api/v1/ai/chat/completions", `{"messages":[]}`)
	if redirectResponse.StatusCode != http.StatusBadGateway || !bytes.Contains(redirectPayload, []byte("ai_upstream_unavailable")) || redirectDestinationCalled.Load() {
		t.Fatalf("redirect handling status=%d destinationCalled=%v body=%s", redirectResponse.StatusCode, redirectDestinationCalled.Load(), redirectPayload)
	}
	waitForAIIdle(t, fixture.server)
	_, outcome, details = waitForAIAudit(t, fixture, auditID)
	if outcome != "failure" || details["upstreamStatus"] != float64(0) {
		t.Fatalf("redirect audit mismatch: outcome=%s details=%v", outcome, details)
	}
}
