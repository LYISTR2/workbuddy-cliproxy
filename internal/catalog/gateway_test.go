package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestGatewayRejectsLegacyUAAndDiscoversWithClientProfile(t *testing.T) {
	versionPattern := regexp.MustCompile(`^WorkBuddy/(\d+\.\d+\.\d+) WorkBuddy/\d+\.\d+\.\d+ CLI/\d+\.\d+\.\d+$`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		match := versionPattern.FindStringSubmatch(r.UserAgent())
		if len(match) == 0 || r.Header.Get("X-IDE-Version") != match[1] || r.Header.Get("X-IDE-Name") != "WorkBuddy" || r.Header.Get("X-IDE-Type") != "WorkBuddy" || r.Header.Get("X-Domain") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":12403,"msg":"check ua, get coding copilot version error"}`))
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer server.Close()
	// Reproduce the old wire request independently of the fixed implementation.
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	request.Header.Set("User-Agent", "WorkBuddy/CLIProxyAPI")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("regression fixture did not reject the old client profile")
	}

	manager := NewManager(HTTPFetcher(server.Client(), server.URL), Options{})
	snapshot, err := manager.Get(context.Background(), Credentials{AccessToken: "fixture-token", UID: "fixture-user"})
	if err != nil || len(snapshot.Models) != 3 {
		t.Fatalf("discovery failed: models=%d error=%v", len(snapshot.Models), err)
	}
}

func TestGatewayErrorCodeIsPreservedWithoutRawMessage(t *testing.T) {
	for _, status := range []int{200, 400} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":12403,"msg":"fixture-private-message"}`))
		}))
		manager := NewManager(HTTPFetcher(server.Client(), server.URL), Options{})
		_, err := manager.Get(context.Background(), Credentials{AccessToken: "fixture-token", UID: "fixture-user"})
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "12403") || !strings.Contains(err.Error(), "version") || strings.Contains(err.Error(), "fixture-private-message") {
			t.Fatalf("wrong diagnostic for HTTP %d: %v", status, err)
		}
		if status == 400 {
			var upstream *HTTPError
			if !errors.As(err, &upstream) || upstream.Status != 400 || upstream.Code != 12403 {
				t.Fatal("HTTP error details were lost")
			}
		}
	}
}

func TestMissingModelsDoesNotBecomeSuccessfulEmptyCatalog(t *testing.T) {
	_, err := Parse([]byte(`{"code":0,"data":{"models":null,"agents":[{"name":"cli","models":[]}]}}`), "cli", time.Now())
	if err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("missing directory was treated as success: %v", err)
	}
}
