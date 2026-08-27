package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type publicHTTPConfig struct {
	PublicURL      string
	SecureCookies  bool
	AllowedOrigins []string
}

func normalizeOrigin(value string, httpsOnly bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("origin must contain only an http(s) scheme and host")
	}
	if httpsOnly {
		if parsed.Scheme != "https" {
			return "", errors.New("origin must use HTTPS")
		}
	} else if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("origin must use HTTP or HTTPS")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func (s *Server) loadPublicHTTPConfig(ctx context.Context) (publicHTTPConfig, error) {
	var cfg publicHTTPConfig
	var publicURL string
	err := s.store.Pool.QueryRow(ctx, `SELECT COALESCE(general_config->>'publicUrl',''),
		CASE WHEN lower(COALESCE(general_config->>'secureCookies','false'))='true' THEN TRUE ELSE FALSE END,
		allowed_origins FROM app_settings WHERE id='default'`).Scan(&publicURL, &cfg.SecureCookies, &cfg.AllowedOrigins)
	if err != nil {
		return publicHTTPConfig{}, err
	}
	if strings.TrimSpace(publicURL) != "" {
		cfg.PublicURL, err = normalizeOrigin(publicURL, true)
		if err != nil {
			return publicHTTPConfig{}, err
		}
	}
	for index, origin := range cfg.AllowedOrigins {
		cfg.AllowedOrigins[index], err = normalizeOrigin(origin, false)
		if err != nil {
			return publicHTTPConfig{}, err
		}
	}
	return cfg, nil
}

func (s *Server) useSecureCookies(ctx context.Context, r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	cfg, err := s.loadPublicHTTPConfig(ctx)
	if err != nil {
		return true
	}
	return cfg.SecureCookies || strings.HasPrefix(cfg.PublicURL, "https://")
}

func (s *Server) configuredPublicOrigin(ctx context.Context) (string, error) {
	cfg, err := s.loadPublicHTTPConfig(ctx)
	if err != nil {
		return "", err
	}
	if cfg.PublicURL == "" {
		return "", errors.New("general publicUrl must be configured")
	}
	return cfg.PublicURL, nil
}

func loopbackHost(hostport string) bool {
	host := hostport
	if parsedHost, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}
