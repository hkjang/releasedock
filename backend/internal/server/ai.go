package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const absoluteMaxTokens = 262144

type aiRequestMetadata struct {
	Model               string `json:"model"`
	MaxTokens           *int   `json:"maxTokens,omitempty"`
	MaxCompletionTokens *int   `json:"maxCompletionTokens,omitempty"`
	Stream              bool   `json:"stream"`
	UpstreamStatus      int    `json:"upstreamStatus"`
}

func applyAIRequestPolicy(payload map[string]any, cfg aiSettings) (aiRequestMetadata, error) {
	if requested := strings.TrimSpace(stringValue(payload["model"])); requested != "" && requested != cfg.Model {
		return aiRequestMetadata{}, errors.New("model override is not allowed")
	}
	payload["model"] = cfg.Model
	if _, ok := payload["stream"]; !ok {
		payload["stream"] = true
	}
	stream, ok := payload["stream"].(bool)
	if !ok {
		return aiRequestMetadata{}, errors.New("stream must be a boolean")
	}
	metadata := aiRequestMetadata{Model: cfg.Model, Stream: stream}
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		if value, exists := payload[field]; exists {
			tokens, valid := integerValue(value)
			if !valid || tokens < 1 || tokens > cfg.MaxTokens || tokens > absoluteMaxTokens {
				return aiRequestMetadata{}, errors.New("requested token limit exceeds the configured maximum")
			}
			if field == "max_tokens" {
				metadata.MaxTokens = &tokens
			} else {
				metadata.MaxCompletionTokens = &tokens
			}
		}
	}
	if metadata.MaxTokens == nil && metadata.MaxCompletionTokens == nil {
		payload["max_tokens"] = cfg.MaxTokens
		value := cfg.MaxTokens
		metadata.MaxTokens = &value
	}
	return metadata, nil
}

type aiAuditSnapshot struct {
	Context   context.Context
	ActorID   string
	IP        string
	UserAgent string
}

func snapshotAIRequest(r *http.Request) aiAuditSnapshot {
	p, _ := principalFrom(r)
	return aiAuditSnapshot{Context: context.WithoutCancel(r.Context()), ActorID: p.UserID, IP: remoteIP(r), UserAgent: r.UserAgent()}
}

func (s *Server) auditAIRequest(snapshot aiAuditSnapshot, metadata aiRequestMetadata, outcome string) {
	details, _ := json.Marshal(metadata)
	ctx, cancel := context.WithTimeout(snapshot.Context, 5*time.Second)
	defer cancel()
	s.store.Audit(ctx, snapshot.ActorID, "ai.chat.completions", "ai", metadata.Model, outcome, snapshot.IP, snapshot.UserAgent, details)
}

func (s *Server) proxyAI(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadAI(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load AI settings")
		return
	}
	if !cfg.Enabled {
		writeError(w, http.StatusServiceUnavailable, "ai_disabled", "AI integration is disabled")
		return
	}
	var payload map[string]any
	if !decodeJSON(w, r, &payload) {
		return
	}
	metadata, policyErr := applyAIRequestPolicy(payload, cfg)
	if policyErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_ai_request", policyErr.Error())
		return
	}
	auditSnapshot := snapshotAIRequest(r)
	if !s.acquireAIRequest(auditSnapshot.ActorID) {
		s.auditAIRequest(auditSnapshot, metadata, "rate_limited")
		writeError(w, http.StatusTooManyRequests, "ai_concurrency_limit", "too many concurrent AI requests")
		return
	}
	defer s.releaseAIRequest(auditSnapshot.ActorID)
	body, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "AI request could not be encoded")
		return
	}
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	parsed, parseErr := url.Parse(endpoint)
	if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		writeError(w, http.StatusInternalServerError, "ai_configuration_error", "AI endpoint is invalid")
		return
	}
	if !strings.HasSuffix(parsed.Path, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ai_configuration_error", "AI endpoint is invalid")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	if cfg.APIKeyEnc != "" {
		apiKey, decryptErr := s.vault.Decrypt(cfg.APIKeyEnc, "ai.api_key")
		if decryptErr != nil {
			s.log.Error("decrypt AI API key", "error", decryptErr)
			writeError(w, http.StatusInternalServerError, "secret_error", "AI credential could not be decrypted")
			return
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.auditAIRequest(auditSnapshot, metadata, "failure")
		writeError(w, http.StatusBadGateway, "ai_upstream_unavailable", "AI endpoint is unavailable")
		return
	}
	defer resp.Body.Close()
	metadata.UpstreamStatus = resp.StatusCode
	outcome := "success"
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		outcome = "failure"
	}
	defer func() { s.auditAIRequest(auditSnapshot, metadata, outcome) }()
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				outcome = "failure"
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				s.log.Warn("AI upstream stream ended unexpectedly", "error", readErr)
				outcome = "failure"
			}
			break
		}
	}
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func integerValue(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) {
		return 0, false
	}
	return int(number), true
}
