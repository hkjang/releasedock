package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hkjang/releasedock/runner/internal/model"
)

const maxHealthBodyBytes = 1 << 20

type Checker struct{}

func (Checker) Check(ctx context.Context, check model.HealthCheck) error {
	if err := validate(check); err != nil {
		return err
	}
	var lastErr error
	for attempt := 1; attempt <= check.Attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
		switch strings.ToLower(check.Type) {
		case "http", "https":
			lastErr = checkHTTP(attemptCtx, check)
		case "tcp":
			lastErr = checkTCP(attemptCtx, check)
		}
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt == check.Attempts {
			break
		}
		timer := time.NewTimer(time.Duration(check.IntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("health check failed after %d attempt(s): %w", check.Attempts, lastErr)
}

func validate(check model.HealthCheck) error {
	kind := strings.ToLower(check.Type)
	if kind != "http" && kind != "https" && kind != "tcp" {
		return fmt.Errorf("unsupported health check type %q", check.Type)
	}
	if check.Address == "" {
		return errors.New("health check address is required")
	}
	if check.TimeoutSeconds <= 0 || check.Attempts <= 0 || check.IntervalSeconds < 0 {
		return errors.New("health timeout/attempts must be positive and interval non-negative")
	}
	if kind != "tcp" {
		method := strings.ToUpper(check.Method)
		if method == "" {
			method = http.MethodGet
		}
		if method != http.MethodGet && method != http.MethodHead {
			return errors.New("HTTP health check method must be GET or HEAD")
		}
		if check.ExpectedStatusMin < 100 || check.ExpectedStatusMax > 599 || check.ExpectedStatusMin > check.ExpectedStatusMax {
			return errors.New("invalid expected HTTP status range")
		}
		for key, value := range check.Headers {
			if !validHeader(key, value) {
				return fmt.Errorf("invalid HTTP header %q", key)
			}
		}
	}
	return nil
}

func checkHTTP(ctx context.Context, check model.HealthCheck) error {
	method := strings.ToUpper(check.Method)
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, check.Address, nil)
	if err != nil {
		return fmt.Errorf("create HTTP request: %w", err)
	}
	for key, value := range check.Headers {
		req.Header.Set(key, value)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: check.InsecureSkipVerify} // #nosec G402 -- explicit admin policy.
	if check.CAPEM != "" {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(check.CAPEM)) {
			return errors.New("health check CA PEM contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	transport := &http.Transport{
		Proxy:             nil,
		TLSClientConfig:   tlsConfig,
		DisableKeepAlives: true,
		DialContext:       (&net.Dialer{}).DialContext,
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < check.ExpectedStatusMin || resp.StatusCode > check.ExpectedStatusMax {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHealthBodyBytes))
		return fmt.Errorf("HTTP status %d outside expected %d-%d", resp.StatusCode, check.ExpectedStatusMin, check.ExpectedStatusMax)
	}
	if check.ExpectedBody != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBodyBytes+1))
		if err != nil {
			return fmt.Errorf("read HTTP body: %w", err)
		}
		if len(body) > maxHealthBodyBytes {
			return errors.New("HTTP health response exceeds 1 MiB")
		}
		if !strings.Contains(string(body), check.ExpectedBody) {
			return errors.New("HTTP response does not contain expected text")
		}
	}
	return nil
}

func checkTCP(ctx context.Context, check model.HealthCheck) error {
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", check.Address)
	if err != nil {
		return fmt.Errorf("TCP connect: %w", err)
	}
	return connection.Close()
}

func validHeader(key, value string) bool {
	if key == "" || strings.ContainsAny(key, "\r\n:") || strings.ContainsAny(value, "\r\n") {
		return false
	}
	return true
}
