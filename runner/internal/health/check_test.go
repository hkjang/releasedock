package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hkjang/releasedock/runner/internal/model"
)

func TestHTTPHealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"UP"}`))
	}))
	defer server.Close()
	err := (Checker{}).Check(context.Background(), model.HealthCheck{
		Type: "http", Address: server.URL, Method: "GET",
		ExpectedStatusMin: 200, ExpectedStatusMax: 299,
		ExpectedBody: "UP", TimeoutSeconds: 1, Attempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTCPHealthCheck(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		connection, _ := listener.Accept()
		if connection != nil {
			_ = connection.Close()
		}
		close(done)
	}()
	err = (Checker{}).Check(context.Background(), model.HealthCheck{
		Type: "tcp", Address: listener.Addr().String(), TimeoutSeconds: 1, Attempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestHTTPHealthCheckRejectsHeaderInjection(t *testing.T) {
	err := (Checker{}).Check(context.Background(), model.HealthCheck{
		Type: "http", Address: "http://127.0.0.1", Method: "GET",
		Headers:           map[string]string{"X-Test": "ok\r\nevil: yes"},
		ExpectedStatusMin: 200, ExpectedStatusMax: 299,
		TimeoutSeconds: 1, Attempts: 1,
	})
	if err == nil {
		t.Fatal("expected invalid header error")
	}
}
