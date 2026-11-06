package catalog

import (
	"strings"
	"testing"
	"time"
)

func reasoningModel(levels ...string) Model {
	values := make([]any, len(levels))
	for i, level := range levels {
		values[i] = level
	}
	return Model{ID: "deepseek-v4.1-flash", Reasoning: map[string]any{"defaultEffort": "high", "supportedEfforts": values}}
}

func TestCompatibilityMapsOnlyUnsupportedStandardEfforts(t *testing.T) {
	for _, test := range []struct {
		request, expected string
		levels            []string
	}{
		{"medium", "high", []string{"low", "high", "max"}},
		{"xhigh", "max", []string{"low", "high", "max"}},
		{"max", "xhigh", []string{"low", "high", "xhigh"}},
		{"low", "high", []string{"high"}},
		{"low", "low", []string{"low", "high", "max"}},
		{"max", "max", []string{"low", "high", "max"}},
		{"medium", "medium", nil}, // No advertised set: preserve the original value.
	} {
		body := map[string]any{"reasoning_effort": test.request}
		if err := reasoningModel(test.levels...).ApplyReasoning(body); err != nil || body["reasoning_effort"] != test.expected {
			t.Fatalf("%s with %v: body=%v error=%v", test.request, test.levels, body, err)
		}
	}
}

func TestStrictPolicyStillRejectsUnadvertisedEfforts(t *testing.T) {
	model := reasoningModel("low", "high", "max")
	body := map[string]any{"reasoning_effort": "medium"}
	err := model.ApplyReasoningWithPolicy(body, ReasoningStrict)
	if err == nil || !strings.Contains(err.Error(), "supported: low, high, max") || body["reasoning_effort"] != "medium" {
		t.Fatalf("strict policy changed or accepted request: %v %v", body, err)
	}
}

func TestCompatibilityDoesNotEnableForbiddenThinkingOrAcceptTypos(t *testing.T) {
	model := reasoningModel("low", "high", "max")
	model.Reasoning["canDisableThinking"] = false
	for _, value := range []any{"none", "typo", 42, nil, ""} {
		if err := model.ApplyReasoning(map[string]any{"reasoning_effort": value}); err == nil {
			t.Fatalf("unexpectedly accepted %v", value)
		}
	}
	model.Reasoning["canDisableThinking"] = true
	body := map[string]any{"reasoning_effort": "none"}
	if err := model.ApplyReasoning(body); err != nil || body["reasoning_effort"] != "none" {
		t.Fatal("explicit disable was changed")
	}
}

func TestNestedReasoningEffortIsNormalizedWithoutConflictingFields(t *testing.T) {
	model := reasoningModel("low", "high", "max")
	for _, summary := range []bool{false, true} {
		reasoning := map[string]any{"effort": "medium"}
		if summary {
			reasoning["summary"] = "auto"
		}
		body := map[string]any{"reasoning": reasoning}
		if err := model.ApplyReasoning(body); err != nil || body["reasoning_effort"] != "high" {
			t.Fatalf("nested effort was not adapted: %v %v", body, err)
		}
		if _, exists := reasoning["effort"]; exists {
			t.Fatal("original effort would also be forwarded")
		}
		if summary && reasoning["summary"] != "auto" {
			t.Fatal("unrelated reasoning option was lost")
		}
		if !summary {
			if _, exists := body["reasoning"]; exists {
				t.Fatal("empty reasoning object was retained")
			}
		}
	}
}

func TestMetadataSeparatesCompatibilityMappingFromNativeLevels(t *testing.T) {
	model := reasoningModel("low", "high", "max")
	snapshot := Snapshot{}
	fields := snapshot.PublicFields(model, time.Now())
	for _, level := range fields["supported_reasoning_efforts"].([]string) {
		if level == "medium" {
			t.Fatal("compatibility alias was advertised as native")
		}
	}
	mapping := fields["reasoning_effort_mapping"].(map[string]string)
	if mapping["medium"] != "high" || mapping["xhigh"] != "max" || fields["reasoning_effort_policy"] != "compatible" {
		t.Fatalf("missing compatibility metadata: %v", fields)
	}
	strict := snapshot.PublicFields(model, time.Now(), ReasoningStrict)
	if _, exists := strict["reasoning_effort_mapping"]; exists {
		t.Fatal("strict mode exposed an active alias mapping")
	}
	if strict["reasoning_effort_policy"] != "strict" {
		t.Fatal("strict policy was not exposed")
	}
}
