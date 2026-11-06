//go:build cgo

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatStreamOutlivesTheGenericClientTotalTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", fixtureChunk(map[string]any{"role": "assistant"}, ""))
		flusher.Flush()
		for i := 0; i < 3; i++ {
			time.Sleep(40 * time.Millisecond)
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", fixtureChunk(map[string]any{"content": "long reasoning completed"}, "stop"))
	}))
	defer server.Close()
	previous := sharedHTTPClient()
	sharedClient = &http.Client{Timeout: 10 * time.Millisecond, Transport: server.Client().Transport}
	t.Cleanup(func() { sharedClient = previous })
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	response, err := doChatRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := aggregateCompletion(response.Body, "fixture")
	if err != nil || !strings.Contains(string(raw), "long reasoning completed") {
		t.Fatalf("active stream was cut off: %s %v", raw, err)
	}
}

func TestIdleStreamBodyClosesAStalledRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := newIdleStreamBody(reader, 20*time.Millisecond)
	defer body.Close()
	completed := make(chan error, 1)
	go func() { _, err := body.Read(make([]byte, 1)); completed <- err }()
	select {
	case err := <-completed:
		if !errors.Is(err, errChatReadIdle) {
			t.Fatalf("wrong idle error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled stream was not closed")
	}
}

func TestIdleStreamDeadlineIsResetByHeartbeats(t *testing.T) {
	reader, writer := io.Pipe()
	body := newIdleStreamBody(reader, time.Second)
	defer body.Close()
	go func() {
		defer writer.Close()
		for i := 0; i < 60; i++ {
			if _, err := writer.Write([]byte(":")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	raw, err := io.ReadAll(body)
	if err != nil || len(raw) != 60 {
		t.Fatalf("active heartbeats did not reset idle timeout: bytes=%d error=%v", len(raw), err)
	}
}
