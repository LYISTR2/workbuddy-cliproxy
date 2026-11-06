//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/lovingfish/workbuddy-cliproxy/internal/catalog"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRuntimeDiscoversAndExecutesThroughVersionCheckingGateway(t *testing.T) {
	setupRuntimeCatalog(t)
	dynamicCatalog.manager = nil // Exercise the production HTTP fetcher wiring.
	var catalogCalls, chatCalls int
	version := regexp.MustCompile(`^WorkBuddy/(\d+\.\d+\.\d+) `)
	mockWorkbuddyHTTP(t, func(req *http.Request) (int, string) {
		match := version.FindStringSubmatch(req.UserAgent())
		if len(match) == 0 || req.Header.Get("X-IDE-Version") != match[1] || req.Header.Get("X-IDE-Name") != "WorkBuddy" || req.Header.Get("X-Domain") == "" {
			return 400, `{"code":12403}`
		}
		if req.Header.Get("Authorization") != "Bearer TEST_TOKEN" || req.Header.Get("X-User-Id") != "TEST_UID" {
			t.Error("account context was lost")
		}
		switch req.URL.Path {
		case "/v3/config":
			catalogCalls++
			return 200, runtimeFixture
		case "/v2/chat/completions":
			chatCalls++
			return 200, "data: {\"choices\":[{\"delta\":{\"content\":\"fixture response\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		default:
			t.Errorf("unexpected request %s", req.URL.Path)
			return 404, `{}`
		}
	})
	if err := configureCatalog(nil); err != nil {
		t.Fatal(err)
	}
	storage, _ := json.Marshal(testAuth())
	request, _ := json.Marshal(pluginapi.AuthModelRequest{AuthID: "fixture", StorageJSON: storage})
	raw, err := handleCatalogForAuth(request)
	var models pluginapi.ModelResponse
	decodeAuthResult(t, raw, err, &models)
	if len(models.Models) != 1 || models.Models[0].ID != "hy3" {
		t.Fatal("models were not registered")
	}
	request, _ = json.Marshal(pluginapi.ExecutorRequest{AuthID: "fixture", Model: "hy3", StorageJSON: storage, Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)})
	raw, err = handleExecExecute(request)
	var result pluginapi.ExecutorResponse
	decodeAuthResult(t, raw, err, &result)
	if !strings.Contains(string(result.Payload), "fixture response") || catalogCalls != 1 || chatCalls != 1 {
		t.Fatalf("discovery/inference did not complete: catalog=%d chat=%d", catalogCalls, chatCalls)
	}
}

func TestDiscoveryRecoversUIDFromLegacyCredential(t *testing.T) {
	setupRuntimeCatalog(t)
	var accountCalls int
	mockWorkbuddyHTTP(t, func(req *http.Request) (int, string) {
		if req.URL.Path != "/v2/plugin/account" || req.Header.Get("Authorization") != "Bearer TEST_TOKEN" {
			t.Error("unexpected profile lookup")
		}
		accountCalls++
		return 200, `{"code":0,"data":{"uid":"recovered-user","enterpriseId":"recovered-enterprise"}}`
	})
	dynamicCatalog.manager = catalog.NewManager(func(_ context.Context, c catalog.Credentials) ([]byte, error) {
		if c.UID != "recovered-user" || c.EnterpriseID != "recovered-enterprise" {
			t.Error("recovered profile was not used for discovery")
		}
		return []byte(runtimeFixture), nil
	}, catalog.Options{})
	legacy := testAuth()
	legacy.Account.UID = ""
	storage, _ := json.Marshal(legacy)
	request, _ := json.Marshal(pluginapi.AuthModelRequest{AuthID: "fixture", StorageJSON: storage})
	raw, err := handleCatalogForAuth(request)
	var models pluginapi.ModelResponse
	decodeAuthResult(t, raw, err, &models)
	if len(models.Models) != 1 || accountCalls != 1 {
		t.Fatal("legacy credential could not discover models")
	}
}
