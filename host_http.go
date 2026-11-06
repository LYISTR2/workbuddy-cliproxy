package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// These additive RPC fields match CPA v8.0.56. Keep the v7 SDK for ABI and
// credential compatibility; older hosts ignore the quota/display capabilities.
var hostSchema atomic.Uint32

type hostCallbackKey struct{}

func withHostCallback(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, hostCallbackKey{}, id)
}
func hostRPC(method string, request any, result any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	response, callErr := hostCallFn(method, raw)
	var env envelope
	if err := json.Unmarshal(response, &env); err != nil {
		return fmt.Errorf("CPA host callback failed")
	}
	if !env.OK {
		status := 502
		if env.Error != nil && env.Error.HTTPStatus >= 400 {
			status = env.Error.HTTPStatus
		}
		return &requestError{status: status, message: "CPA host HTTP operation failed"}
	}
	if callErr != nil {
		return fmt.Errorf("CPA host callback failed")
	}
	if result != nil {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("invalid CPA host callback response")
		}
	}
	return nil
}

// Route chat through CPA's transport. Its callback context carries downstream
// cancellation and proxy policy, including while the upstream sends heartbeats.
// A local operation cancellation also closes the host HTTP stream on unload.
func doHostChatRequest(req *http.Request, callback string) (*http.Response, error) {
	var operation struct {
		ID string `json:"operation_id"`
	}
	if hostSchema.Load() >= 10 {
		if err := hostRPC("host.http.operation_open", map[string]string{"host_callback_id": callback}, &operation); err != nil {
			return nil, err
		}
	}
	var cancelOnce sync.Once
	cancelOperation := func() {
		cancelOnce.Do(func() {
			if operation.ID != "" {
				_ = hostRPC("host.http.cancel", map[string]string{"host_callback_id": callback, "operation_id": operation.ID}, nil)
			}
		})
	}
	stop := context.AfterFunc(req.Context(), cancelOperation)
	var body []byte
	var err error
	if req.Body != nil {
		body, err = io.ReadAll(io.LimitReader(req.Body, 32<<20+1))
	}
	if err != nil || len(body) > 32<<20 {
		stop()
		cancelOperation()
		return nil, fmt.Errorf("invalid outbound body")
	}
	var opened struct {
		StatusCode int         `json:"status_code"`
		Headers    http.Header `json:"headers"`
		StreamID   string      `json:"stream_id"`
	}
	err = hostRPC("host.http.do_stream", map[string]any{"host_callback_id": callback, "operation_id": operation.ID, "method": req.Method, "url": req.URL.String(), "headers": req.Header, "body": body}, &opened)
	if err != nil {
		stop()
		cancelOperation()
		return nil, err
	}
	if opened.StreamID == "" {
		stop()
		cancelOperation()
		return nil, fmt.Errorf("CPA host returned no HTTP stream")
	}
	reader := &hostStreamBody{id: opened.StreamID, callback: callback, cancel: cancelOperation, stopOperation: stop}
	reader.setStop(context.AfterFunc(req.Context(), func() { _ = reader.Close() }))
	return &http.Response{StatusCode: opened.StatusCode, Header: opened.Headers, Body: reader, Request: req}, nil
}

type hostStreamBody struct {
	id, callback  string
	pending       []byte // Only Read accesses these fields.
	terminal      error
	once          sync.Once
	mu            sync.Mutex
	closed        bool
	stop          func() bool
	stopOperation func() bool
	cancel        func()
}

func (b *hostStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.terminal != nil {
			return 0, b.terminal
		}
		var chunk struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		err := hostRPC("host.http.stream_read", map[string]string{"stream_id": b.id}, &chunk)
		if err != nil {
			b.terminal = err
			return 0, err
		}
		b.pending = chunk.Payload
		if chunk.Error != "" {
			b.terminal = fmt.Errorf("CPA upstream HTTP stream failed")
		} else if chunk.Done {
			b.terminal = io.EOF
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *hostStreamBody) Close() error {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		stop := b.stop
		b.mu.Unlock()
		if b.stopOperation != nil {
			b.stopOperation()
		}
		if stop != nil {
			stop()
		}
		_ = hostRPC("host.http.stream_close", map[string]string{"stream_id": b.id}, nil)
		b.cancel()
	})
	return nil
}

func (b *hostStreamBody) setStop(stop func() bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		stop()
	} else {
		b.stop = stop
	}
}

// Each RPC owns a different host callback. Reuse only the state-specific cookie
// jar, never a callback from an earlier login poll.
type callbackTransport struct{ callback string }

func (t callbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return doHostChatRequest(req, t.callback)
}
func rpcHTTPClient(base *http.Client, raw []byte) *http.Client {
	var request struct {
		Callback string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Callback == "" {
		return base
	}
	return &http.Client{Timeout: base.Timeout, Jar: base.Jar, Transport: callbackTransport{request.Callback}, CheckRedirect: refuseRedirect}
}
