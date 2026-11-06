package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const chatReadIdleTimeout = 3 * time.Minute

var errChatReadIdle = errors.New("upstream stream idle timeout")

func chatHeaders(req *http.Request, auth *storedAuth) {
	backendHeaders(req, auth)
	req.Header.Set("Accept", "text/event-stream")
}

// Reasoning streams may remain active beyond the generic client's two-minute
// total timeout. Bound connection/header waits and read inactivity instead.
func doChatRequest(req *http.Request) (*http.Response, error) {
	client := *sharedHTTPClient()
	client.Timeout = 0
	var response *http.Response
	var err error
	if callback, _ := req.Context().Value(hostCallbackKey{}).(string); callback != "" {
		response, err = doHostChatRequest(req, callback)
	} else {
		response, err = client.Do(req)
	}
	if err != nil {
		return nil, err
	}
	response.Body = newIdleStreamBody(response.Body, chatReadIdleTimeout)
	return response, nil
}

type idleStreamBody struct {
	body            io.ReadCloser
	timer           *time.Timer
	idle            time.Duration
	mu              sync.Mutex
	closed, expired bool
}

func newIdleStreamBody(body io.ReadCloser, idle time.Duration) *idleStreamBody {
	result := &idleStreamBody{body: body, idle: idle}
	result.timer = time.AfterFunc(idle, func() {
		result.mu.Lock()
		if result.closed {
			result.mu.Unlock()
			return
		}
		result.expired = true
		result.closed = true
		result.mu.Unlock()
		_ = body.Close()
	})
	return result
}

func (b *idleStreamBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.mu.Lock()
	if b.expired {
		err = fmt.Errorf("WorkBuddy stream was idle for %s: %w", b.idle, errChatReadIdle)
	} else if n > 0 && !b.closed {
		b.timer.Reset(b.idle)
	}
	b.mu.Unlock()
	return n, err
}

func (b *idleStreamBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.timer.Stop()
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.timer.Stop()
	b.mu.Unlock()
	return b.body.Close()
}

// Bound the complete operation as well as read inactivity. The response headers
// are available synchronously on all three execution paths.
func openUpstreamChat(body []byte, sa *storedAuth, callback string) (*http.Response, func(), error) {
	ctx, done, err := startOperation(chatDuration())
	if err != nil {
		return nil, nil, err
	}
	if callback != "" {
		ctx = withHostCallback(ctx, callback)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointChat, bytes.NewReader(body))
	if err != nil {
		done()
		return nil, nil, err
	}
	chatHeaders(req, sa)
	resp, err := doChatRequest(req)
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			err = cause
		}
		done()
		return nil, nil, &requestError{status: errorStatus(err), message: publicError(err)}
	}
	if resp.Request == nil {
		resp.Request = req
	}
	if err := upstreamStatusError(resp); err != nil {
		resp.Body.Close()
		done()
		return nil, nil, err
	}
	return resp, done, nil
}
func upstreamStatusError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Only headers are needed. Never wait for or echo an untrusted error body.
	status := resp.StatusCode
	if status < 400 {
		status = 502
	}
	return &requestError{status: status, message: fmt.Sprintf("WorkBuddy upstream HTTP %d", resp.StatusCode)}
}
