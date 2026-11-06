//go:build cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestMultiAccountAuthIdentityIsUnique(t *testing.T) {
	a, b := testAuth(), testAuth()
	a.Account.UID = "a/../../user"
	b.Account.UID = "a____user"
	aa, bb := toAuthData(a), toAuthData(b)
	if aa.ID == bb.ID || aa.FileName == bb.FileName || filepath.Base(aa.FileName) != aa.FileName {
		t.Fatal("account collision or unsafe filename")
	}
	a.Auth.AccessToken = "rotated"
	if id, _ := authIdentity(a); id != aa.ID {
		t.Fatal("rotation changed identity")
	}
	a.Account.EnterpriseID = "enterprise"
	if id, _ := authIdentity(a); id == aa.ID {
		t.Fatal("tenant identities collided")
	}
	legacy := testAuth()
	legacy.Account.UID = ""
	if id, file := authIdentity(legacy); id != providerName || file != authFileName {
		t.Fatal("legacy fallback lost")
	}
	for _, req := range []pluginapi.AuthParseRequest{{FileName: "workbuddy.json"}, {Path: "/tmp/auths/workbuddy.json"}} {
		req.RawJSON, _ = json.Marshal(a)
		raw, _ := json.Marshal(req)
		out, err := handleParseAuth(raw)
		var parsed pluginapi.AuthParseResponse
		decodeAuthResult(t, out, err, &parsed)
		if !parsed.Handled || parsed.Auth.FileName != "workbuddy.json" || parsed.Auth.ID != "" {
			t.Fatal("legacy path was replaced")
		}
	}
	if aa.ID != aa.FileName {
		t.Fatal("login and watcher identities differ")
	}
}
func TestAuthExpiryAndRefreshScheduling(t *testing.T) {
	a := testAuth()
	a.Auth.ExpiresAt = time.Now().Add(time.Hour).Unix()
	a.Auth.RefreshToken = "refresh-secret"
	auth := toAuthData(a)
	expiry, err := time.Parse(time.RFC3339, auth.Metadata["expires_at"].(string))
	if err != nil || auth.NextRefreshAfter.Sub(expiry) != -5*time.Minute || auth.Metadata["refresh_interval_seconds"] != 300 {
		t.Fatal("missing host refresh schedule")
	}
	for _, seconds := range []int64{0, -1} {
		if delta := time.Until(time.Unix(tokenExpiry(seconds), 0)); delta < 59*time.Minute || delta > time.Hour {
			t.Fatal("bad fallback expiry")
		}
	}
	mockWorkbuddyHTTP(t, func(r *http.Request) (int, string) {
		if r.URL.Path == "/v2/plugin/account" {
			return 503, `{}`
		}
		return 200, `{"code":0,"data":{"accessToken":"rotated","expiresIn":0}}`
	})
	storage, _ := json.Marshal(a)
	req, _ := json.Marshal(pluginapi.AuthRefreshRequest{AuthID: "workbuddy.json", StorageJSON: storage})
	raw, err := handleRefreshAuth(req)
	var response pluginapi.AuthRefreshResponse
	decodeAuthResult(t, raw, err, &response)
	if response.Auth.ID != "workbuddy.json" || response.Auth.FileName != "" || response.NextRefreshAfter.IsZero() || response.NextRefreshAfter.Before(time.Now().Add(50*time.Minute)) {
		t.Fatal("refresh changed path or schedule")
	}
	a.Auth.RefreshToken = ""
	storage, _ = json.Marshal(a)
	req, _ = json.Marshal(pluginapi.AuthRefreshRequest{StorageJSON: storage})
	if _, err := handleRefreshAuth(req); err == nil || !strings.Contains(err.Error(), "missing refreshToken") {
		t.Fatal("empty refresh token sent")
	}
}
func TestAccountRequestsDoNotShareCookiesOrRefreshTokens(t *testing.T) {
	if sharedHTTPClient().Jar != nil {
		t.Fatal("shared credentials use a cookie jar")
	}
	var cookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		if r.Header.Get("X-Refresh-Token") != "" {
			t.Error("refresh credential exposed")
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "account-A"})
	}))
	defer server.Close()
	for _, uid := range []string{"A", "B"} {
		sa := testAuth()
		sa.Auth.RefreshToken = "long-lived"
		sa.Account.UID = uid
		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		backendHeaders(req, sa)
		resp, err := sharedHTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if cookie != "" {
			t.Fatal("cookie crossed accounts")
		}
	}
	req, _ := http.NewRequest(http.MethodPost, endpointChat, nil)
	a := testAuth()
	a.Auth.RefreshToken = "long-lived"
	chatHeaders(req, a)
	if req.Header.Get("X-Refresh-Token") != "" {
		t.Fatal("refresh token exposed to chat")
	}
}
func TestPayloadPreservesNumbersHTMLAndUserMessages(t *testing.T) {
	setupRuntimeCatalog(t)
	text := "You are Claude Code, Anthropic's official CLI for Claude."
	input := fmt.Sprintf(`{"seed":12345678901234567891,"messages":[{"role":"user","content":%q},{"role":"assistant","content":%q},{"role":"tool","content":%q},{"role":"system","content":%q},{"role":"developer","content":[{"type":"text","text":%q}]}],"extra":"<b>&"}`, text, text, text, text, text)
	raw, err := prepareUpstreamPayload(pluginapi.ExecutorRequest{Model: "hy3", Payload: []byte(input)}, testAuth())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"seed":12345678901234567891`)) || !bytes.Contains(raw, []byte(`<b>&`)) {
		t.Fatal("numeric or HTML data changed")
	}
	obj, _ := decodeObject(raw)
	messages := obj["messages"].([]any)
	for i := 0; i < 3; i++ {
		if messages[i].(map[string]any)["content"] != text {
			t.Fatal("non-system content changed")
		}
	}
	if messages[3].(map[string]any)["content"] == text {
		t.Fatal("system compatibility rewrite missing")
	}
	for _, invalid := range []string{`{`, `[]`, `{} {}`} {
		if _, err := decodeObject([]byte(invalid)); err == nil {
			t.Fatal("invalid object accepted")
		}
	}
}
func TestTruncateAlwaysReturnsValidUTF8(t *testing.T) {
	s := strings.Repeat("中文🙂", 100)
	for n := -1; n <= len(s)+1; n++ {
		if !utf8.ValidString(truncate(s, n)) {
			t.Fatalf("invalid UTF-8 at %d", n)
		}
	}
}
func TestJSONResponseLimitAndErrorRedaction(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", 10<<20), `{"code":12345,"msg":"access-secret\nrefresh-secret","data":null}`} {
		mockWorkbuddyHTTP(t, func(*http.Request) (int, string) { return 200, body })
		_, _, err := doJSON(sharedHTTPClient(), http.MethodGet, endpointAccount, nil, nil)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "\n") {
			t.Fatal("unbounded or untrusted error response")
		}
	}
}
func TestLoginCleanupAndConcurrentPoll(t *testing.T) {
	for i := 0; i < 3; i++ {
		loginStates.Store(fmt.Sprintf("abandoned-%d", i), &loginCtx{expires: time.Now().Add(-time.Second)})
	}
	mockWorkbuddyHTTP(t, func(*http.Request) (int, string) {
		return 200, `{"code":0,"data":{"state":"cleanup-new","authUrl":"https://www.workbuddy.cn/login?platform=workbuddy&state=cleanup-new"}}`
	})
	raw, err := handleStartLogin(nil)
	var start pluginapi.AuthLoginStartResponse
	decodeAuthResult(t, raw, err, &start)
	t.Cleanup(func() {
		v, _ := loginStates.LoadAndDelete(start.State)
		if v != nil && v.(*loginCtx).timer != nil {
			v.(*loginCtx).timer.Stop()
		}
	})
	count := 0
	loginStates.Range(func(k, v any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("abandoned logins remain: %d", count)
	}
	v, _ := loginStates.Load(start.State)
	lc := v.(*loginCtx)
	lc.mu.Lock()
	defer lc.mu.Unlock()
	req, _ := json.Marshal(pluginapi.AuthLoginPollRequest{State: start.State})
	raw, err = handlePollLogin(req)
	var poll pluginapi.AuthLoginPollResponse
	decodeAuthResult(t, raw, err, &poll)
	if poll.Status != "pending" {
		t.Fatal("concurrent poll blocked")
	}
}
func TestDiagnosticNeverLogsCredentialsOrBodies(t *testing.T) {
	var buffer bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	defer slog.SetDefault(old)
	sa := testAuth()
	sa.Auth.AccessToken = "access-secret"
	sa.Auth.RefreshToken = "refresh-secret"
	diagnostic("test_failure", sa, fmt.Errorf("access-secret refresh-secret raw-body"), time.Second)
	text := buffer.String()
	if !strings.Contains(text, "account_hash") || strings.Contains(text, "secret") || strings.Contains(text, "TEST_UID") || strings.Contains(text, "raw-body") {
		t.Fatal("unsafe diagnostics")
	}
}
func TestUnknownRPCAndProviderRegistration(t *testing.T) {
	if _, err := handleMethod("nope", nil); err == nil || errorStatus(err) != 501 {
		t.Fatal("unknown RPC succeeded")
	}
	for _, method := range []string{"auth.identifier", "executor.identifier", "quota.identifier", "quota.describe", "model.static"} {
		raw, err := handleMethod(method, nil)
		if err != nil || !bytes.Contains(raw, []byte(`"ok":true`)) {
			t.Fatalf("%s: %s %v", method, raw, err)
		}
	}
	reg := wbRegistration()
	if !reg.Capabilities.QuotaProvider || reg.Metadata.Logo == "" || reg.Metadata.Version != version {
		t.Fatal("missing native CPA metadata")
	}
}

func makeExecutorRequest(t *testing.T, id string) []byte {
	t.Helper()
	storage, _ := json.Marshal(testAuth())
	raw, err := json.Marshal(executorStreamRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "fixture", StorageJSON: storage, Model: "hy3", Payload: []byte(`{}`), Metadata: map[string]any{"request_path": "/v1/chat/completions"}}, StreamID: id})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestStreamHTTPStatusBeforeSuccess(t *testing.T) {
	setupRuntimeCatalog(t)
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			mockWorkbuddyHTTP(t, func(*http.Request) (int, string) { return status, `{"msg":"do-not-echo"}` })
			for _, id := range []string{"", "async"} {
				_, err := handleExecStream(makeExecutorRequest(t, id))
				var failure *requestError
				if !errors.As(err, &failure) || failure.StatusCode() != status || strings.Contains(err.Error(), "do-not-echo") {
					t.Fatalf("status lost: %v", err)
				}
			}
			if _, err := collectUpstreamStream([]byte(`{}`), testAuth(), false); errorStatus(err) != status {
				t.Fatal(err)
			}
		})
	}
}
func resetTestLifecycle(t *testing.T) {
	t.Helper()
	old := lifecycle
	lifecycle = newLifecycle()
	t.Cleanup(func() { shutdownPlugin(); lifecycle = old })
}
func TestAsyncStreamEmitsAndCloses(t *testing.T) {
	setupRuntimeCatalog(t)
	resetTestLifecycle(t)
	input := "data: " + fixtureChunk(map[string]any{"content": "ok"}, "stop") + "\n\ndata: [DONE]\n\n"
	mockWorkbuddyHTTP(t, func(*http.Request) (int, string) { return 200, input })
	var mu sync.Mutex
	var payloads [][]byte
	closed := make(chan struct{})
	var once sync.Once
	previous := hostCallFn
	hostCallFn = func(method string, raw []byte) ([]byte, error) {
		if method == "host.stream.emit" {
			var v struct{ Payload []byte }
			_ = json.Unmarshal(raw, &v)
			mu.Lock()
			payloads = append(payloads, v.Payload)
			mu.Unlock()
		}
		if method == "host.stream.close" {
			once.Do(func() { close(closed) })
		}
		return okEnvelope(map[string]any{})
	}
	defer func() { lifecycle.wg.Wait(); hostCallFn = previous }()
	if _, err := handleExecStream(makeExecutorRequest(t, "test-async")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stream not closed")
	}
	lifecycle.wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 1 || !json.Valid(payloads[0]) {
		t.Fatal("invalid async chunk")
	}
}
func TestShutdownAndTotalDeadlineStopHeartbeatOnlyStreams(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			resetTestLifecycle(t)
			ctx, done, err := startOperation(50 * time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			reader, writer := io.Pipe()
			stop := context.AfterFunc(ctx, func() { reader.CloseWithError(ctx.Err()) })
			defer stop()
			response := &http.Response{StatusCode: 200, Body: reader, Request: (&http.Request{}).WithContext(ctx)}
			previous := hostCallFn
			var mu sync.Mutex
			var result string
			hostCallFn = func(method string, raw []byte) ([]byte, error) {
				mu.Lock()
				result += string(raw)
				mu.Unlock()
				return okEnvelope(map[string]any{})
			}
			defer func() { lifecycle.wg.Wait(); hostCallFn = previous }()
			go func() { defer done(); pumpUpstreamStream(response, "heartbeat", false, nil) }()
			go func() {
				defer writer.Close()
				for {
					if _, err := writer.Write([]byte(": heartbeat\n\n")); err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			start := time.Now()
			if shutdown {
				shutdownPlugin()
			} else {
				lifecycle.wg.Wait()
			}
			if time.Since(start) > time.Second {
				t.Fatal("stream survived cancellation")
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(result, "request_canceled") && !strings.Contains(result, "upstream_error") {
				t.Fatal("cancellation error was lost")
			}
			if shutdown {
				if _, _, err := startOperation(time.Second); err == nil {
					t.Fatal("new work accepted after shutdown")
				}
			}
		})
	}
}

func TestAuthParserDoesNotClaimForeignCredentials(t *testing.T) {
	for _, req := range []pluginapi.AuthParseRequest{{Provider: "foreign", RawJSON: []byte(`{"auth":{"accessToken":"secret"}}`)}, {RawJSON: []byte(`{"type":"foreign","auth":{"accessToken":"secret"}}`)}, {RawJSON: []byte(`{`)}} {
		raw, _ := json.Marshal(req)
		out, err := handleParseAuth(raw)
		var parsed pluginapi.AuthParseResponse
		decodeAuthResult(t, out, err, &parsed)
		if parsed.Handled {
			t.Fatal("foreign credentials claimed")
		}
	}
}
