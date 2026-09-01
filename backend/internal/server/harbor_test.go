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

// The operator needs to know whether the robot account or the rule is at fault.
func TestHarborErrorExplainsTheCommonFailures(t *testing.T) {
	unauthorized := harborError("복제 실행", &http.Response{StatusCode: 401, Body: http.NoBody})
	if !strings.Contains(unauthorized.Error(), "Robot") {
		t.Fatalf("401 should point at the robot account: %v", unauthorized)
	}
	notFound := harborError("복제 실행", &http.Response{StatusCode: 404, Body: http.NoBody})
	if !strings.Contains(notFound.Error(), "복제 규칙") {
		t.Fatalf("404 should point at the rule id: %v", notFound)
	}
	server := harborError("복제 실행", &http.Response{StatusCode: 500, Body: http.NoBody})
	if !strings.Contains(server.Error(), "500") {
		t.Fatalf("other statuses should be reported verbatim: %v", server)
	}
}
