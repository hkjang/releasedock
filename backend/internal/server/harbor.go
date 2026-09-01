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

func (s *Server) harborRequest(ctx context.Context, reg harborRegistry, method, path string, body any) (*http.Response, string, error) {
	target, err := harborURL(reg.Endpoint, path)
	if err != nil {
		return nil, target, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, target, err
		}
		reader = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, target, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.SetBasicAuth(reg.Username, reg.Password)
	client := harborClient(reg.Insecure)
	defer client.CloseIdleConnections()
	resp, err := client.Do(request)
	return resp, target, err
}

// harborError turns a non-2xx response into a message an operator can act on.
// The requested URL is included because the most common failures are a wrong
// endpoint or a proxy that does not expose Harbor's API, and neither is
// diagnosable without seeing what was actually called.
func harborError(action, requestURL string, resp *http.Response) error {
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	trimmed := strings.TrimSpace(string(detail))
	if len(trimmed) > 300 {
		trimmed = trimmed[:300] + "…"
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf(
			"%s: Harbor 인증이 거부되었습니다 (HTTP %d, %s). Robot 계정에 replication 조회/실행 권한이 있는지 확인하십시오",
			action, resp.StatusCode, requestURL)
	case http.StatusNotFound:
		return fmt.Errorf(
			"%s: Harbor API 경로를 찾을 수 없습니다 (HTTP 404, %s). "+
				"레지스트리 Endpoint 가 Harbor 포털 주소인지, 앞단 프록시가 /api/ 를 통과시키는지, "+
				"Harbor 가 v2 API(/api/v2.0)를 제공하는 버전인지 확인하십시오",
			action, requestURL)
	}
	if trimmed == "" {
		return fmt.Errorf("%s: Harbor 가 HTTP %d 로 응답했습니다 (%s)", action, resp.StatusCode, requestURL)
	}
	return fmt.Errorf("%s: Harbor 가 HTTP %d 로 응답했습니다 (%s): %s", action, resp.StatusCode, requestURL, trimmed)
}

// listReplicationPolicies fetches the rules configured on a Harbor so an
// administrator can pick one instead of typing an opaque id.
func (s *Server) listReplicationPolicies(ctx context.Context, reg harborRegistry) ([]replicationPolicy, error) {
	resp, requestURL, err := s.harborRequest(ctx, reg, http.MethodGet, "/api/v2.0/replication/policies?page_size=100", nil)
	if err != nil {
		return nil, fmt.Errorf("Harbor 에 연결하지 못했습니다: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			return nil, s.diagnoseReplicationAccess(ctx, reg, requestURL)
		}
		return nil, harborError("복제 규칙 조회", requestURL, resp)
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
	resp, requestURL, err := s.harborRequest(ctx, reg, http.MethodPost, "/api/v2.0/replication/executions",
		map[string]any{"policy_id": policyID})
	if err != nil {
		return 0, fmt.Errorf("Harbor 에 연결하지 못했습니다: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, harborError("복제 실행", requestURL, resp)
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
	resp, requestURL, err := s.harborRequest(ctx, reg, http.MethodGet,
		fmt.Sprintf("/api/v2.0/replication/executions/%d", executionID), nil)
	if err != nil {
		return replicationExecution{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return replicationExecution{}, harborError("복제 상태 조회", requestURL, resp)
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

// diagnoseReplicationAccess runs after a 404 from the replication API to tell
// apart the three causes that produce the same status: the endpoint is not a
// Harbor API root, the Harbor is too old for the v2 API, or the robot account
// cannot see system-level resources. Harbor answers 404 rather than 403 for
// resources a principal is not allowed to know about, so the status alone is
// not enough to act on.
func (s *Server) diagnoseReplicationAccess(ctx context.Context, reg harborRegistry, requestURL string) error {
	base := fmt.Sprintf(
		"복제 규칙 조회: Harbor 가 404 로 응답했습니다 (%s).", requestURL)

	resp, probeURL, err := s.harborRequest(ctx, reg, http.MethodGet, "/api/v2.0/systeminfo", nil)
	if err != nil {
		return fmt.Errorf("%s Harbor API 에 연결하지 못했습니다 (%s): %w", base, probeURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf(
			"%s v2 API 자체가 응답하지 않습니다 (%s 도 404). "+
				"레지스트리 Endpoint 가 Harbor 포털 주소인지, 앞단 프록시가 /api/ 를 통과시키는지, "+
				"Harbor 가 2.0 이상인지 확인하십시오",
			base, probeURL)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return fmt.Errorf(
			"%s Harbor API 는 정상 응답하므로 경로 문제가 아니라 권한 문제입니다. "+
				"복제 규칙은 시스템 수준 리소스라 프로젝트 범위 Robot 계정으로는 조회할 수 없습니다. "+
				"replication 조회/실행 권한을 가진 시스템 Robot 계정이나 관리자 계정을 사용하십시오",
			base)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf(
			"%s Harbor 가 이 계정을 거부했습니다 (%s 에서 HTTP %d). "+
				"Robot 계정의 사용자명과 비밀번호, 만료 여부를 확인하십시오",
			base, probeURL, resp.StatusCode)
	default:
		return fmt.Errorf("%s 진단 요청도 HTTP %d 로 응답했습니다 (%s)", base, resp.StatusCode, probeURL)
	}
}

// harborProbe is one endpoint check in a connection test.
type harborProbe struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

// harborDiagnosis is the structured result of a connection test, built so an
// operator can tell authentication, authorisation and routing apart without
// shell access to the server.
type harborDiagnosis struct {
	RegistryName string        `json:"registryName"`
	Endpoint     string        `json:"endpoint"`
	Username     string        `json:"username"`
	RobotPrefix  bool          `json:"robotPrefix"`
	Probes       []harborProbe `json:"probes"`
	Verdict      string        `json:"verdict"`
	Detail       string        `json:"detail"`
}

func (s *Server) probeHarbor(ctx context.Context, reg harborRegistry, name, path string) harborProbe {
	probe := harborProbe{Name: name}
	resp, requestURL, err := s.harborRequest(ctx, reg, http.MethodGet, path, nil)
	probe.URL = requestURL
	if err != nil {
		probe.Error = err.Error()
		return probe
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	probe.Status = resp.StatusCode
	return probe
}

// diagnoseHarbor probes an unauthenticated-safe endpoint, an authenticated
// endpoint, and the replication API. Comparing the three separates the causes
// that all surface as a bare 404 on the replication call.
func (s *Server) diagnoseHarbor(ctx context.Context, reg harborRegistry) harborDiagnosis {
	report := harborDiagnosis{
		RegistryName: reg.Name,
		Endpoint:     reg.Endpoint,
		Username:     reg.Username,
		// Harbor rejects a robot account whose name is missing the prefix it
		// issued, and an unauthenticated caller is shown 404 rather than 401 on
		// system resources, so the two failures look identical.
		RobotPrefix: strings.HasPrefix(reg.Username, "robot$"),
	}
	ping := s.probeHarbor(ctx, reg, "ping", "/api/v2.0/ping")
	systeminfo := s.probeHarbor(ctx, reg, "systeminfo", "/api/v2.0/systeminfo")
	volumes := s.probeHarbor(ctx, reg, "systeminfo/volumes", "/api/v2.0/systeminfo/volumes")
	policies := s.probeHarbor(ctx, reg, "replication/policies", "/api/v2.0/replication/policies?page_size=1")
	report.Probes = []harborProbe{ping, systeminfo, volumes, policies}

	switch {
	case ping.Error != "" && systeminfo.Error != "":
		report.Verdict = "unreachable"
		report.Detail = "Harbor 에 연결하지 못했습니다. Endpoint, 방화벽, TLS 인증서를 확인하십시오: " + ping.Error
	case ping.Status == 404 && systeminfo.Status == 404:
		report.Verdict = "not_harbor_v2"
		report.Detail = "v2 API 가 응답하지 않습니다. Endpoint 가 Harbor 포털 주소인지, 앞단 프록시가 /api/ 를 통과시키는지, Harbor 가 2.0 이상인지 확인하십시오."
	case volumes.Status == 401 || volumes.Status == 403 || policies.Status == 401 || policies.Status == 403:
		report.Verdict = "unauthorized"
		report.Detail = "Harbor 가 이 계정을 거부했습니다. 사용자명과 비밀번호, Robot 계정 만료 여부를 확인하십시오."
		if !report.RobotPrefix {
			report.Detail += " 사용자명에 Harbor 가 발급한 robot$ 접두어가 빠져 있습니다."
		}
	case policies.Status >= 200 && policies.Status < 300:
		report.Verdict = "ok"
		report.Detail = "복제 규칙을 조회할 수 있습니다."
	case policies.Status == 404 && volumes.Status >= 200 && volumes.Status < 300:
		report.Verdict = "forbidden_system_scope"
		report.Detail = "인증은 되지만 복제 API 만 404 입니다. 복제 규칙은 시스템 수준 리소스이므로 시스템 범위 Robot 계정이나 관리자 계정이 필요합니다."
	case policies.Status == 404:
		report.Verdict = "unauthenticated_or_scope"
		report.Detail = "인증된 요청으로 인식되지 않아 Harbor 가 시스템 리소스를 404 로 숨기고 있을 가능성이 높습니다."
		if !report.RobotPrefix {
			report.Detail += " 사용자명에 Harbor 가 발급한 robot$ 접두어가 빠져 있습니다. Harbor 에 표시된 전체 이름을 그대로 입력하십시오."
		} else {
			report.Detail += " Robot 계정 만료 여부와 시스템 범위 여부를 확인하십시오."
		}
	default:
		report.Verdict = "unexpected"
		report.Detail = fmt.Sprintf("복제 API 가 HTTP %d 로 응답했습니다.", policies.Status)
	}
	return report
}
