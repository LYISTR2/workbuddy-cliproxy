package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// normalizeToolChoice preserves a forced function by limiting the offered tools
// to that function and using the upstream's string-valued "required" choice.
func normalizeToolChoice(body map[string]any) error {
	value, present := body["tool_choice"]
	if !present || value == nil {
		return nil
	}
	if choice, ok := value.(string); ok {
		switch choice {
		case "auto", "none", "required":
			return nil
		}
		return &requestError{status: 400, message: "unsupported tool_choice value"}
	}
	choice, ok := value.(map[string]any)
	if !ok || choice["type"] != "function" {
		return &requestError{status: 400, message: "unsupported tool_choice object"}
	}
	function, _ := choice["function"].(map[string]any)
	name, _ := function["name"].(string)
	if name == "" {
		return &requestError{status: 400, message: "forced tool_choice requires a function name"}
	}
	entries, _ := body["tools"].([]any)
	for _, entry := range entries {
		tool, ok := entry.(map[string]any)
		if !ok || tool["type"] != "function" {
			continue
		}
		candidate, _ := tool["function"].(map[string]any)
		if candidate["name"] == name {
			body["tools"] = []any{entry}
			body["tool_choice"] = "required"
			return nil
		}
	}
	return &requestError{status: 400, message: fmt.Sprintf("forced function %q is missing from tools", name)}
}

func mergeToolCallDelta(calls map[int]map[string]any, delta map[string]any, fallback int, known ...[]string) {
	index := fallback
	if value, ok := delta["index"].(float64); ok {
		index = int(value)
	}
	if index < 0 {
		return
	}
	target := calls[index]
	if target == nil {
		target = map[string]any{"type": "function", "function": map[string]any{}}
		calls[index] = target
	}
	for _, key := range []string{"id", "type"} {
		if value, ok := delta[key].(string); ok && value != "" {
			target[key] = value
		}
	}
	incoming, ok := delta["function"].(map[string]any)
	if !ok {
		return
	}
	function := target["function"].(map[string]any)
	if name, ok := incoming["name"].(string); ok && name != "" {
		current, _ := function["name"].(string)
		function["name"] = mergeToolName(current, name, known...)
	}
	if arguments, ok := incoming["arguments"].(string); ok {
		builder, _ := function["arguments"].(*strings.Builder)
		if builder == nil {
			builder = new(strings.Builder)
			function["arguments"] = builder
		}
		builder.WriteString(arguments)
	}

}

func orderedToolCalls(calls map[int]map[string]any) []map[string]any {
	indices := make([]int, 0, len(calls))
	for index := range calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	result := make([]map[string]any, 0, len(indices))
	for _, index := range indices {
		call := calls[index]
		function := call["function"].(map[string]any)
		if builder, ok := function["arguments"].(*strings.Builder); ok {
			function["arguments"] = builder.String()
		}
		result = append(result, call)
	}
	return result
}

// The first name is normally complete. Known offered tool names disambiguate
// repeated complete/cumulative names from true fragments (including "ab"+"ab").
// Without a tool schema, preserve the first complete name instead of guessing.
func mergeToolName(current, incoming string, known ...[]string) string {
	if current == "" {
		return incoming
	}
	if len(known) == 0 || len(known[0]) == 0 {
		return current
	}
	for _, name := range known[0] {
		if name == current {
			return current
		}
	}
	for _, name := range known[0] {
		if name == incoming && strings.HasPrefix(incoming, current) {
			return incoming
		}
	}
	for _, name := range known[0] {
		if strings.HasPrefix(name, current+incoming) {
			return current + incoming
		}
	}
	return current
}
func offeredToolNames(raw []byte) []string {
	var body struct {
		Tools []struct{ Function struct{ Name string } }
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	names := make([]string, 0, len(body.Tools))
	for _, tool := range body.Tools {
		if tool.Function.Name != "" {
			names = append(names, tool.Function.Name)
		}
	}
	return names
}
