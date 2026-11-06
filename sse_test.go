//go:build cgo

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func fixtureChunk(delta map[string]any, finish any) string {
	raw, _ := json.Marshal(map[string]any{"id": "fixture-chat", "model": "deepseek-v4.1-flash", "object": "chat.completion.chunk", "created": 1793929110, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}}, "usage": nil})
	return string(raw)
}

func heartbeatStream() string {
	return ": heartbeat\n\nevent: message\nid: transport-only\nretry: 1000\ndata: " + fixtureChunk(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{}}, "") + "\n\n: heartbeat\n\ndata: " + fixtureChunk(map[string]any{"reasoning_content": "The reasoning"}, "") + "\n\ndata: " + fixtureChunk(map[string]any{"content": "heartbeat is valid content"}, "stop") + "\n\ndata: {\"id\":\"fixture-chat\",\"model\":\"deepseek-v4.1-flash\",\"object\":\"chat.completion.chunk\",\"created\":1793929110,\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"
}

type singleByteReader struct{ io.Reader }

func (r singleByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func TestSSEIgnoresControlsAndEmitsValidJSON(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for _, framed := range []bool{false, true} {
			input := strings.ReplaceAll(heartbeatStream(), "\n", ending)
			chunks, err := aggregateSSE(singleByteReader{strings.NewReader(input)}, framed)
			if err != nil || len(chunks) != 4 {
				t.Fatalf("framed=%v ending=%q chunks=%d error=%v", framed, ending, len(chunks), err)
			}
			for i, chunk := range chunks {
				payload := string(chunk.Payload)
				if framed {
					if !strings.HasSuffix(payload, "\n\n") {
						t.Fatal("missing SSE event delimiter")
					}
					var events []string
					if err := readSSEData(strings.NewReader(payload), func(data string) error { events = append(events, data); return nil }); err != nil || len(events) != 1 {
						t.Fatal("invalid framed chunk")
					}
					payload = events[0]
				}
				var parsed map[string]any
				if json.Unmarshal([]byte(payload), &parsed) != nil {
					t.Fatalf("control line leaked as JSON: %q", payload)
				}
				if i == 0 {
					choice := parsed["choices"].([]any)[0].(map[string]any)
					if choice["finish_reason"] != nil {
						t.Fatal("empty finish reason was not normalized to null")
					}
				}
			}
		}
	}
}

func TestSSEMultilineDataAndBOMSurviveFragmentedReads(t *testing.T) {
	input := "\ufeff: comment\r\nevent: message\r\ndata: {\r\ndata:   \"choices\": [{\"delta\": {\"content\": \"你好\"}, \"finish_reason\": \"stop\"}]\r\ndata: }\r\n\r\n"
	chunks, err := aggregateSSE(singleByteReader{strings.NewReader(input)}, false)
	if err != nil || len(chunks) != 1 || !strings.Contains(string(chunks[0].Payload), "你好") {
		t.Fatalf("multiline event lost: %v %v", chunks, err)
	}
}

func TestCompletionAggregationPreservesReasoningTextAndUsage(t *testing.T) {
	raw, err := aggregateCompletion(strings.NewReader(heartbeatStream()), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content   string
				Reasoning string `json:"reasoning_content"`
			}
			Finish string `json:"finish_reason"`
		}
		Usage map[string]int
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Choices[0].Message.Content != "heartbeat is valid content" || result.Choices[0].Message.Reasoning != "The reasoning" || result.Choices[0].Finish != "stop" || result.Usage["completion_tokens"] != 7 {
		t.Fatalf("completion changed: %s", raw)
	}
}

func TestSSEStopsAtDoneAndAcceptsFinishedEOF(t *testing.T) {
	finished := "data: " + fixtureChunk(map[string]any{"content": "done"}, "stop") + "\n\n"
	for _, suffix := range []string{"", "data: [DONE]\n\ndata: invalid trailing data\n\n"} {
		chunks, err := aggregateSSE(strings.NewReader(finished+suffix), false)
		if err != nil || len(chunks) != 1 {
			t.Fatalf("incorrect stream termination: %v %v", chunks, err)
		}
	}
}

func TestSSERejectsMalformedErrorAndUnfinishedStreams(t *testing.T) {
	role := "data: " + fixtureChunk(map[string]any{"role": "assistant"}, "") + "\n\n"
	for _, input := range []string{
		": heartbeat\n\n",
		"data: not JSON\n\n",
		"data: {\"error\":{\"message\":\"fixture failure\"}}\n\n",
		"data: {\"code\":12403}\n\n",
		"data: {\"choices\":null}\n\n",
		role,
		role + "data: {\"choices\":[",
	} {
		if _, err := aggregateSSE(strings.NewReader(input), false); err == nil {
			t.Fatalf("accepted malformed or incomplete stream: %q", input)
		}
		if _, err := aggregateCompletion(strings.NewReader(input), "fixture"); err == nil {
			t.Fatal("non-streaming aggregation swallowed stream failure")
		}
	}
}

type failingSSEReader struct{ err error }

func (r failingSSEReader) Read([]byte) (int, error) { return 0, r.err }

func TestSSEPropagatesReadFailuresAndEmitterCancellation(t *testing.T) {
	want := errors.New("fixture read failure")
	r := io.MultiReader(strings.NewReader("data: "+fixtureChunk(map[string]any{"role": "assistant"}, "")+"\n\n"), failingSSEReader{want})
	if err := forwardCompletionStream(r, false, func([]byte) error { return nil }); !errors.Is(err, want) {
		t.Fatalf("read error was hidden: %v", err)
	}
	cancel := errors.New("fixture client disconnected")
	if err := forwardCompletionStream(strings.NewReader(heartbeatStream()), false, func([]byte) error { return cancel }); !errors.Is(err, cancel) {
		t.Fatal("emitter cancellation was ignored")
	}
}

func TestSSEDeliversACompleteEventBeforeUpstreamCloses(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	emitted := make(chan []byte, 1)
	completed := make(chan error, 1)
	go func() {
		completed <- forwardCompletionStream(reader, false, func(payload []byte) error { emitted <- payload; return nil })
	}()
	go func() {
		_, _ = io.WriteString(writer, ": heartbeat\n\ndata: "+fixtureChunk(map[string]any{"content": "live"}, "stop")+"\n\n")
	}()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("event was buffered until upstream closure")
	}
	writer.Close()
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}

func TestSynchronousStreamFallbackReturnsParsingErrors(t *testing.T) {
	setupRuntimeCatalog(t)
	mockWorkbuddyHTTP(t, func(req *http.Request) (int, string) {
		if req.Header.Get("Accept") != "text/event-stream" {
			t.Error("chat request did not request SSE")
		}
		return 200, ": heartbeat\n\ndata: invalid JSON\n\n"
	})
	storage, _ := json.Marshal(testAuth())
	request, _ := json.Marshal(executorStreamRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "fixture", Model: "hy3", StorageJSON: storage, Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}})
	if _, err := handleExecStream(request); err == nil {
		t.Fatal("synchronous fallback hid a malformed SSE event")
	}
}
