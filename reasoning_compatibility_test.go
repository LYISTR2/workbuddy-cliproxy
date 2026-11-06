//go:build cgo

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const deepseekReasoningFixture = `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash","supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]}}],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`

func TestDeepSeekMediumReachesGatewayAsAdvertisedHigh(t *testing.T) {
	setupRuntimeCatalog(t)
	dynamicCatalog.manager = nil
	var chatCalls int
	mockWorkbuddyHTTP(t, func(req *http.Request) (int, string) {
		switch req.URL.Path {
		case "/v3/config":
			return 200, deepseekReasoningFixture
		case "/v2/chat/completions":
			chatCalls++
			raw, _ := io.ReadAll(req.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if body["model"] != "deepseek-v4.1-flash" || body["reasoning_effort"] != "high" {
				t.Errorf("wrong upstream request: %s", raw)
			}
			return 200, "data: {\"choices\":[{\"delta\":{\"content\":\"reasoning compatibility works\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		default:
			t.Errorf("unexpected path: %s", req.URL.Path)
			return 404, `{}`
		}
	})
	if err := configureCatalog(nil); err != nil {
		t.Fatal(err)
	}
	storage, _ := json.Marshal(testAuth())
	request, _ := json.Marshal(pluginapi.ExecutorRequest{AuthID: "fixture", Model: "deepseek-v4.1-flash", StorageJSON: storage, Payload: []byte(`{"reasoning_effort":"medium","messages":[{"role":"user","content":"hello"}]}`)})
	raw, err := handleExecExecute(request)
	var result pluginapi.ExecutorResponse
	decodeAuthResult(t, raw, err, &result)
	if chatCalls != 1 || !strings.Contains(string(result.Payload), "reasoning compatibility works") {
		t.Fatal("request did not complete")
	}
}

func TestReasoningPolicyConfigSupportsStrictAndRejectsInvalid(t *testing.T) {
	setupRuntimeCatalog(t)
	configure := func(policy string) error {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("catalog:\n  reasoning-effort-policy: " + policy + "\n")})
		return configureCatalog(raw)
	}
	request := pluginapi.ExecutorRequest{AuthID: "fixture", Model: "hy3", Payload: []byte(`{"reasoning_effort":"medium"}`)}
	if err := configure("strict"); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareUpstreamPayload(request, testAuth()); err == nil {
		t.Fatal("strict runtime policy was ignored")
	}
	if err := configure("compatible"); err != nil {
		t.Fatal(err)
	}
	if err := configure("invalid"); err == nil {
		t.Fatal("invalid policy was accepted")
	}
	raw, err := prepareUpstreamPayload(request, testAuth())
	if err != nil || !strings.Contains(string(raw), `"reasoning_effort":"high"`) {
		t.Fatalf("valid policy was not retained: %s %v", raw, err)
	}
}
