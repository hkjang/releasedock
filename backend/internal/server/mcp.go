package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	mcpProtocolVersion       = "2026-07-28"
	mcpLegacyProtocolVersion = "2025-11-25"
)

func supportedMCPProtocolVersion(version string) bool {
	return version == mcpProtocolVersion || version == mcpLegacyProtocolVersion
}

func validMCPPostAccept(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "application/json") && strings.Contains(value, "text/event-stream")
}

func validModernMCPRequestHeaders(r *http.Request, method, expectedName string) bool {
	if r.Header.Get("Mcp-Method") != method {
		return false
	}
	if expectedName == "" {
		return true
	}
	name, ok := decodeMCPNameHeader(r.Header.Get("Mcp-Name"))
	return ok && name == expectedName
}

func decodeMCPNameHeader(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		encoded := strings.TrimSuffix(strings.TrimPrefix(value, "=?base64?"), "?=")
		var decoded []byte
		var err error
		for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			decoded, err = encoding.DecodeString(encoded)
			if err == nil {
				value = string(decoded)
				break
			}
		}
		if err != nil {
			return "", false
		}
	}
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	return value, true
}

func modernMCPRequestMetadata(params json.RawMessage, method, headerVersion string) (string, error) {
	var object map[string]json.RawMessage
	if len(bytes.TrimSpace(params)) == 0 || json.Unmarshal(params, &object) != nil {
		return "", errors.New("modern MCP params must be an object")
	}
	var metadata map[string]json.RawMessage
	if raw, ok := object["_meta"]; !ok || json.Unmarshal(raw, &metadata) != nil {
		return "", errors.New("modern MCP params._meta is required")
	}
	var protocolVersion string
	if raw, ok := metadata["io.modelcontextprotocol/protocolVersion"]; !ok || json.Unmarshal(raw, &protocolVersion) != nil || protocolVersion != mcpProtocolVersion || protocolVersion != headerVersion {
		return "", errors.New("params._meta protocolVersion must match MCP-Protocol-Version")
	}
	var capabilities map[string]any
	if raw, ok := metadata["io.modelcontextprotocol/clientCapabilities"]; !ok || json.Unmarshal(raw, &capabilities) != nil || capabilities == nil {
		return "", errors.New("params._meta clientCapabilities must be an object")
	}
	if method == "server/discover" {
		var clientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		raw, ok := metadata["io.modelcontextprotocol/clientInfo"]
		if ok && (json.Unmarshal(raw, &clientInfo) != nil || strings.TrimSpace(clientInfo.Name) == "" || strings.TrimSpace(clientInfo.Version) == "") {
			return "", errors.New("server/discover params._meta clientInfo must contain name and version when provided")
		}
	}
	if method != "tools/call" {
		return "", nil
	}
	var name string
	if raw, ok := object["name"]; !ok || json.Unmarshal(raw, &name) != nil || strings.TrimSpace(name) == "" || len(name) > 128 {
		return "", errors.New("tools/call requires a valid params.name")
	}
	return name, nil
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (s *Server) validMCPOrigin(r *http.Request) bool {
	cfg, err := s.loadPublicHTTPConfig(r.Context())
	if err != nil {
		return false
	}
	requestHostAllowed := loopbackHost(r.Host)
	configuredOrigins := append([]string{}, cfg.AllowedOrigins...)
	if cfg.PublicURL != "" {
		configuredOrigins = append(configuredOrigins, cfg.PublicURL)
	}
	for _, configured := range configuredOrigins {
		parsed, parseErr := url.Parse(configured)
		if parseErr == nil && strings.EqualFold(parsed.Host, r.Host) {
			requestHostAllowed = true
			break
		}
	}
	if !requestHostAllowed {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	normalized, err := normalizeOrigin(origin, false)
	if err != nil {
		return false
	}
	for _, allowed := range configuredOrigins {
		if normalized == allowed {
			return true
		}
	}
	return false
}

func (s *Server) mcpGET(w http.ResponseWriter, r *http.Request) {
	if !s.validMCPOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid_origin", "Origin is not allowed")
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		writeError(w, http.StatusNotAcceptable, "event_stream_required", "MCP GET requires Accept: text/event-stream")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("MCP-Protocol-Version", mcpProtocolVersion)
	w.Header().Set("Mcp-Method", "server/stream")
	w.Header().Set("Mcp-Name", "releasedock")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(": ReleaseDock MCP stream ready\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) mcpPOST(w http.ResponseWriter, r *http.Request) {
	if !s.validMCPOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid_origin", "Origin is not allowed")
		return
	}
	mediaType := strings.ToLower(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "json_required", "MCP POST requires application/json")
		return
	}
	if !validMCPPostAccept(r.Header.Get("Accept")) {
		writeError(w, http.StatusNotAcceptable, "mcp_accept_required", "MCP POST Accept must include application/json and text/event-stream")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var request jsonRPCRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		s.writeMCP(w, r, jsonRPCResponse{JSONRPC: "2.0", Error: &jsonRPCError{Code: -32700, Message: "Parse error"}})
		return
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		s.writeMCP(w, r, jsonRPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &jsonRPCError{Code: -32600, Message: "Invalid Request"}})
		return
	}
	w.Header().Set("Mcp-Method", request.Method)
	w.Header().Set("Mcp-Name", "releasedock")
	negotiatedVersion := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if request.Method == "initialize" {
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || params.ProtocolVersion == "" {
			s.writeMCP(w, r, jsonRPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &jsonRPCError{Code: -32602, Message: "Unsupported or missing protocolVersion", Data: map[string]any{"legacyInitialize": []string{mcpLegacyProtocolVersion}, "stateless": []string{mcpProtocolVersion}}}})
			return
		}
		if params.ProtocolVersion == mcpProtocolVersion {
			w.Header().Set("MCP-Protocol-Version", mcpProtocolVersion)
			s.writeMCP(w, r, jsonRPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &jsonRPCError{Code: -32601, Message: "initialize is not supported by stateless MCP 2026-07-28; use server/discover"}})
			return
		}
		if params.ProtocolVersion != mcpLegacyProtocolVersion {
			s.writeMCP(w, r, jsonRPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &jsonRPCError{Code: -32602, Message: "Unsupported protocolVersion", Data: map[string]any{"legacyInitialize": []string{mcpLegacyProtocolVersion}, "stateless": []string{mcpProtocolVersion}}}})
			return
		}
		negotiatedVersion = mcpLegacyProtocolVersion
	} else if !supportedMCPProtocolVersion(negotiatedVersion) {
		s.writeMCP(w, r, jsonRPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &jsonRPCError{Code: -32600, Message: "Missing or unsupported MCP-Protocol-Version"}})
		return
	}
	if request.Method == "server/discover" && negotiatedVersion != mcpProtocolVersion {
		s.writeMCP(w, r, jsonRPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &jsonRPCError{Code: -32601, Message: "server/discover requires MCP 2026-07-28"}})
		return
	}
	if negotiatedVersion == mcpProtocolVersion {
		expectedName, metadataErr := modernMCPRequestMetadata(request.Params, request.Method, negotiatedVersion)
		if metadataErr != nil || !validModernMCPRequestHeaders(r, request.Method, expectedName) {
			message := "MCP 2026-07-28 routing headers or params._meta are invalid"
			if metadataErr != nil {
				message = metadataErr.Error()
			}
			s.writeMCPStatus(w, r, http.StatusBadRequest, jsonRPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &jsonRPCError{Code: -32020, Message: message}})
			return
		}
	}
	w.Header().Set("MCP-Protocol-Version", negotiatedVersion)
	if request.Method == "notifications/initialized" || strings.HasPrefix(request.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	response := jsonRPCResponse{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": negotiatedVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "releasedock", "version": s.build.Version},
			"instructions":    "ReleaseDock release orchestration API. Tool authorization follows the caller's RBAC permissions and API-key scopes.",
		}
	case "ping":
		response.Result = map[string]any{}
	case "server/discover":
		response.Result = map[string]any{
			"supportedVersions": []string{mcpProtocolVersion},
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions":      "ReleaseDock release orchestration API. Tool authorization follows the caller's RBAC permissions and API-key scopes.",
			"ttlMs":             300_000,
			"cacheScope":        "public",
		}
	case "tools/list":
		response.Result = map[string]any{"tools": mcpTools(), "resultType": "complete", "ttlMs": 300_000, "cacheScope": "public"}
	case "tools/call":
		result, rpcErr := s.callMCPTool(r, request.Params)
		if rpcErr != nil {
			response.Error = rpcErr
		} else {
			response.Result = result
		}
	default:
		response.Error = &jsonRPCError{Code: -32601, Message: "Method not found"}
	}
	if negotiatedVersion == mcpProtocolVersion && response.Error == nil {
		response.Result = s.withModernMCPResultMetadata(response.Result)
	}
	s.writeMCP(w, r, response)
}

func (s *Server) writeMCP(w http.ResponseWriter, r *http.Request, response jsonRPCResponse) {
	s.writeMCPStatus(w, r, http.StatusOK, response)
}

func (s *Server) writeMCPStatus(w http.ResponseWriter, r *http.Request, status int, response jsonRPCResponse) {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		encoded, _ := json.Marshal(response)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("event: message\ndata: "))
		_, _ = w.Write(encoded)
		_, _ = w.Write([]byte("\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	writeJSON(w, status, response)
}

func (s *Server) withModernMCPResultMetadata(result any) any {
	object, ok := result.(map[string]any)
	if !ok {
		return result
	}
	cloned := make(map[string]any, len(object)+1)
	for key, value := range object {
		cloned[key] = value
	}
	if _, exists := cloned["resultType"]; !exists {
		cloned["resultType"] = "complete"
	}
	metadata := map[string]any{}
	if existing, ok := cloned["_meta"].(map[string]any); ok {
		for key, value := range existing {
			metadata[key] = value
		}
	}
	metadata["io.modelcontextprotocol/serverInfo"] = map[string]any{"name": "releasedock", "version": s.build.Version}
	cloned["_meta"] = metadata
	return cloned
}

func mcpTools() []map[string]any {
	objectSchema := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	uuid := func() map[string]any { return map[string]any{"type": "string", "format": "uuid"} }
	limit := func(maximum int) map[string]any {
		return map[string]any{"type": "integer", "minimum": 1, "maximum": maximum}
	}
	return []map[string]any{
		{"name": "releasedock_list_applications", "description": "List applications available to the caller.", "inputSchema": objectSchema(map[string]any{"limit": limit(100), "search": map[string]any{"type": "string"}})},
		{"name": "releasedock_list_environments", "description": "List environments, optionally within one application.", "inputSchema": objectSchema(map[string]any{"applicationId": uuid(), "limit": limit(100)})},
		{"name": "releasedock_list_profiles", "description": "List deployment profiles and their registry, script, and Runner label bindings.", "inputSchema": objectSchema(map[string]any{"applicationId": uuid(), "environmentId": uuid(), "limit": limit(100)})},
		{"name": "releasedock_list_releases", "description": "List ReleaseDock releases and their current states.", "inputSchema": objectSchema(map[string]any{"limit": limit(100), "status": map[string]any{"type": "string"}, "search": map[string]any{"type": "string"}})},
		{"name": "releasedock_get_release", "description": "Get one release including execution steps and image digests.", "inputSchema": objectSchema(map[string]any{"id": uuid()}, "id")},
		{"name": "releasedock_dashboard", "description": "Get aggregate release and approval status.", "inputSchema": objectSchema(map[string]any{})},
		{"name": "releasedock_create_release", "description": "Create release metadata and return the REST multipart artifact upload handoff.", "inputSchema": objectSchema(map[string]any{"applicationId": uuid(), "environmentId": uuid(), "deploymentProfileId": uuid(), "version": map[string]any{"type": "string"}, "notes": map[string]any{"type": "string"}}, "applicationId", "environmentId", "deploymentProfileId", "version")},
		{"name": "releasedock_enqueue_release", "description": "Submit a release for review when required, or enqueue it when approval is bypassed or complete.", "inputSchema": objectSchema(map[string]any{"id": uuid()}, "id")},
		{"name": "releasedock_retry_release", "description": "Explicitly retry a failed deploy or rollback job, creating a new approval request when required.", "inputSchema": objectSchema(map[string]any{"id": uuid()}, "id")},
		{"name": "releasedock_review_release", "description": "Mark an approval-queue release as under review.", "inputSchema": objectSchema(map[string]any{"id": uuid(), "comment": map[string]any{"type": "string", "maxLength": 4096}}, "id")},
		{"name": "releasedock_approve_release", "description": "Approve a release or rollback request using the configured self-approval policy.", "inputSchema": objectSchema(map[string]any{"id": uuid(), "comment": map[string]any{"type": "string", "maxLength": 4096}}, "id")},
		{"name": "releasedock_reject_release", "description": "Reject a release or rollback request.", "inputSchema": objectSchema(map[string]any{"id": uuid(), "comment": map[string]any{"type": "string", "maxLength": 4096}}, "id")},
		{"name": "releasedock_rollback_release", "description": "Request or enqueue rollback to the previous successful release under the same approval policy as REST.", "inputSchema": objectSchema(map[string]any{"id": uuid()}, "id")},
		{"name": "releasedock_list_jobs", "description": "List release execution jobs.", "inputSchema": objectSchema(map[string]any{"limit": limit(100)})},
		{"name": "releasedock_get_release_logs", "description": "Read a bounded page of persisted execution logs for a release.", "inputSchema": objectSchema(map[string]any{"releaseId": uuid(), "after": map[string]any{"type": "integer", "minimum": 0}, "limit": limit(500)}, "releaseId")},
	}
}

type mcpHTTPRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *mcpHTTPRecorder) Header() http.Header { return w.header }
func (w *mcpHTTPRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *mcpHTTPRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (s *Server) callMCPHTTPHandler(r *http.Request, handler http.HandlerFunc, method, id string, query url.Values, body any) (any, int, error) {
	request := r.Clone(r.Context())
	request.Method = method
	clonedURL := *r.URL
	clonedURL.Path = "/"
	clonedURL.RawQuery = query.Encode()
	request.URL = &clonedURL
	request.RequestURI = ""
	request.Header = r.Header.Clone()
	if body == nil {
		request.Body = http.NoBody
		request.ContentLength = 0
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		request.Body = io.NopCloser(bytes.NewReader(encoded))
		request.ContentLength = int64(len(encoded))
		request.Header.Set("Content-Type", "application/json")
	}
	if id != "" {
		request.SetPathValue("id", id)
	}
	recorder := &mcpHTTPRecorder{header: make(http.Header)}
	handler(recorder, request)
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	if recorder.body.Len() == 0 {
		return map[string]any{"status": recorder.status}, recorder.status, nil
	}
	var payload any
	if err := json.Unmarshal(recorder.body.Bytes(), &payload); err != nil {
		return nil, recorder.status, err
	}
	return payload, recorder.status, nil
}

func decodeMCPToolArguments(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return strictUnmarshal(raw, dst)
}

func (s *Server) callMCPTool(r *http.Request, raw json.RawMessage) (any, *jsonRPCError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      json.RawMessage `json:"_meta"`
	}
	if err := strictUnmarshal(raw, &call); err != nil || call.Name == "" {
		return nil, &jsonRPCError{Code: -32602, Message: "Invalid params"}
	}
	p, _ := principalFrom(r)
	toolResult := func(value any) any {
		encoded, _ := json.Marshal(value)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": string(encoded)}}, "structuredContent": value, "resultType": "complete"}
	}
	toolError := func(message string) any {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": message}}, "isError": true, "resultType": "complete"}
	}
	toolHTTP := func(handler http.HandlerFunc, method, id string, query url.Values, body any) (any, *jsonRPCError) {
		payload, status, err := s.callMCPHTTPHandler(r, handler, method, id, query, body)
		if err != nil {
			return toolError("internal handler response could not be decoded"), nil
		}
		if status < 200 || status >= 300 {
			encoded, _ := json.Marshal(payload)
			return map[string]any{"content": []map[string]any{{"type": "text", "text": string(encoded)}}, "structuredContent": payload, "isError": true, "resultType": "complete"}, nil
		}
		return toolResult(payload), nil
	}
	switch call.Name {
	case "releasedock_list_applications":
		if !p.Has("applications.read") {
			return toolError("permission denied"), nil
		}
		var args struct {
			Limit  int    `json:"limit"`
			Search string `json:"search"`
		}
		if decodeMCPToolArguments(call.Arguments, &args) != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "Invalid tool arguments"}
		}
		if args.Limit < 1 {
			args.Limit = 50
		}
		query := url.Values{"limit": []string{strconv.Itoa(min(args.Limit, 100))}, "activeOnly": []string{"true"}}
		if args.Search != "" {
			query.Set("search", args.Search)
		}
		return toolHTTP(s.listApplications, http.MethodGet, "", query, nil)
	case "releasedock_list_environments":
		if !p.Has("applications.read") {
			return toolError("permission denied"), nil
		}
		var args struct {
			ApplicationID string `json:"applicationId"`
			Limit         int    `json:"limit"`
		}
		if decodeMCPToolArguments(call.Arguments, &args) != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "Invalid tool arguments"}
		}
		if args.Limit < 1 {
			args.Limit = 50
		}
		query := url.Values{"limit": []string{strconv.Itoa(min(args.Limit, 100))}, "activeOnly": []string{"true"}}
		if args.ApplicationID != "" {
			return toolHTTP(s.listEnvironments, http.MethodGet, args.ApplicationID, query, nil)
		}
		return toolHTTP(s.listAllEnvironments, http.MethodGet, "", query, nil)
	case "releasedock_list_profiles":
		if !p.Has("profiles.read") {
			return toolError("permission denied"), nil
		}
		var args struct {
			ApplicationID string `json:"applicationId"`
			EnvironmentID string `json:"environmentId"`
			Limit         int    `json:"limit"`
		}
		if decodeMCPToolArguments(call.Arguments, &args) != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "Invalid tool arguments"}
		}
		if args.Limit < 1 {
			args.Limit = 50
		}
		query := url.Values{"limit": []string{strconv.Itoa(min(args.Limit, 100))}, "activeOnly": []string{"true"}}
		if args.ApplicationID != "" {
			query.Set("applicationId", args.ApplicationID)
		}
		if args.EnvironmentID != "" {
			query.Set("environmentId", args.EnvironmentID)
		}
		return toolHTTP(s.listProfiles, http.MethodGet, "", query, nil)
	case "releasedock_list_releases":
		if !p.Has("releases.read") {
			return toolError("permission denied"), nil
		}
		var args struct {
			Limit  int    `json:"limit"`
			Status string `json:"status"`
			Search string `json:"search"`
		}
		if len(call.Arguments) > 0 && strictUnmarshal(call.Arguments, &args) != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "Invalid tool arguments"}
		}
		if args.Limit < 1 {
			args.Limit = 20
		}
		if args.Limit > 100 {
			args.Limit = 100
		}
		items, err := s.releaseRows(r, args.Limit, 0, args.Status, args.Search)
		if err != nil {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "database query failed"}}, "isError": true, "resultType": "complete"}, nil
		}
		return toolResult(map[string]any{"items": items}), nil
	case "releasedock_get_release":
		if !p.Has("releases.read") {
			return toolError("permission denied"), nil
		}
		var args struct {
			ID string `json:"id"`
		}
		if strictUnmarshal(call.Arguments, &args) != nil || args.ID == "" {
			return nil, &jsonRPCError{Code: -32602, Message: "id is required"}
		}
		item, err := scanRelease(s.store.Pool.QueryRow(r.Context(), releaseSelect+` WHERE r.id=$1`, args.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "release not found"}}, "isError": true, "resultType": "complete"}, nil
		}
		if err != nil {
			return nil, &jsonRPCError{Code: -32603, Message: "Internal error"}
		}
		steps, _ := s.loadReleaseSteps(r.Context(), args.ID)
		images, _ := s.loadReleaseImages(r.Context(), args.ID)
		item["steps"] = steps
		item["images"] = images
		return toolResult(item), nil
	case "releasedock_dashboard":
		if !p.Has("releases.read") {
			return toolError("permission denied"), nil
		}
		var args struct{}
		if decodeMCPToolArguments(call.Arguments, &args) != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "Invalid tool arguments"}
		}
		return toolHTTP(s.dashboard, http.MethodGet, "", url.Values{}, nil)
	case "releasedock_create_release":
		if !p.Has("releases.create") {
			return toolError("permission denied"), nil
		}
		var args releaseInput
		if strictUnmarshal(call.Arguments, &args) != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "Invalid tool arguments"}
		}
		if err := validateReleaseInput(args); err != nil {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true, "resultType": "complete"}, nil
		}
		id, err := s.insertRelease(r, args)
		if err != nil {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true, "resultType": "complete"}, nil
		}
		return toolResult(map[string]any{
			"id": id, "status": "DRAFT",
			"artifactUpload": map[string]any{"method": http.MethodPost, "path": "/api/v1/releases/" + id + "/artifacts/upload", "contentType": "multipart/form-data", "field": "artifact"},
		}), nil
	case "releasedock_enqueue_release", "releasedock_retry_release":
		if !p.Has("releases.submit") {
			return toolError("permission denied"), nil
		}
		var args struct {
			ID string `json:"id"`
		}
		if decodeMCPToolArguments(call.Arguments, &args) != nil || !validUUID(args.ID) {
			return nil, &jsonRPCError{Code: -32602, Message: "valid id is required"}
		}
		handler := s.enqueueRelease
		if call.Name == "releasedock_retry_release" {
			handler = s.retryRelease
		}
		return toolHTTP(handler, http.MethodPost, args.ID, url.Values{}, nil)
	case "releasedock_review_release", "releasedock_approve_release", "releasedock_reject_release":
		permission := map[string]string{
			"releasedock_review_release":  "releases.review",
			"releasedock_approve_release": "releases.approve",
			"releasedock_reject_release":  "releases.reject",
		}[call.Name]
		if !p.Has(permission) {
			return toolError("permission denied"), nil
		}
		var args struct {
			ID      string `json:"id"`
			Comment string `json:"comment"`
		}
		if decodeMCPToolArguments(call.Arguments, &args) != nil || !validUUID(args.ID) {
			return nil, &jsonRPCError{Code: -32602, Message: "valid id is required"}
		}
		handler := s.reviewRelease
		if call.Name == "releasedock_approve_release" {
			handler = s.approveRelease
		} else if call.Name == "releasedock_reject_release" {
			handler = s.rejectRelease
		}
		return toolHTTP(handler, http.MethodPost, args.ID, url.Values{}, decisionInput{Comment: args.Comment})
	case "releasedock_rollback_release":
		if !p.Has("releases.submit") {
			return toolError("permission denied"), nil
		}
		var args struct {
			ID string `json:"id"`
		}
		if decodeMCPToolArguments(call.Arguments, &args) != nil || !validUUID(args.ID) {
			return nil, &jsonRPCError{Code: -32602, Message: "valid id is required"}
		}
		return toolHTTP(s.rollbackRelease, http.MethodPost, args.ID, url.Values{}, nil)
	case "releasedock_list_jobs":
		if !p.Has("releases.read") {
			return toolError("permission denied"), nil
		}
		var args struct {
			Limit int `json:"limit"`
		}
		if decodeMCPToolArguments(call.Arguments, &args) != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "Invalid tool arguments"}
		}
		if args.Limit < 1 {
			args.Limit = 50
		}
		return toolHTTP(s.listJobs, http.MethodGet, "", url.Values{"limit": []string{strconv.Itoa(min(args.Limit, 100))}}, nil)
	case "releasedock_get_release_logs":
		if !p.Has("releases.read") {
			return toolError("permission denied"), nil
		}
		var args struct {
			ReleaseID string `json:"releaseId"`
			After     int64  `json:"after"`
			Limit     int    `json:"limit"`
		}
		if decodeMCPToolArguments(call.Arguments, &args) != nil || !validUUID(args.ReleaseID) || args.After < 0 {
			return nil, &jsonRPCError{Code: -32602, Message: "valid releaseId and cursor are required"}
		}
		if args.Limit < 1 {
			args.Limit = 200
		}
		args.Limit = min(args.Limit, 500)
		rows, err := s.store.Pool.Query(r.Context(), `SELECT l.id,j.id::text,l.step_id,l.stream,l.sequence,l.payload,l.created_at
			FROM release_job_logs l JOIN release_jobs j ON j.id=l.job_id
			WHERE j.release_id=$1 AND l.id>$2 ORDER BY l.id LIMIT $3`, args.ReleaseID, args.After, args.Limit)
		if err != nil {
			return toolError("database query failed"), nil
		}
		defer rows.Close()
		items := []map[string]any{}
		nextAfter := args.After
		for rows.Next() {
			var id, stepID, sequence int64
			var jobID, stream string
			var payload []byte
			var created time.Time
			if err := rows.Scan(&id, &jobID, &stepID, &stream, &sequence, &payload, &created); err != nil {
				return toolError("database query failed"), nil
			}
			nextAfter = id
			items = append(items, map[string]any{"id": id, "jobId": jobID, "stepId": stepID, "stream": stream, "sequence": sequence, "message": string(payload), "createdAt": created})
		}
		if err := rows.Err(); err != nil {
			return toolError("database query failed"), nil
		}
		return toolResult(map[string]any{"items": items, "nextAfter": nextAfter}), nil
	default:
		return nil, &jsonRPCError{Code: -32602, Message: "Unknown tool"}
	}
}

func strictUnmarshal(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return err
	}
	return nil
}
