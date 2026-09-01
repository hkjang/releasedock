package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestReplicationTerminalMapsHarborStates(t *testing.T) {
	cases := []struct {
		status   string
		done, ok bool
	}{
		{"Succeed", true, true},
		{"succeeded", true, true},
		{"Success", true, true},
		{"Failed", true, false},
		{"Error", true, false},
		// A stopped execution did not replicate, so it must not read as success.
		{"Stopped", true, false},
		{"InProgress", false, false},
		{"Running", false, false},
		{"", false, false},
	}
	for _, testCase := range cases {
		done, ok := replicationTerminal(testCase.status)
		if done != testCase.done || ok != testCase.ok {
			t.Fatalf("%q: got done=%v ok=%v, want done=%v ok=%v", testCase.status, done, ok, testCase.done, testCase.ok)
		}
	}
}

func TestHarborURLRejectsUnusableEndpoints(t *testing.T) {
	got, err := harborURL("https://harbor.company.local/", "/api/v2.0/replication/policies")
	if err != nil || got != "https://harbor.company.local/api/v2.0/replication/policies" {
		t.Fatalf("unexpected url %q err=%v", got, err)
	}
	// A path or a port is fine; a missing scheme or host is not.
	if _, err := harborURL("harbor.company.local", "/x"); err == nil {
		t.Fatal("expected a schemeless endpoint to be rejected")
	}
	if _, err := harborURL("ftp://harbor.company.local", "/x"); err == nil {
		t.Fatal("expected a non-http scheme to be rejected")
	}
	if _, err := harborURL("", "/x"); err == nil {
		t.Fatal("expected an empty endpoint to be rejected")
	}
}

// The operator needs to know which URL failed and whether the account or the
// endpoint is at fault.
func TestHarborErrorExplainsTheCommonFailures(t *testing.T) {
	unauthorized := harborError("복제 실행", "https://harbor.local/api/v2.0/replication/executions", &http.Response{StatusCode: 401, Body: http.NoBody})
	if !strings.Contains(unauthorized.Error(), "Robot") {
		t.Fatalf("401 should point at the robot account: %v", unauthorized)
	}
	notFound := harborError("복제 실행", "https://harbor.local/api/v2.0/replication/executions", &http.Response{StatusCode: 404, Body: http.NoBody})
	if !strings.Contains(notFound.Error(), "Harbor API 경로") {
		t.Fatalf("404 should point at the API path: %v", notFound)
	}
	server := harborError("복제 실행", "https://harbor.local/api/v2.0/replication/executions", &http.Response{StatusCode: 500, Body: http.NoBody})
	if !strings.Contains(server.Error(), "500") {
		t.Fatalf("other statuses should be reported verbatim: %v", server)
	}
}

// Every failure message must name the URL that was actually called; a wrong
// endpoint is the most common cause and is invisible without it.
func TestHarborErrorAlwaysReportsTheRequestedURL(t *testing.T) {
	const requestURL = "https://harbor.local/api/v2.0/replication/policies"
	for _, status := range []int{401, 403, 404, 500, 502} {
		err := harborError("복제 규칙 조회", requestURL, &http.Response{StatusCode: status, Body: http.NoBody})
		if !strings.Contains(err.Error(), requestURL) {
			t.Fatalf("HTTP %d message omits the URL: %v", status, err)
		}
	}
}
