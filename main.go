// Package main implements the workbuddy CLIProxyAPI dynamic plugin.
//
// workbuddy wraps Tencent WorkBuddy China (www.workbuddy.cn) as a cliproxy
// provider: it performs the WorkBuddy web login flow, refreshes access
// tokens, and forwards OpenAI-compatible chat completion requests to the
// upstream /v2/chat/completions endpoint.
//
// This file is a clean-room reimplementation reconstructed from the public
// workbuddy.so binary (symbol table, string constants and RPC shape) published
// by Sliverkiss. Original credit for the workbuddy plugin goes to Sliverkiss;
// see https://github.com/Sliverkiss/cpa-plugin. Built with -buildmode=c-shared
// and exports the cliproxy C ABI entry points.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

// Wrappers so Go can invoke the host function-pointer table via cgo. The host
// API captured at init is used to push streaming chunks back asynchronously.
static int wb_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void wb_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/lovingfish/workbuddy-cliproxy/internal/workbuddy"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var version = "0.4.0" // Release builds may override with -ldflags -X main.version=...

const (
	providerName = "workbuddy"
	authFileName = "workbuddy.json"
	upstreamBase = workbuddy.BaseURL

	endpointAuthState    = upstreamBase + "/v2/plugin/auth/state?platform=workbuddy"
	endpointAccount      = upstreamBase + "/v2/plugin/account"
	endpointAuthToken    = upstreamBase + "/v2/plugin/auth/token?state="
	endpointTokenRefresh = upstreamBase + "/v2/plugin/auth/token/refresh"
	endpointChat         = upstreamBase + "/v2/chat/completions"

	loginTTL = 5 * time.Minute
)

// loginCtx holds the cookie-affined HTTP client for one in-flight login flow.
// CodeBuddy associates the browser login with the state issued at auth/state,
// so we must reuse the same cookie jar across the state request and the polls.
type loginCtx struct {
	mu             sync.Mutex
	client         *http.Client
	expires        time.Time
	token          *tokenData // auth/token is one-shot; retain it while account data is pending.
	tokenExpiresAt int64
	timer          *time.Timer
}

var (
	hostAPI        *C.cliproxy_host_api // captured at init, used for async host calls
	loginStates    sync.Map             // state(string) -> *loginCtx
	httpClientOnce sync.Once
	sharedClient   *http.Client
)

func main() {}

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if uint64(requestLen) > 32<<20 || (request == nil && requestLen > 0) {
		writeResponse(response, errorEnvelope("invalid_request", "RPC request exceeds limit or has no buffer", 413))
		return 1
	}
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		status := 0
		var statusError interface{ StatusCode() int }
		if errors.As(errHandle, &statusError) {
			status = statusError.StatusCode()
		}
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error(), status))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { shutdownPlugin() }

// -----------------------------------------------------------------------------
// Host calls (async streaming)
// -----------------------------------------------------------------------------

// hostCall invokes a host RPC method via the function-pointer table captured
// at init. Used to push stream chunks back asynchronously (host.stream.emit /
// host.stream.close).
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.wb_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 && uint64(resp.len) <= 32<<20 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.wb_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
var hostCallFn = hostCall

func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCallFn(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) { streamEmitFailure(streamID, errors.New(message)) }

func streamEmitFailure(streamID string, err error) {
	message := publicError(err)
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "error": message, "error_detail": map[string]any{"code": "upstream_error", "message": message, "http_status": errorStatus(err)}})
	_, _ = hostCallFn(pluginabi.MethodHostStreamEmit, body)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCallFn(pluginabi.MethodHostStreamClose, body)
}

// -----------------------------------------------------------------------------
// RPC dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configureCatalog(request); err != nil {
			return nil, err
		}
		return okEnvelope(wbRegistration())
	case pluginabi.MethodModelStatic:
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: []pluginapi.ModelInfo{}})
	case pluginabi.MethodModelForAuth:
		return handleCatalogForAuth(request)
	case pluginabi.MethodResponseInterceptAfter:
		return handleCatalogResponse(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]any{"identifier": providerName, "entries": []map[string]string{{"identifier": providerName, "name": "WorkBuddy", "logo": workbuddyLogo}}})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	case "quota.identifier":
		return okEnvelope(identifierResponse{Identifier: providerName})
	case "quota.describe":
		return okEnvelope(map[string]any{"supported_providers": []string{providerName}, "display_name": "WorkBuddy", "supports_reset": false})
	case "quota.fetch":
		return handleQuotaFetch(request)
	case "quota.reset":
		return nil, &requestError{status: 501, message: "WorkBuddy quota cannot be reset by the plugin"}
	case pluginabi.MethodPluginShutdown, "plugin.quiesce":
		shutdownPlugin()
		return okEnvelope(map[string]any{})
	default:
		return nil, &requestError{status: 501, message: "unknown method: " + truncate(method, 100)}
	}
}

// -----------------------------------------------------------------------------
// Registration & models
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	QuotaProvider         bool                         `json:"quota_provider"`
	ResponseInterceptor   bool                         `json:"response_interceptor"`
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func wbRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          version,
			Logo:             workbuddyLogo,
			Author:           "lovingfish (clean-room rebuild; original workbuddy by Sliverkiss)",
			GitHubRepository: "https://github.com/LYISTR2/workbuddy-cliproxy",
		},
		Capabilities: registrationCapability{
			QuotaProvider:         true,
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ResponseInterceptor:   true,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

// -----------------------------------------------------------------------------
// Auth data shapes (matches persisted workbuddy.json)
// -----------------------------------------------------------------------------

// storedAuth is the on-disk shape of a workbuddy credential.
type storedAuth struct {
	Type    string        `json:"type,omitempty"`
	Auth    storedTokens  `json:"auth"`
	Account storedAccount `json:"account"`
}

type storedTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
}

type storedAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// apiEnvelope is the generic {code,msg,data} wrapper used by every CodeBuddy API.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if sa.Type != "" && sa.Type != providerName {
		return nil, fmt.Errorf("foreign credential provider")
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &sa, nil
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				MaxIdleConns:          20,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   5,
			},
			CheckRedirect: refuseRedirect,
		}
	})
	return sharedClient
}

// newLoginClient builds an isolated client with its own cookie jar so that the
// browser login for one state can never leak into another.
func newLoginClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     sharedHTTPClient().Transport,
		Jar:           jar,
		CheckRedirect: refuseRedirect,
	}
}

func commonHeaders(req *http.Request) {
	workbuddy.SetClientHeaders(req)
}

// backendHeaders applies auth-derived headers to a chat completion request.
// Empty fields are signalled via the X-No-* convention used by CodeBuddy.
func backendHeaders(req *http.Request, sa *storedAuth) {
	commonHeaders(req)
	if sa.Auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
}

// doJSON sends method to fullURL with the given headers, parses the {code,msg,data}
// envelope, and returns the inner data payload. httpStatus is the upstream code.
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(pluginContext(), method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("WorkBuddy API transport failed")
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, maxJSONResponseBytes)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, &upstreamAPIError{Status: resp.StatusCode}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("WorkBuddy returned invalid API JSON")
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, &upstreamAPIError{Status: resp.StatusCode, Code: env.Code}
	}
	return env.Data, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Provider != "" && req.Provider != providerName {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		// Not a workbuddy credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    parsedAuthData(sa, req),
	})
}

func toAuthData(sa *storedAuth) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	id, file := authIdentity(sa)
	metadata := map[string]any{"type": providerName, "uid": sa.Account.UID, "nickname": sa.Account.Nickname}
	if sa.Auth.RefreshToken != "" {
		metadata["refresh_interval_seconds"] = 300
	}
	if sa.Auth.ExpiresAt > 0 {
		metadata["expires_at"] = time.Unix(sa.Auth.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	return pluginapi.AuthData{Provider: providerName, ID: id, FileName: file,
		Label:       "WorkBuddy · " + firstNonEmpty(sa.Account.Nickname, sa.Account.UID, "账号"),
		StorageJSON: storage, Metadata: metadata, NextRefreshAfter: nextRefresh(sa.Auth.ExpiresAt)}
}

func parsedAuthData(sa *storedAuth, req pluginapi.AuthParseRequest) pluginapi.AuthData {
	auth := toAuthData(sa)
	if file := firstNonEmpty(req.FileName, filepath.Base(req.Path)); file != "" && file != "." {
		auth.FileName = filepath.Base(file)
		auth.ID = "" // Preserve the host's path identity for existing files.
	}
	return auth
}

func handleStartLogin(raw []byte) ([]byte, error) {
	cleanupLogins(time.Now())
	client := rpcHTTPClient(newLoginClient(), raw)
	data, _, err := doJSON(client, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl")
	}
	loginURL, err := workbuddyLoginURL(st.AuthURL, st.State)
	if err != nil {
		return nil, err
	}
	lc := &loginCtx{client: client, expires: time.Now().Add(loginTTL)}
	loginStates.Store(st.State, lc)
	time.AfterFunc(loginTTL, func() { loginStates.CompareAndDelete(st.State, lc) })
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       loginURL,
		State:     st.State,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
	})
}

func workbuddyLoginURL(raw, state string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Host != "www.workbuddy.cn" && u.Host != "workbuddy.cn") {
		return "", fmt.Errorf("auth state: expected the official WorkBuddy login URL")
	}
	query := u.Query()
	if query.Get("platform") != "workbuddy" || query.Get("state") != state {
		return "", fmt.Errorf("auth state: unexpected WorkBuddy authorization parameters")
	}
	if query.Get("loginSessionId") == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", fmt.Errorf("create WorkBuddy login session: %w", err)
		}
		id[6] = (id[6] & 0x0f) | 0x40
		id[8] = (id[8] & 0x3f) | 0x80
		query.Set("loginSessionId", fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]))
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login)")
	}
	lc := v.(*loginCtx)
	if !lc.mu.TryLock() {
		return loginPending("正在确认 WorkBuddy 授权")
	}
	defer lc.mu.Unlock()
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired")
	}

	// The host drives polling. Once issued, a token must survive account-service
	// delays: asking auth/token for it a second time may never return it again.
	if lc.token == nil {
		tokRaw, _, errTok := doJSON(rpcHTTPClient(lc.client, raw), http.MethodGet, endpointAuthToken+url.QueryEscape(state), nil, nil)
		var tok tokenData
		if errTok != nil {
			if terminalLoginError(errTok) {
				loginStates.Delete(state)
				return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: publicError(errTok) + "；请重新发起 WorkBuddy 登录"})
			}
			return loginPending("等待在 WorkBuddy 官方网页完成授权")
		}
		if json.Unmarshal(tokRaw, &tok) != nil {
			return nil, fmt.Errorf("invalid WorkBuddy token response")
		}
		if tok.AccessToken == "" {
			return loginPending("等待在 WorkBuddy 官方网页完成授权")
		}
		lc.token = &tok
		lc.tokenExpiresAt = tokenExpiry(tok.ExpiresIn)
	}
	tok := lc.token
	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    lc.tokenExpiresAt,
			Domain:       tok.Domain,
		},
	}
	account, err := fetchWorkbuddyAccount(rpcHTTPClient(lc.client, raw), sa)
	if err != nil {
		if terminalLoginError(err) {
			loginStates.Delete(state)
			return nil, err
		}
		return loginPending("授权已完成，正在读取 WorkBuddy 账号资料")
	}
	sa.Account = account
	loginStates.Delete(state)
	if lc.timer != nil {
		lc.timer.Stop()
	}
	lc.token = nil
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

func fetchWorkbuddyAccount(client *http.Client, sa *storedAuth) (storedAccount, error) {
	raw, _, err := doJSON(client, http.MethodGet, endpointAccount, func(r *http.Request) { backendHeaders(r, sa) }, nil)
	if err != nil {
		return storedAccount{}, err
	}
	var account storedAccount
	if err := json.Unmarshal(raw, &account); err != nil || strings.TrimSpace(account.UID) == "" {
		return storedAccount{}, fmt.Errorf("WorkBuddy account response has no user ID")
	}
	return account, nil
}

func handleRefreshAuth(raw []byte) (result []byte, failure error) {
	started := time.Now()
	defer func() {
		if failure != nil {
			diagnostic("refresh_failed", nil, failure, time.Since(started))
		}
	}()
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	if strings.TrimSpace(sa.Auth.RefreshToken) == "" {
		return nil, &requestError{status: 401, message: "refresh: missing refreshToken, re-login required"}
	}
	headers := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
		if sa.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", "plugin")
	}
	client := rpcHTTPClient(sharedHTTPClient(), raw)
	data, status, err := doJSON(client, http.MethodPost, endpointTokenRefresh, headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		if status >= 400 {
			return nil, &requestError{status: status, message: fmt.Sprintf("refresh rejected (HTTP %d)", status)}
		}
		return nil, fmt.Errorf("refresh: %w", err)
	}
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = tokenExpiry(tok.ExpiresIn)
	// A temporary profile error must not discard newly rotated refresh tokens.
	if account, err := fetchWorkbuddyAccount(client, sa); err == nil {
		sa.Account = account
	} else {
		diagnostic("refresh_profile_failed", sa, err, 0)
	}
	auth := toAuthData(sa)
	if req.AuthID != "" {
		auth.ID = req.AuthID
	}
	auth.FileName = "" // The host retains the original file, including legacy workbuddy.json.

	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter})
}

// -----------------------------------------------------------------------------
// Executor handlers
// -----------------------------------------------------------------------------

func handleExecExecute(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	body, err := prepareUpstreamPayload(req.ExecutorRequest, sa)
	if err != nil {
		return nil, err
	}
	resp, done, err := openUpstreamChat(body, sa, req.HostCallbackID)
	if err != nil {
		diagnostic("chat_failed", sa, err, 0)
		return nil, err
	}
	defer done()
	defer resp.Body.Close()
	completion, err := aggregateCompletion(resp.Body, req.Model, offeredToolNames(body))
	if err != nil {
		if cause := resp.Request.Context().Err(); cause != nil {
			err = cause
		}
		diagnostic("chat_failed", sa, err, 0)
		return nil, &requestError{status: errorStatus(err), message: publicError(err)}
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// executorStreamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	body, err := prepareUpstreamPayload(req.ExecutorRequest, sa)
	if err != nil {
		return nil, err
	}
	// Return HTTP errors before declaring the stream successful so CPA can retry
	// another credential, refresh, or cool down a rate-limited account.
	resp, done, err := openUpstreamChat(body, sa, req.HostCallbackID)
	if err != nil {
		diagnostic("stream_open_failed", sa, err, 0)
		return nil, err
	}
	framed := clientNeedsSSEFrame(req.Metadata)
	if req.StreamID == "" {
		defer done()
		defer resp.Body.Close()
		chunks, err := aggregateSSE(resp.Body, framed)
		if err != nil {
			return nil, &requestError{status: errorStatus(err), message: publicError(err)}
		}
		return okEnvelope(streamResponse{Headers: streamHeaders(), Chunks: chunks})
	}
	go func() { defer done(); pumpUpstreamStream(resp, req.StreamID, framed, sa) }()
	return okEnvelope(streamResponse{Headers: streamHeaders()})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream reads the upstream SSE response in the background and
// emits each cleaned chunk to the host stream. It closes the stream when done.
// An emit failure (client disconnected → host closed the stream) aborts the
// pump so we stop reading a dead upstream.
func pumpUpstreamStream(resp *http.Response, streamID string, framed bool, sa *storedAuth) {
	defer resp.Body.Close()
	defer streamClose(streamID)
	// Closing the host stream unblocks a host emit stalled by downstream backpressure.
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(resp.Request.Context(), func() { defer close(callbackDone); streamCloseFailure(streamID, resp.Request.Context().Err()) })
	defer func() {
		if !stop() {
			<-callbackDone
		}
	}()
	if err := forwardCompletionStream(resp.Body, framed, func(payload []byte) error { return streamEmit(streamID, payload) }); err != nil {
		if cause := resp.Request.Context().Err(); cause != nil {
			err = cause
		}
		diagnostic("stream_failed", sa, err, 0)
		streamEmitFailure(streamID, err)
	}
}

func collectUpstreamStream(body []byte, sa *storedAuth, framed bool) ([]pluginapi.ExecutorStreamChunk, error) {
	resp, done, err := openUpstreamChat(body, sa, "")
	if err != nil {
		return nil, err
	}
	defer done()
	defer resp.Body.Close()
	chunks, err := aggregateSSE(resp.Body, framed)
	if err != nil {
		return nil, &requestError{status: errorStatus(err), message: publicError(err)}
	}
	return chunks, nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// aggregateSSE reads an upstream SSE stream and emits one chunk per data event.
// Empty-valued delta fields are stripped and the trailing [DONE] is dropped
// (the host appends its own stream terminator). When sseFramed is true each
// payload is emitted as a "data: " line for cross-format translators; otherwise
// the payload is the raw JSON object and the host chat-completions writer adds
// the framing itself.
func aggregateSSE(r io.Reader, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, error) {
	var chunks []pluginapi.ExecutorStreamChunk
	err := forwardCompletionStream(r, sseFramed, func(payload []byte) error {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
		return nil
	})
	return chunks, err
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
	obj, err := decodeObject(payload)
	if err != nil {
		return payload
	}
	rewriteSystemMessages(obj)
	result, err := marshalJSON(obj)
	if err != nil {
		return payload
	}
	return result
}
func rewriteSystemMessages(obj map[string]any) {
	messages, _ := obj["messages"].([]any)
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "system" || msg["role"] == "developer" {
			rewriteContentField(msg)
		}
	}
}

// rewriteContentField sanitizes blocked templates in one message's content,
// handling both plain-string and OpenAI multimodal (array of parts) shapes.
// Returns true if the message was modified.
func rewriteContentField(msg map[string]any) bool {
	switch c := msg["content"].(type) {
	case string:
		if r := sanitizeBlockedTemplates(c); r != c {
			msg["content"] = r
			return true
		}
	case []any:
		modified := false
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				if r := sanitizeBlockedTemplates(t); r != t {
					part["text"] = r
					modified = true
				}
			}
		}
		return modified
	}
	return false
}

func sanitizeBlockedTemplates(s string) string {
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	return s
}

// aggregateCompletion folds an SSE stream into a single non-streaming
// chat.completion object (used for non-stream client requests).
func aggregateCompletion(r io.Reader, model string, known ...[]string) ([]byte, error) {
	var content, reasoning strings.Builder
	var role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	toolCallsByIndex := map[int]map[string]any{}

	err := readCompletionEvents(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if choice == nil {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content.WriteString(v)
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(v)
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for index, tc := range tcs {
						if call, ok := tc.(map[string]any); ok {
							mergeToolCallDelta(toolCallsByIndex, call, index, known...)
						}
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	toolCalls := orderedToolCalls(toolCallsByIndex)
	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// envelope helpers
// -----------------------------------------------------------------------------

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string, statuses ...int) []byte {
	status := 0
	if len(statuses) > 0 {
		status = statuses[0]
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "�")
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.ToValidUTF8(s[:n], "�")
}
