//go:build cgo

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type workbuddyAuthTransport func(*http.Request) (*http.Response, error)

func (f workbuddyAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func mockWorkbuddyHTTP(t *testing.T, handler func(*http.Request) (int, string)) {
	t.Helper()
	previous := sharedHTTPClient()
	sharedClient = &http.Client{Transport: workbuddyAuthTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "www.workbuddy.cn" || req.Header.Get("Origin") != "https://www.workbuddy.cn" || req.Header.Get("X-Product-Code") != "workbuddy" || strings.Contains(req.UserAgent(), "CodeBuddy") {
			t.Errorf("request did not use WorkBuddy product identity: %s", req.URL.Path)
		}
		status, body := handler(req)
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	t.Cleanup(func() { sharedClient = previous })
}

func decodeAuthResult(t *testing.T, raw []byte, err error, result any) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("invalid auth response: %s", raw)
	}
	if err := json.Unmarshal(env.Result, result); err != nil {
		t.Fatal(err)
	}
}

func TestWorkbuddyLoginURLRejectsOtherProducts(t *testing.T) {
	good := "https://www.workbuddy.cn/login?platform=workbuddy&state=test&extra=keep"
	result, err := workbuddyLoginURL(good, "test")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(result)
	if u.Query().Get("extra") != "keep" || len(u.Query().Get("loginSessionId")) != 36 {
		t.Fatal("WorkBuddy session parameters were lost")
	}
	for _, bad := range []string{
		"https://copilot.tencent.com/login?platform=CLI&state=test",
		"https://www.workbuddy.cn/login?platform=CLI&state=test",
		"https://www.workbuddy.cn/login?platform=workbuddy&state=other",
		"http://www.workbuddy.cn/login?platform=workbuddy&state=test",
	} {
		if _, err := workbuddyLoginURL(bad, "test"); err == nil {
			t.Fatalf("accepted incorrect login URL %s", bad)
		}
	}
}

func TestWorkbuddyLoginRetainsOneShotTokenUntilAccountIsReady(t *testing.T) {
	var tokenCalls, accountCalls int
	mockWorkbuddyHTTP(t, func(req *http.Request) (int, string) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			if req.Method != http.MethodPost || req.URL.Query().Get("platform") != "workbuddy" {
				t.Error("wrong login platform")
			}
			return 200, `{"code":0,"data":{"state":"test-workbuddy","authUrl":"https://www.workbuddy.cn/login?platform=workbuddy&state=test-workbuddy"}}`
		case "/v2/plugin/auth/token":
			tokenCalls++
			if tokenCalls == 1 {
				return 200, `{"code":11217,"data":null}`
			}
			if tokenCalls > 2 {
				t.Error("one-shot token was requested again")
			}
			return 200, `{"code":0,"data":{"accessToken":"mock-access","refreshToken":"mock-refresh","expiresIn":3600,"domain":"workbuddy.cn"}}`
		case "/v2/plugin/account":
			accountCalls++
			if req.Header.Get("Authorization") != "Bearer mock-access" || req.URL.RawQuery != "" {
				t.Error("wrong WorkBuddy account request")
			}
			if accountCalls == 1 {
				return 200, `{"code":12151,"data":null}`
			}
			if accountCalls == 2 {
				return 200, `{"code":0,"data":{"nickname":"not-ready"}}`
			}
			return 200, `{"code":0,"data":{"uid":"mock-user","enterpriseId":"mock-enterprise","nickname":"WorkBuddy User"}}`
		default:
			t.Errorf("unexpected auth path: %s", req.URL.Path)
			return 404, `{}`
		}
	})
	raw, err := handleStartLogin(nil)
	var start pluginapi.AuthLoginStartResponse
	decodeAuthResult(t, raw, err, &start)
	t.Cleanup(func() { loginStates.Delete(start.State) })
	u, _ := url.Parse(start.URL)
	if u.Host != "www.workbuddy.cn" || u.Query().Get("platform") != "workbuddy" {
		t.Fatalf("wrong login page: %s", start.URL)
	}
	for _, status := range []pluginapi.AuthLoginStatus{pluginapi.AuthLoginStatusPending, pluginapi.AuthLoginStatusPending, pluginapi.AuthLoginStatusPending, pluginapi.AuthLoginStatusSuccess} {
		request, _ := json.Marshal(pluginapi.AuthLoginPollRequest{State: start.State})
		raw, err := handlePollLogin(request)
		var poll pluginapi.AuthLoginPollResponse
		decodeAuthResult(t, raw, err, &poll)
		if poll.Status != status {
			t.Fatalf("status=%v, expected=%v", poll.Status, status)
		}
		if status == pluginapi.AuthLoginStatusSuccess {
			auth, err := parseStored(poll.Auth.StorageJSON)
			if err != nil || auth.Account.UID != "mock-user" || auth.Auth.RefreshToken != "mock-refresh" {
				t.Fatal("authorized WorkBuddy account was not persisted")
			}
		}
	}
	if tokenCalls != 2 || accountCalls != 3 {
		t.Fatalf("token calls=%d, account calls=%d", tokenCalls, accountCalls)
	}
}

func TestWorkbuddyRefreshKeepsRotatedTokensOnProfileFailure(t *testing.T) {
	for _, profileReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "profile_unavailable", true: "profile_ready"}[profileReady], func(t *testing.T) {
			mockWorkbuddyHTTP(t, func(req *http.Request) (int, string) {
				switch req.URL.Path {
				case "/v2/plugin/auth/token/refresh":
					body, _ := io.ReadAll(req.Body)
					if string(body) != "{}" || req.Header.Get("X-Auth-Refresh-Source") != "plugin" || req.Header.Get("X-Refresh-Token") != "old-refresh" {
						t.Error("incorrect WorkBuddy refresh request")
					}
					return 200, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`
				case "/v2/plugin/account":
					if req.Header.Get("Authorization") != "Bearer new-access" {
						t.Error("profile used the old token")
					}
					if !profileReady {
						return 503, `{}`
					}
					return 200, `{"code":0,"data":{"uid":"mock-user","nickname":"new-name"}}`
				default:
					t.Errorf("unexpected refresh path: %s", req.URL.Path)
					return 404, `{}`
				}
			})
			storage, _ := json.Marshal(storedAuth{Auth: storedTokens{AccessToken: "old-access", RefreshToken: "old-refresh"}, Account: storedAccount{UID: "mock-user", Nickname: "old-name"}})
			request, _ := json.Marshal(pluginapi.AuthRefreshRequest{StorageJSON: storage})
			raw, err := handleRefreshAuth(request)
			var refresh pluginapi.AuthRefreshResponse
			decodeAuthResult(t, raw, err, &refresh)
			auth, err := parseStored(refresh.Auth.StorageJSON)
			if err != nil || auth.Auth.AccessToken != "new-access" || auth.Auth.RefreshToken != "new-refresh" || auth.Account.UID != "mock-user" {
				t.Fatal("rotated credential was lost")
			}
			if profileReady && auth.Account.Nickname != "new-name" {
				t.Fatal("account profile was not refreshed")
			}
		})
	}
}
