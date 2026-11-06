//go:build cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHostHTTPReadsAndCleansUpThroughCPA(t *testing.T) {
	previous := hostCallFn
	schema := hostSchema.Load()
	hostSchema.Store(10)
	defer func() { hostCallFn = previous; hostSchema.Store(schema) }()
	var mu sync.Mutex
	var methods []string
	reads := 0
	hostCallFn = func(method string, raw []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, method)
		switch method {
		case "host.http.operation_open":
			return okEnvelope(map[string]any{"operation_id": "op"})
		case "host.http.do_stream":
			var req struct {
				HostCallbackID string `json:"host_callback_id"`
				OperationID    string `json:"operation_id"`
				Headers        http.Header
				Body           []byte
			}
			_ = json.Unmarshal(raw, &req)
			if req.HostCallbackID != "callback" || req.OperationID != "op" || string(req.Body) != "{}" {
				t.Error("host context lost")
			}
			return okEnvelope(map[string]any{"status_code": 200, "stream_id": "http-stream"})
		case "host.http.stream_read":
			reads++
			if reads == 1 {
				return okEnvelope(map[string]any{"payload": []byte("hello")})
			}
			return okEnvelope(map[string]any{"done": true})
		default:
			return okEnvelope(map[string]any{})
		}
	}
	req, _ := http.NewRequestWithContext(withHostCallback(context.Background(), "callback"), http.MethodPost, endpointChat, strings.NewReader(`{}`))
	resp, err := doChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil || string(raw) != "hello" {
		t.Fatalf("body=%s err=%v", raw, err)
	}
	resp.Body.Close()
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(methods, ",")
	if strings.Count(joined, "host.http.stream_close") != 1 || strings.Count(joined, "host.http.cancel") != 1 {
		t.Fatal(joined)
	}
}
func TestHostHTTPClosesWhenCallerCancels(t *testing.T) {
	previous := hostCallFn
	schema := hostSchema.Load()
	hostSchema.Store(10)
	var mu sync.Mutex
	closed := make(chan struct{})
	var once sync.Once
	hostCallFn = func(method string, raw []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "host.http.operation_open":
			return okEnvelope(map[string]any{"operation_id": "op"})
		case "host.http.do_stream":
			return okEnvelope(map[string]any{"status_code": 200, "stream_id": "stream"})
		case "host.http.stream_close":
			once.Do(func() { close(closed) })
		}
		return okEnvelope(map[string]any{})
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpointChat, bytes.NewReader([]byte(`{}`)))
	resp, err := doHostChatRequest(req, "callback")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("host stream survived cancellation")
	}
	resp.Body.Close()
	hostCallFn = previous
	hostSchema.Store(schema)
}
func TestHostHTTPErrorDoesNotFallBackToDirectNetwork(t *testing.T) {
	previous := hostCallFn
	defer func() { hostCallFn = previous }()
	for _, response := range [][]byte{[]byte(`bad-json`), errorEnvelope("denied", "do-not-leak", 403), []byte(`{"ok":true,"result":"wrong-type"}`)} {
		hostCallFn = func(string, []byte) ([]byte, error) { return response, nil }
		if err := hostRPC("test", map[string]string{}, new(map[string]any)); err == nil || strings.Contains(err.Error(), "do-not-leak") {
			t.Fatal("host failure bypassed or leaked")
		}
	}
	hostCallFn = func(method string, raw []byte) ([]byte, error) {
		if method == "host.http.do_stream" {
			return okEnvelope(map[string]any{"status_code": 200, "stream_id": "s"})
		}
		if method == "host.http.stream_read" {
			return okEnvelope(map[string]any{"error": "do-not-leak", "done": true})
		}
		return okEnvelope(map[string]any{})
	}
	req, _ := http.NewRequest(http.MethodPost, endpointChat, strings.NewReader(`{}`))
	resp, err := doHostChatRequest(req, "callback")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || strings.Contains(fmt.Sprint(err), "do-not-leak") {
		t.Fatal("host stream error leaked or vanished")
	}
}

func TestLoginRPCClientsKeepIsolatedCookiesAndCurrentCallback(t *testing.T) {
	previous := hostCallFn
	schema := hostSchema.Load()
	hostSchema.Store(10)
	defer func() { hostCallFn = previous; hostSchema.Store(schema) }()
	var mu sync.Mutex
	var current string
	calls := 0
	hostCallFn = func(method string, raw []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "host.http.operation_open":
			return okEnvelope(map[string]any{"operation_id": "op"})
		case "host.http.do_stream":
			var req struct {
				HostCallbackID string `json:"host_callback_id"`
				Headers        http.Header
			}
			_ = json.Unmarshal(raw, &req)
			calls++
			current = req.HostCallbackID
			if calls == 2 && (current != "poll-current" || req.Headers.Get("Cookie") != "login=state-one") {
				t.Error("poll lost cookie or reused old callback")
			}
			if calls == 3 && req.Headers.Get("Cookie") != "" {
				t.Error("second login inherited cookies")
			}
			return okEnvelope(map[string]any{"status_code": 200, "stream_id": current, "headers": http.Header{"Set-Cookie": {"login=state-one; Path=/"}}})
		case "host.http.stream_read":
			return okEnvelope(map[string]any{"payload": []byte(`{"code":0,"data":{}}`), "done": true})
		default:
			return okEnvelope(map[string]any{})
		}
	}
	client := rpcHTTPClient(newLoginClient(), []byte(`{"host_callback_id":"start-old"}`))
	for _, next := range []*http.Client{client, rpcHTTPClient(client, []byte(`{"host_callback_id":"poll-current"}`)), rpcHTTPClient(newLoginClient(), []byte(`{"host_callback_id":"second-login"}`))} {
		if _, _, err := doJSON(next, http.MethodGet, endpointAccount, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}
