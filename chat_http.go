package main

import (
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
	response, err := client.Do(req)
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
