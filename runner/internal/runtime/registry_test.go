package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegistryDigest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/v2/crm/api/manifests/2.4.1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "robot" || password != "secret" {
			t.Error("missing registry credentials")
		}
		w.Header().Set("Docker-Content-Digest", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := NewRegistryClient(server.URL, "", false, Credential{Username: "robot", Password: "secret"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := client.Digest(context.Background(), "crm/api", "2.4.1")
	if err != nil {
		t.Fatal(err)
	}
	if digest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("unexpected digest: %q", digest)
	}
}

func TestRegistryDigestExchangesBearerChallenge(t *testing.T) {
	const expectedToken = "harbor-access-token"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/service/token":
			username, password, ok := r.BasicAuth()
			if !ok || username != "robot" || password != "secret" {
				t.Error("missing token service credentials")
			}
			if r.URL.Query().Get("service") != "harbor-registry" || r.URL.Query().Get("scope") != "repository:crm/api:pull" {
				t.Errorf("unexpected token query: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"` + expectedToken + `"}`))
		case "/v2/crm/api/manifests/2.4.1":
			if r.Header.Get("Authorization") != "Bearer "+expectedToken {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/service/token",service="harbor-registry",scope="repository:crm/api:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewRegistryClient(server.URL, "", false, Credential{Username: "robot", Password: "secret"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := client.Digest(context.Background(), "crm/api", "2.4.1")
	if err != nil {
		t.Fatal(err)
	}
	if digest != "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("unexpected digest: %q", digest)
	}
}
