// Package catalog discovers account-scoped WorkBuddy models and public metadata.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lovingfish/workbuddy-cliproxy/internal/workbuddy"
)

const ConfigURL = workbuddy.BaseURL + "/v3/config"

type Credentials struct{ AccessToken, UID, EnterpriseID, Domain string }
type Options struct {
	Channel       string
	TTL, MaxStale time.Duration
}

func (o Options) normalized() Options {
	if o.Channel == "" {
		o.Channel = "cli"
	}
	if o.TTL <= 0 {
		o.TTL = 5 * time.Minute
	}
	if o.MaxStale <= 0 {
		o.MaxStale = 24 * time.Hour
	}
	return o
}

type Model struct {
	ID          string
	Name        string
	Description string
	Credits     string
	Reasoning   map[string]any
	Public      map[string]any
	Promotions  []map[string]any
	Tier        map[string]any
}
type Snapshot struct {
	Models     []Model
	Channel    string
	FetchedAt  time.Time
	FreshUntil time.Time
	Stale      bool
	index      map[string]int
}
type HTTPError struct{ Status, Code int }

func (e *HTTPError) StatusCode() int   { return e.Status }
func (e *HTTPError) UpstreamCode() int { return e.Code }
func (e *HTTPError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("catalog HTTP %d: %s", e.Status, (&BusinessError{Code: e.Code}).Error())
	}
	return fmt.Sprintf("catalog HTTP %d", e.Status)
}

type BusinessError struct{ Code int }

func (e *BusinessError) UpstreamCode() int { return e.Code }

func (e *BusinessError) Error() string {
	if e.Code == 12403 {
		return "catalog upstream code 12403: WorkBuddy client version was rejected"
	}
	return fmt.Sprintf("catalog upstream code %d", e.Code)
}

type Fetcher func(context.Context, Credentials) ([]byte, error)
type cacheEntry struct {
	mu         sync.Mutex
	snapshot   *Snapshot
	forced     bool
	retryAfter time.Time
	lastError  error
	tokenHash  [32]byte
	lastUsed   time.Time
}
type Manager struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	fetch   Fetcher
	now     func() time.Time
	options Options
}

func NewManager(fetch Fetcher, options Options) *Manager {
	return &Manager{entries: map[string]*cacheEntry{}, fetch: fetch, now: time.Now, options: options.normalized()}
}

// Reconfigure preserves a fresh cache when unrelated plugin settings change.
func (m *Manager) Reconfigure(options Options) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.options = options.normalized()
}

// Invalidate is explicit; maintenance can request discovery without changing TTL.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	entries := make([]*cacheEntry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		e.forced = true
		e.mu.Unlock()
	}
}
func scopeKey(c Credentials, channel string) string {
	sum := sha256.Sum256([]byte(c.UID + "\x00" + c.EnterpriseID + "\x00" + c.Domain + "\x00" + channel))
	return hex.EncodeToString(sum[:])
}

// Get returns a defensive copy for callers that need to modify the catalog.
func (m *Manager) Get(ctx context.Context, c Credentials) (Snapshot, error) {
	s, err := m.View(ctx, c)
	if err != nil {
		return s, err
	}
	return cloneSnapshot(s), nil
}

// View returns immutable models with an independent freshness wrapper. Callers
// must not modify the Models slice or nested metadata. Used on the chat hot path.
func (m *Manager) View(ctx context.Context, c Credentials) (Snapshot, error) {
	if c.AccessToken == "" || c.UID == "" {
		return Snapshot{}, fmt.Errorf("catalog requires an authenticated account")
	}
	m.mu.Lock()
	options := m.options
	key := scopeKey(c, options.Channel)
	// Bound abandoned accounts/channels as well as token rotations.
	now := m.now()
	for k, e := range m.entries {
		if now.Sub(e.lastUsed) > options.MaxStale {
			delete(m.entries, k)
		}
	}
	if len(m.entries) >= 256 && m.entries[key] == nil {
		var oldest string
		var when time.Time
		for k, e := range m.entries {
			if oldest == "" || e.lastUsed.Before(when) {
				oldest = k
				when = e.lastUsed
			}
		}
		delete(m.entries, oldest)
	}
	entry := m.entries[key]
	if entry == nil {
		entry = &cacheEntry{}
		m.entries[key] = entry
	}
	entry.lastUsed = now
	m.mu.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	tokenHash := sha256.Sum256([]byte(c.AccessToken))
	if entry.tokenHash != tokenHash {
		entry.tokenHash = tokenHash
		entry.forced = true
		entry.snapshot = nil
	}
	if entry.snapshot != nil && !entry.forced && now.Before(entry.snapshot.FreshUntil) {
		return *entry.snapshot, nil
	}
	if !entry.forced && now.Before(entry.retryAfter) {
		if entry.snapshot != nil && now.Sub(entry.snapshot.FetchedAt) <= options.MaxStale {
			result := *entry.snapshot
			result.Stale = true
			return result, nil
		}
		return Snapshot{}, entry.lastError
	}
	raw, err := m.fetch(ctx, c)
	if err == nil {
		var snapshot Snapshot
		snapshot, err = Parse(raw, options.Channel, now)
		if err == nil {
			snapshot.FreshUntil = now.Add(options.TTL)
			entry.snapshot = &snapshot
			entry.forced = false
			entry.retryAfter = time.Time{}
			entry.lastError = nil
			return snapshot, nil
		}
	}
	denied := false
	var e *HTTPError
	if errors.As(err, &e) {
		denied = e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	}
	entry.lastError = err
	entry.retryAfter = now.Add(30 * time.Second)
	entry.forced = false
	if denied {
		entry.snapshot = nil
	}
	if !denied && entry.snapshot != nil && now.Sub(entry.snapshot.FetchedAt) <= options.MaxStale {
		result := *entry.snapshot
		result.Stale = true
		return result, nil
	}
	return Snapshot{}, err
}
func HTTPFetcher(client *http.Client, endpoint string) Fetcher {
	return func(ctx context.Context, c Credentials) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("create catalog request: %w", err)
		}
		workbuddy.SetClientHeaders(req)
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
		req.Header.Set("X-User-Id", c.UID)
		req.Header.Set("Accept", "application/json")
		if c.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", c.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if c.Domain != "" {
			req.Header.Set("X-Domain", c.Domain)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch catalog: %w", err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
		if err != nil {
			return nil, fmt.Errorf("read catalog: %w", err)
		}
		if len(raw) > 8*1024*1024 {
			return nil, fmt.Errorf("catalog exceeds response size limit")
		}
		if response.StatusCode != http.StatusOK {
			var failure struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(raw, &failure)
			return nil, &HTTPError{Status: response.StatusCode, Code: failure.Code}
		}
		return raw, nil
	}
}

var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)

func Parse(raw []byte, channel string, now time.Time) (Snapshot, error) {
	var envelope struct {
		Code *int `json:"code"`
		Data struct {
			Models []map[string]any `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
			Promotions []map[string]any `json:"modelPromotions"`
			Tiers      []map[string]any `json:"modelTiers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Snapshot{}, fmt.Errorf("decode catalog JSON: %w", err)
	}
	if envelope.Code == nil || *envelope.Code != 0 {
		if envelope.Code != nil && (*envelope.Code == 401 || *envelope.Code == 403) {
			return Snapshot{}, &HTTPError{Status: *envelope.Code}
		}
		if envelope.Code != nil {
			return Snapshot{}, &BusinessError{Code: *envelope.Code}
		}
		return Snapshot{}, fmt.Errorf("catalog response has no business status code")
	}
	if envelope.Data.Models == nil {
		return Snapshot{}, fmt.Errorf("catalog response omitted model definitions; check account authorization or upstream schema")
	}
	var allowed []string
	found := false
	for _, agent := range envelope.Data.Agents {
		if agent.Name == channel {
			allowed = agent.Models
			found = true
			break
		}
	}
	if !found {
		return Snapshot{}, fmt.Errorf("catalog does not publish channel %q", channel)
	}
	pool := map[string]map[string]any{}
	for _, rawModel := range envelope.Data.Models {
		id, _ := rawModel["id"].(string)
		if !modelIDPattern.MatchString(id) {
			continue
		}
		if _, exists := pool[id]; exists {
			return Snapshot{}, fmt.Errorf("catalog has duplicate model %q", id)
		}
		pool[id] = rawModel
	}
	snapshot := Snapshot{Channel: channel, FetchedAt: now.UTC(), Models: []Model{}, index: map[string]int{}}
	seen := map[string]bool{}
	for _, id := range allowed {
		model, ok := pool[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		if model["disabled"] == true || model["enabled"] == false {
			continue
		}
		// A channel may also publish image-generation or completion-only entries.
		if containsID(model["tags"], "text-to-image") || containsID(model["tags"], "embedding") ||
			strings.HasPrefix(id, "completion-") || strings.HasPrefix(id, "hunyuan-image-") {
			continue
		}
		item := Model{ID: id, Name: id, Public: map[string]any{}}
		if name, ok := model["name"].(string); ok && name != "" {
			item.Name = name
		}
		item.Description, _ = model["description"].(string)
		item.Credits, _ = model["credits"].(string)
		item.Reasoning = publicReasoning(model["reasoning"])
		for _, key := range []string{"supportsImages", "supportsReasoning", "onlyReasoning", "disabledMultimodal", "configurable", "configured", "isDefault", "contextLength", "maxCompletionTokens", "inputTokenLimit", "outputTokenLimit", "tags"} {
			if value, ok := model[key]; ok {
				item.Public[key] = cloneValue(value)
			}
		}
		item.Promotions = matchingPromotions(model["promotions"], envelope.Data.Promotions, id)
		if tier, ok := model["tier"].(map[string]any); ok {
			item.Tier = publicRule(tier)
		} else {
			var selected map[string]any
			best := -1.0
			for _, tier := range envelope.Data.Tiers {
				if tier["enabled"] == false || !containsID(tier["modelIds"], id) {
					continue
				}
				priority, _ := tier["priority"].(float64)
				if selected == nil || priority > best {
					selected = tier
					best = priority
				}
			}
			if selected != nil {
				item.Tier = publicRule(selected)
			}
		}
		snapshot.index[item.ID] = len(snapshot.Models)
		snapshot.Models = append(snapshot.Models, item)
	}
	return snapshot, nil
}
func publicReasoning(value any) map[string]any {
	source, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	result := map[string]any{}
	for _, key := range []string{"effort", "defaultEffort", "supportedEfforts", "canDisableThinking", "summary"} {
		if value, ok := source[key]; ok {
			result[key] = cloneValue(value)
		}
	}
	return result
}
func publicRule(source map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"id", "name", "kind", "enabled", "modelIds", "badge", "discount", "priority", "startTime", "endTime", "startAt", "endAt", "conditions", "condition", "credits", "label", "description"} {
		if value, ok := source[key]; ok {
			result[key] = cloneValue(value)
		}
	}
	return result
}
func containsID(value any, id string) bool {
	list, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if item == id {
			return true
		}
	}
	return false
}
func matchingPromotions(local any, global []map[string]any, id string) []map[string]any {
	var result []map[string]any
	if list, ok := local.([]any); ok {
		for _, item := range list {
			if p, ok := item.(map[string]any); ok {
				result = append(result, publicRule(p))
			}
		}
	}
	for _, item := range global {
		if containsID(item["modelIds"], id) {
			result = append(result, publicRule(item))
		}
	}
	return result
}
func cloneValue(value any) any {
	raw, _ := json.Marshal(value)
	var result any
	_ = json.Unmarshal(raw, &result)
	return result
}
func cloneSnapshot(value Snapshot) Snapshot {
	result := value
	result.Models = make([]Model, len(value.Models))
	for i, model := range value.Models {
		result.Models[i] = model
		result.Models[i].Public = cloneMap(model.Public)
		result.Models[i].Reasoning = cloneMap(model.Reasoning)
		result.Models[i].Tier = cloneMap(model.Tier)
		result.Models[i].Promotions = make([]map[string]any, len(model.Promotions))
		for j, p := range model.Promotions {
			result.Models[i].Promotions[j] = cloneMap(p)
		}
	}
	return result
}
func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for k, v := range value {
		result[k] = copyValue(v)
	}
	return result
}
func copyValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = copyValue(item)
		}
		return out
	default:
		return value
	}
}
func (s Snapshot) Find(id string) (Model, bool) {
	if s.index != nil {
		i, ok := s.index[id]
		if !ok {
			return Model{}, false
		}
		return s.Models[i], true
	}
	for _, m := range s.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}
func (m Model) SupportedEfforts() []string {
	var result []string
	for _, item := range asSlice(m.Reasoning["supportedEfforts"]) {
		if s, ok := item.(string); ok && s != "" {
			result = append(result, s)
		}
	}
	return result
}
func asSlice(value any) []any { items, _ := value.([]any); return items }
func (m Model) DefaultEffort() string {
	if effort, ok := m.Reasoning["defaultEffort"].(string); ok && effort != "" {
		return effort
	}
	effort, _ := m.Reasoning["effort"].(string)
	return effort
}
func (m Model) ApplyReasoning(body map[string]any) error {
	return m.ApplyReasoningWithPolicy(body, ReasoningCompatible)
}

func (m Model) ApplyReasoningWithPolicy(body map[string]any, policy ReasoningPolicy) error {
	if policy == "" {
		policy = ReasoningCompatible
	}
	if !policy.Valid() {
		return fmt.Errorf("invalid reasoning effort policy %q", policy)
	}
	value, present := body["reasoning_effort"]
	if !present {
		if reasoning, ok := body["reasoning"].(map[string]any); ok {
			value, present = reasoning["effort"]
		}
	}
	if !present {
		if thinking, ok := body["thinking"].(map[string]any); ok && thinking["type"] == "disabled" {
			value, present = "none", true
		}
	}
	if !present {
		if effort := m.DefaultEffort(); effort != "" {
			body["reasoning_effort"] = effort
		}
		return nil
	}
	effort, ok := value.(string)
	if !ok || strings.TrimSpace(effort) == "" {
		return fmt.Errorf("reasoning_effort must be a non-empty string")
	}
	if effort == "none" {
		if disable, known := m.Reasoning["canDisableThinking"].(bool); known && !disable {
			return fmt.Errorf("model %s does not allow disabling thinking", m.ID)
		}
	} else if levels := m.SupportedEfforts(); len(levels) > 0 {
		if !supportsEffort(levels, effort) {
			target, compatible := compatibleEffort(effort, levels)
			if policy == ReasoningStrict || !compatible {
				return fmt.Errorf("reasoning_effort %q is not advertised for model %s (supported: %s; omit the field to use the model default)", effort, m.ID, strings.Join(levels, ", "))
			}
			effort = target
		}
	}
	body["reasoning_effort"] = effort
	// Do not send the original, conflicting Responses-style effort alongside
	// the normalized Chat Completions field. Preserve other reasoning settings.
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		delete(reasoning, "effort")
		if len(reasoning) == 0 {
			delete(body, "reasoning")
		}
	}
	return nil
}
func (s Snapshot) PublicFields(model Model, now time.Time, policies ...ReasoningPolicy) map[string]any {
	fields := map[string]any{
		"name": model.Name, "display_name": model.Name,
		"catalog": map[string]any{"provider": "workbuddy", "channel": s.Channel, "source": ConfigURL,
			"fetched_at": s.FetchedAt.Format(time.RFC3339Nano), "stale": s.Stale || (!s.FreshUntil.IsZero() && !now.Before(s.FreshUntil)),
			"availability": "listed_by_upstream"},
	}
	if model.Description != "" {
		fields["description"] = model.Description
	}
	if model.Credits != "" {
		fields["credits"] = model.Credits
	}
	if len(model.Reasoning) > 0 {
		fields["reasoning"] = cloneValue(model.Reasoning)
	}
	if levels := model.SupportedEfforts(); len(levels) > 0 {
		fields["supported_reasoning_efforts"] = levels
		policy := ReasoningCompatible
		if len(policies) > 0 && policies[0] == ReasoningStrict {
			policy = ReasoningStrict
		}
		fields["reasoning_effort_policy"] = string(policy)
		if policy == ReasoningCompatible {
			if mapping := model.compatibleEffortMapping(); len(mapping) > 0 {
				fields["reasoning_effort_mapping"] = mapping
			}
		}
	}
	if effort := model.DefaultEffort(); effort != "" {
		fields["default_reasoning_effort"] = effort
	}
	pricing := map[string]any{"source": "account_channel_catalog", "unit": "credit_multiplier"}
	if model.Credits != "" {
		pricing["display"] = model.Credits
	}
	if len(model.Promotions) > 0 {
		pricing["promotions"] = cloneValue(model.Promotions)
	}
	if model.Tier != nil {
		pricing["tier"] = cloneValue(model.Tier)
	}
	fields["pricing"] = pricing
	for _, pair := range [][2]string{{"supportsReasoning", "supports_reasoning"}, {"supportsImages", "supports_images"}, {"onlyReasoning", "only_reasoning"}, {"contextLength", "context_length"}, {"maxCompletionTokens", "max_completion_tokens"}} {
		if value, ok := model.Public[pair[0]]; ok {
			fields[pair[1]] = cloneValue(value)
		}
	}
	return fields
}
