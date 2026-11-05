//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lovingfish/workbuddy-cliproxy/internal/catalog"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCatalogAuthFailurePreservesHTTPStatus(t *testing.T) {
	setupRuntimeCatalog(t)
	for _, status := range []int{401, 403} {
		dynamicCatalog.manager = catalog.NewManager(func(context.Context, catalog.Credentials) ([]byte, error) {
			return nil, &catalog.HTTPError{Status: status}
		}, catalog.Options{})
		_, err := prepareUpstreamPayload(pluginapi.ExecutorRequest{AuthID: "test", Model: "hy3", Payload: []byte(`{}`)}, testAuth())
		var requestErr *requestError
		if !errors.As(err, &requestErr) || requestErr.StatusCode() != status {
			t.Fatalf("status=%d, error=%v", status, err)
		}
	}
}

func TestCatalogInterceptorClearsOriginalBodyHeaders(t *testing.T) {
	setupRuntimeCatalog(t)
	_, _ = accountCatalog("test", testAuth())
	request := pluginapi.ResponseInterceptRequest{StatusCode: http.StatusOK, ResponseHeaders: http.Header{"Content-Length": {"42"}, "Etag": {"old"}, "Content-Type": {"application/json"}}, Body: []byte(`{"object":"list","data":[{"id":"hy3","object":"model","owned_by":"workbuddy"}]}`)}
	raw, _ := json.Marshal(request)
	result, err := handleCatalogResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(result, &env)
	var response pluginapi.ResponseInterceptResponse
	_ = json.Unmarshal(env.Result, &response)
	if len(response.ClearHeaders) != 2 || response.Headers.Get("Content-Length") != "" || response.Headers.Get("ETag") != "" || response.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("header invalidation missing from ABI response: %s", env.Result)
	}
}

func TestAggregateRejectsErrorAndEmptyStreams(t *testing.T) {
	for _, stream := range []string{"", "data: [DONE]\n\n", "data: {\"error\":{\"message\":\"unavailable\"}}\n\n"} {
		if _, err := aggregateCompletion(strings.NewReader(stream), "test-model"); err == nil {
			t.Fatalf("returned a successful completion for stream %q", stream)
		}
	}
}
