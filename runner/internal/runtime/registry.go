package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type RegistryClient struct {
	baseURL    *url.URL
	httpClient *http.Client
	credential Credential
}

func NewRegistryClient(endpoint, caPEM string, insecure bool, credential Credential, timeout time.Duration) (*RegistryClient, error) {
	baseURL, err := url.Parse(endpoint)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" || baseURL.User != nil {
		return nil, errors.New("registry_url must be an absolute HTTP(S) URL without user info")
	}
	if timeout <= 0 {
		return nil, errors.New("registry timeout must be positive")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} // #nosec G402 -- explicit administrator profile option.
	if caPEM != "" {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, errors.New("registry CA PEM contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig}
	return &RegistryClient{
		baseURL: baseURL, credential: credential,
		httpClient: &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

func (c *RegistryClient) Digest(ctx context.Context, repository, tag string) (string, error) {
	segments := strings.Split(strings.Trim(repository, "/"), "/")
	if len(segments) == 0 || tag == "" {
		return "", errors.New("repository and tag are required")
	}
	for i, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("invalid registry repository")
		}
		segments[i] = url.PathEscape(segment)
	}
	requestURL := *c.baseURL
	requestURL.Path = strings.TrimRight(c.baseURL.Path, "/") + "/v2/" + strings.Join(segments, "/") + "/manifests/" + url.PathEscape(tag)
	req, err := c.digestRequest(ctx, requestURL.String(), "")
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("query registry digest: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		token, tokenErr := c.exchangeBearerToken(ctx, challenge)
		if tokenErr != nil {
			return "", fmt.Errorf("query registry digest: %w", tokenErr)
		}
		req, err = c.digestRequest(ctx, requestURL.String(), token)
		if err != nil {
			return "", err
		}
		resp, err = c.httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("query registry digest with bearer token: %w", err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("query registry digest: HTTP %d", resp.StatusCode)
	}
	digest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	encoded := strings.TrimPrefix(digest, "sha256:")
	if !strings.HasPrefix(digest, "sha256:") || len(encoded) != 64 {
		return "", fmt.Errorf("registry returned invalid Docker-Content-Digest %q", digest)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", fmt.Errorf("registry returned invalid Docker-Content-Digest %q", digest)
	}
	return digest, nil
}

func (c *RegistryClient) digestRequest(ctx context.Context, endpoint, bearerToken string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	} else if c.credential.Username != "" {
		req.SetBasicAuth(c.credential.Username, c.credential.Password)
	}
	return req, nil
}

func (c *RegistryClient) exchangeBearerToken(ctx context.Context, challenge string) (string, error) {
	parameters, err := parseBearerChallenge(challenge)
	if err != nil {
		return "", err
	}
	realm, err := url.Parse(parameters["realm"])
	if err != nil || (realm.Scheme != "http" && realm.Scheme != "https") || realm.Host == "" || realm.User != nil || realm.Fragment != "" {
		return "", errors.New("registry Bearer challenge contains an invalid realm")
	}
	if c.baseURL.Scheme == "https" && realm.Scheme != "https" {
		return "", errors.New("registry Bearer realm cannot downgrade HTTPS")
	}
	query := realm.Query()
	for _, key := range []string{"service", "scope"} {
		if value := parameters[key]; value != "" {
			query.Set(key, value)
		}
	}
	realm.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if c.credential.Username != "" {
		req.SetBasicAuth(c.credential.Username, c.credential.Password)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("registry token request: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode registry token: %w", err)
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" || len(token) > 64<<10 || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("registry token response is invalid")
	}
	return token, nil
}

func parseBearerChallenge(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	space := strings.IndexAny(value, " \t")
	if space < 0 || !strings.EqualFold(value[:space], "Bearer") {
		return nil, errors.New("registry did not return a Bearer authentication challenge")
	}
	input := strings.TrimSpace(value[space+1:])
	parameters := make(map[string]string)
	for len(input) > 0 {
		input = strings.TrimLeft(input, " \t,")
		if input == "" {
			break
		}
		equals := strings.IndexByte(input, '=')
		if equals <= 0 {
			return nil, errors.New("registry Bearer challenge is malformed")
		}
		key := strings.ToLower(strings.TrimSpace(input[:equals]))
		input = strings.TrimSpace(input[equals+1:])
		if key == "" || input == "" {
			return nil, errors.New("registry Bearer challenge is malformed")
		}
		var parsed string
		if input[0] == '"' {
			input = input[1:]
			var builder strings.Builder
			closed := false
			for index := 0; index < len(input); index++ {
				if input[index] == '\\' && index+1 < len(input) {
					index++
					builder.WriteByte(input[index])
					continue
				}
				if input[index] == '"' {
					parsed = builder.String()
					input = input[index+1:]
					closed = true
					break
				}
				builder.WriteByte(input[index])
			}
			if !closed {
				return nil, errors.New("registry Bearer challenge has an unterminated value")
			}
		} else {
			comma := strings.IndexByte(input, ',')
			if comma < 0 {
				parsed, input = strings.TrimSpace(input), ""
			} else {
				parsed, input = strings.TrimSpace(input[:comma]), input[comma+1:]
			}
		}
		parameters[key] = parsed
	}
	if parameters["realm"] == "" {
		return nil, errors.New("registry Bearer challenge has no realm")
	}
	return parameters, nil
}
