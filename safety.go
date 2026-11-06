package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxJSONResponseBytes = 1 << 20
const workbuddyLogo = "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCA2NCA2NCI+PHJlY3Qgd2lkdGg9IjY0IiBoZWlnaHQ9IjY0IiByeD0iMTYiIGZpbGw9IiMyNTYzZWIiLz48dGV4dCB4PSIzMiIgeT0iNDYiIGZpbGw9IndoaXRlIiBmb250LXNpemU9IjM4IiBmb250LWZhbWlseT0ic2Fucy1zZXJpZiIgdGV4dC1hbmNob3I9Im1pZGRsZSI+VzwvdGV4dD48L3N2Zz4="

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read WorkBuddy response failed")
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("WorkBuddy response exceeds size limit")
	}
	return raw, nil
}
func decodeObject(raw []byte) (map[string]any, error) {
	var value map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil || value == nil {
		return nil, fmt.Errorf("invalid JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON")
	}
	return value, nil
}
func marshalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(value)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), err
}
func authIdentity(sa *storedAuth) (string, string) {
	if strings.TrimSpace(sa.Account.UID) == "" {
		return providerName, authFileName
	}
	sum := sha256.Sum256([]byte(sa.Account.UID + "\x00" + sa.Account.EnterpriseID + "\x00" + sa.Auth.Domain))
	id := providerName + "-" + hex.EncodeToString(sum[:16])
	return id + ".json", id + ".json"
}
func tokenExpiry(seconds int64) int64 {
	// Some token responses omit the lifetime. One hour is a conservative fallback;
	// schedule refresh five minutes early rather than entering a zero-lifetime loop.
	if seconds <= 0 {
		seconds = 3600
	}
	if seconds > 365*24*3600 {
		seconds = 365 * 24 * 3600
	}
	return time.Now().Add(time.Duration(seconds) * time.Second).Unix()
}
func nextRefresh(expiry int64) time.Time {
	if expiry <= 0 {
		return time.Time{}
	}
	return time.Unix(expiry, 0).Add(-5 * time.Minute).UTC()
}
func cleanupLogins(now time.Time) {
	loginStates.Range(func(key, value any) bool {
		if !now.Before(value.(*loginCtx).expires) {
			if loginStates.CompareAndDelete(key, value) && value.(*loginCtx).timer != nil {
				value.(*loginCtx).timer.Stop()
			}
		}
		return true
	})
}
func loginPending(message string) ([]byte, error) {
	return okEnvelope(struct {
		Status  string
		Message string
	}{"pending", message})
}

type upstreamAPIError struct{ Status, Code int }

func (e *upstreamAPIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("WorkBuddy API code=%d (HTTP %d)", e.Code, e.Status)
	}
	return fmt.Sprintf("WorkBuddy API HTTP %d", e.Status)
}
func (e *upstreamAPIError) UpstreamCode() int { return e.Code }
func (e *upstreamAPIError) StatusCode() int {
	if e.Status >= 400 {
		return e.Status
	}
	return 502
}
func terminalLoginError(err error) bool {
	var api *upstreamAPIError
	return errors.As(err, &api) && (api.Status == 401 || api.Status == 403 || api.Code == 401 || api.Code == 403)
}
func errorStatus(err error) int {
	var s interface{ StatusCode() int }
	if errors.As(err, &s) {
		return s.StatusCode()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errChatReadIdle) {
		return 504
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return 502
}
func publicError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "WorkBuddy request exceeded the total duration limit"
	}
	if errors.Is(err, context.Canceled) {
		return "WorkBuddy request was canceled"
	}
	// All upstream network/body errors are created here or in validated parsers;
	// never include transport URLs or untrusted response bodies in host output.
	var req *requestError
	var api *upstreamAPIError
	if errors.As(err, &req) {
		return req.Error()
	}
	if errors.As(err, &api) {
		return api.Error()
	}
	if errors.Is(err, errChatReadIdle) {
		return "WorkBuddy upstream stream idle timeout"
	}
	return "WorkBuddy upstream request failed"
}
func diagnostic(event string, sa *storedAuth, err error, elapsed time.Duration) {
	fields := map[string]any{"plugin": "workbuddy", "event": event, "elapsed_ms": elapsed.Milliseconds()}
	if sa != nil && sa.Account.UID != "" {
		sum := sha256.Sum256([]byte(sa.Account.UID))
		fields["account_hash"] = hex.EncodeToString(sum[:6])
	}
	if err != nil {
		fields["http_status"] = errorStatus(err)
		var code interface{ UpstreamCode() int }
		if errors.As(err, &code) {
			fields["upstream_code"] = code.UpstreamCode()
		}
	}
	raw, _ := json.Marshal(map[string]any{"level": "warn", "message": "WorkBuddy " + event, "fields": fields})
	if _, e := hostCallFn("host.log", raw); e == nil {
		return
	}
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	slog.Warn("WorkBuddy diagnostic", args...)
}

type pluginLifecycle struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

func newLifecycle() *pluginLifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &pluginLifecycle{ctx: ctx, cancel: cancel}
}

var lifecycle = newLifecycle()
var chatMaxSeconds atomic.Int64

func pluginContext() context.Context {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	return lifecycle.ctx
}
func startOperation(limit time.Duration) (context.Context, func(), error) {
	l := lifecycle
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return nil, nil, &requestError{status: 503, message: "WorkBuddy plugin is shutting down"}
	}
	ctx, cancel := context.WithTimeout(l.ctx, limit)
	l.wg.Add(1)
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); l.wg.Done() }) }, nil
}
func chatDuration() time.Duration {
	if n := chatMaxSeconds.Load(); n > 0 {
		return time.Duration(n) * time.Second
	}
	return 30 * time.Minute
}
func shutdownPlugin() {
	l := lifecycle
	l.mu.Lock()
	l.stopped = true
	l.cancel()
	l.mu.Unlock()
	loginStates.Range(func(k, v any) bool {
		if loginStates.CompareAndDelete(k, v) && v.(*loginCtx).timer != nil {
			v.(*loginCtx).timer.Stop()
		}
		return true
	})
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		diagnostic("shutdown_drain_timeout", nil, context.DeadlineExceeded, 0)
	}
	sharedHTTPClient().CloseIdleConnections()
}

func streamCloseFailure(id string, err error) {
	raw, _ := json.Marshal(map[string]any{"stream_id": id, "error": publicError(err), "error_detail": map[string]any{"code": "request_canceled", "http_status": errorStatus(err), "message": publicError(err)}})
	_, _ = hostCallFn("host.stream.close", raw)
}
