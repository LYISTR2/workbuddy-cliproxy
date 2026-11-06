package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxSSEEventBytes = 4 * 1024 * 1024

var errSSEDone = errors.New("SSE stream completed")

// splitSSELines accepts LF, CRLF and CR, including CRLF split across reads.
func splitSSELines(data []byte, atEOF bool) (int, []byte, error) {
	for i, value := range data {
		if value == '\n' {
			return i + 1, data[:i], nil
		}
		if value == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance := i + 1
			if advance < len(data) && data[advance] == '\n' {
				advance++
			}
			return advance, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// readSSEData dispatches only complete data events. Comments, event names, IDs
// and retry fields are transport metadata, never JSON completion payloads.
func readSSEData(r io.Reader, emit func(string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Split(splitSSELines)
	scanner.Buffer(make([]byte, 64*1024), maxSSEEventBytes)
	var data []string
	size := 0
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if line == "" {
			if len(data) > 0 {
				if err := emit(strings.Join(data, "\n")); err != nil {
					return err
				}
			}
			data = nil
			size = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			value = ""
		}
		if field != "data" {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		size += len(value) + 1
		if size > maxSSEEventBytes {
			return fmt.Errorf("upstream SSE event exceeds size limit")
		}
		data = append(data, value)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read upstream SSE: %w", err)
	}
	// SSE does not dispatch a pending event at EOF without its empty-line delimiter.
	return nil
}

func readCompletionEvents(r io.Reader, consume func(map[string]any) error) error {
	sawChoice, finished := false, false
	err := readSSEData(r, func(data string) error {
		if strings.TrimSpace(data) == "[DONE]" {
			finished = true
			return errSSEDone
		}
		if strings.TrimSpace(data) == "" {
			return nil
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil || chunk == nil {
			return &requestError{status: http.StatusBadGateway, message: "WorkBuddy returned invalid JSON in an SSE data event"}
		}
		if upstreamError, present := chunk["error"]; present && upstreamError != nil {
			return &requestError{status: http.StatusBadGateway, message: "WorkBuddy returned an error in the upstream stream"}
		}
		if code, ok := chunk["code"].(float64); ok && code != 0 {
			return &requestError{status: http.StatusBadGateway, message: fmt.Sprintf("WorkBuddy stream returned upstream code %.0f", code)}
		}
		choices, hasChoices := chunk["choices"].([]any)
		if !hasChoices {
			if _, present := chunk["choices"]; present {
				return &requestError{status: http.StatusBadGateway, message: "WorkBuddy returned invalid completion choices"}
			}
			if chunk["usage"] == nil {
				return nil
			}
			chunk["choices"] = []any{}
		}
		for _, entry := range choices {
			choice, ok := entry.(map[string]any)
			if !ok {
				return &requestError{status: http.StatusBadGateway, message: "WorkBuddy returned an invalid completion choice"}
			}
			sawChoice = true
			if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
				finished = true
			}
		}
		return consume(chunk)
	})
	if err != nil && !errors.Is(err, errSSEDone) {
		return err
	}
	if !sawChoice {
		return &requestError{status: http.StatusBadGateway, message: "WorkBuddy returned no completion choices"}
	}
	if !finished {
		return &requestError{status: http.StatusBadGateway, message: "WorkBuddy stream ended before a completion finish event"}
	}
	return nil
}

func formatStreamPayload(payload string, sseFramed bool) []byte {
	if sseFramed {
		return []byte("data: " + payload + "\n\n")
	}
	return []byte(payload)
}

// forwardCompletionStream is shared by the async pump and synchronous fallback.
func forwardCompletionStream(r io.Reader, sseFramed bool, emit func([]byte) error) error {
	return readCompletionEvents(r, func(chunk map[string]any) error {
		payload, err := cleanCompletionChunk(chunk)
		if err != nil {
			return err
		}
		return emit(formatStreamPayload(payload, sseFramed))
	})
}

func cleanCompletionChunk(chunk map[string]any) (string, error) {
	if choices, ok := chunk["choices"].([]any); ok {
		for _, entry := range choices {
			choice, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if choice["finish_reason"] == "" {
				choice["finish_reason"] = nil
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				for key, value := range delta {
					if isEmptyValue(value) {
						delete(delta, key)
					}
				}
			}
		}
	}
	raw, err := json.Marshal(chunk)
	return string(raw), err
}
