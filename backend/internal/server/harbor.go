package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// harborRegistry is a configured Harbor endpoint with its robot credential
// already decrypted, ready to be called.
type harborRegistry struct {
	ID       string
	Name     string
	Endpoint string
	Username string
	Password string
	Insecure bool
}

// replicationPolicy is the subset of a Harbor replication rule the operator
// needs in order to pick one.
type replicationPolicy struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Destination string `json:"destination"`
}

// loadHarborRegistry decrypts the stored robot credential for a registry. The
// API process already holds the encryption key because it is what wrote these
// records, so no new secret boundary is crossed here.
func (s *Server) loadHarborRegistry(ctx context.Context, registryID string) (harborRegistry, error) {
	var reg harborRegistry
	var version int
	var ciphertext string
	err := s.store.Pool.QueryRow(ctx,
		`SELECT id::text,name,endpoint,username,insecure_skip_verify,version,ciphertext
		 FROM runner_credentials WHERE id=$1 AND revoked_at IS NULL AND active`, registryID).
		Scan(&reg.ID, &reg.Name, &reg.Endpoint, &reg.Username, &reg.Insecure, &version, &ciphertext)
	if err != nil {
		return harborRegistry{}, err
	}
	plaintext, err := s.vault.Decrypt(ciphertext, registryAAD(reg.ID, version))
	if err != nil {
		return harborRegistry{}, fmt.Errorf("decrypt registry credential: %w", err)
	}
	var credential struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(plaintext), &credential); err != nil {
		return harborRegistry{}, errors.New("stored registry credential is invalid")
	}
	if credential.Username != "" {
		reg.Username = credential.Username
	}
	reg.Password = credential.Password
	return reg, nil
}

// harborClient builds a client for one call. A registry may legitimately use a
// private CA in an air-gapped network, so the per-registry TLS decision cannot
// come from the shared client.
func harborClient(insecure bool) *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // administrator opt-in per registry
	}
	return &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// A redirect could send the robot credential somewhere else.
			return errOutboundRedirect
		},
	}
}

func harborURL(endpoint, path string) (string, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(endpoint), "/"))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return "", fmt.Errorf("registry endpoint is invalid: %q", endpoint)
	}
	return base.Scheme + "://" + base.Host + path, nil
}

func (s *Server) harborRequest(ctx context.Context, reg harborRegistry, method, path string, body any) (*http.Response, error) {
	target, err := harborURL(reg.Endpoint, path)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.SetBasicAuth(reg.Username, reg.Password)
	client := harborClient(reg.Insecure)
	defer client.CloseIdleConnections()
	return client.Do(request)
}

// harborError turns a non-2xx response into a message an operator can act on,
// without echoing an unbounded response body.
func harborError(action string, resp *http.Response) error {
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	trimmed := strings.TrimSpace(string(detail))
	if len(trimmed) > 300 {
		trimmed = trimmed[:300] + "…"
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s: Harbor 인증이 거부되었습니다 (HTTP %d). Robot 계정 권한을 확인하십시오", action, resp.StatusCode)
	case http.StatusNotFound:
		return fmt.Errorf("%s: 대상을 찾을 수 없습니다 (HTTP 404). 복제 규칙 ID 를 확인하십시오", action)
	}
	if trimmed == "" {
		return fmt.Errorf("%s: Harbor 가 HTTP %d 로 응답했습니다", action, resp.StatusCode)
	}
	return fmt.Errorf("%s: Harbor 가 HTTP %d 로 응답했습니다: %s", action, resp.StatusCode, trimmed)
}

// listReplicationPolicies fetches the rules configured on a Harbor so an
// administrator can pick one instead of typing an opaque id.
func (s *Server) listReplicationPolicies(ctx context.Context, reg harborRegistry) ([]replicationPolicy, error) {
	resp, err := s.harborRequest(ctx, reg, http.MethodGet, "/api/v2.0/replication/policies?page_size=100", nil)
	if err != nil {
		return nil, fmt.Errorf("Harbor 에 연결하지 못했습니다: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, harborError("복제 규칙 조회", resp)
	}
	var raw []struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Description  string `json:"description"`
		Enabled      bool   `json:"enabled"`
		DestRegistry *struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"dest_registry"`
		SrcRegistry *struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"src_registry"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("복제 규칙 응답을 해석하지 못했습니다: %w", err)
	}
	policies := make([]replicationPolicy, 0, len(raw))
	for _, item := range raw {
		// Harbor sets exactly one of the two registries; the other end is the
		// Harbor being asked, which is what the operator already selected.
		destination := ""
		if item.DestRegistry != nil {
			destination = item.DestRegistry.Name
		} else if item.SrcRegistry != nil {
			destination = item.SrcRegistry.Name
		}
		policies = append(policies, replicationPolicy{
			ID: item.ID, Name: item.Name, Description: item.Description,
			Enabled: item.Enabled, Destination: destination,
		})
	}
	return policies, nil
}

// startReplication triggers one execution of a rule and returns its id.
func (s *Server) startReplication(ctx context.Context, reg harborRegistry, policyID int64) (int64, error) {
	resp, err := s.harborRequest(ctx, reg, http.MethodPost, "/api/v2.0/replication/executions",
		map[string]any{"policy_id": policyID})
	if err != nil {
		return 0, fmt.Errorf("Harbor 에 연결하지 못했습니다: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, harborError("복제 실행", resp)
	}
	// Harbor returns 201 with the new execution in the Location header rather
	// than in the body.
	if location := resp.Header.Get("Location"); location != "" {
		if index := strings.LastIndex(location, "/"); index >= 0 {
			if id, convErr := strconv.ParseInt(strings.TrimSpace(location[index+1:]), 10, 64); convErr == nil {
				return id, nil
			}
		}
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) == nil && body.ID > 0 {
		return body.ID, nil
	}
	// The rule was triggered even though the id could not be read, so this is
	// reported as a success with an unknown execution rather than a failure.
	return 0, nil
}

type replicationExecution struct {
	ID       int64  `json:"id"`
	Status   string `json:"status"`
	Total    int    `json:"total"`
	Succeed  int    `json:"succeed"`
	Failed   int    `json:"failed"`
	StatusTx string `json:"status_text"`
}

func (s *Server) replicationExecution(ctx context.Context, reg harborRegistry, executionID int64) (replicationExecution, error) {
	resp, err := s.harborRequest(ctx, reg, http.MethodGet,
		fmt.Sprintf("/api/v2.0/replication/executions/%d", executionID), nil)
	if err != nil {
		return replicationExecution{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return replicationExecution{}, harborError("복제 상태 조회", resp)
	}
	var execution replicationExecution
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&execution); err != nil {
		return replicationExecution{}, err
	}
	return execution, nil
}

// replicationTerminal maps Harbor's execution states. Harbor reports "Succeed"
// for a finished run and "Stopped" for a cancelled one.
func replicationTerminal(status string) (done bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeed", "succeeded", "success":
		return true, true
	case "failed", "error":
		return true, false
	case "stopped":
		return true, false
	default:
		return false, false
	}
}
