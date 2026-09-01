package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hkjang/releasedock/backend/internal/store"
)

// Run history is operational data about other people's deployments, so only a
// simple-mode administrator may look past their own runs.
func TestCanReadEveryRunRequiresSimpleAdministration(t *testing.T) {
	cases := []struct {
		name        string
		permissions []string
		want        bool
	}{
		{"deployer only", []string{"simple.deploy", "simple.read"}, false},
		{"reader only", []string{"simple.read"}, false},
		{"simple administrator", []string{"simple.read", "admin.simple.read"}, true},
		{"unrelated admin permission", []string{"simple.read", "admin.users.read"}, false},
		{"nothing", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "/api/v1/simple/runs", nil)
			principal := store.Principal{UserID: "user-1", Permissions: testCase.permissions}
			request = request.WithContext(context.WithValue(request.Context(), principalKey, principal))
			if got := canReadEveryRun(request); got != testCase.want {
				t.Fatalf("canReadEveryRun = %v, want %v", got, testCase.want)
			}
		})
	}
}

// An API key narrows what its owner can do, so a key without the permission
// must not inherit the owner's administration rights.
func TestCanReadEveryRunRespectsAPIKeyScope(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v1/simple/runs", nil)
	principal := store.Principal{
		UserID:      "user-1",
		Permissions: []string{"simple.read", "admin.simple.read"},
		ViaAPIKey:   true,
		Scopes:      []string{"simple.read"},
	}
	request = request.WithContext(context.WithValue(request.Context(), principalKey, principal))
	if canReadEveryRun(request) {
		t.Fatal("an API key scoped to simple.read must not read other people's runs")
	}
}
