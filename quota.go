package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Additive CPA v8 quota-provider RPC contract, kept separate from v7 ABI types.
type quotaRequest struct {
	Provider       string `json:"provider"`
	StorageJSON    []byte `json:"storage_json"`
	HostCallbackID string `json:"host_callback_id"`
}
type quotaResponse struct {
	Groups []quotaGroup `json:"groups"`
}
type quotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []quotaBucket `json:"buckets"`
}
type quotaBucket struct {
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description"`
}

func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req quotaRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Provider != "" && req.Provider != providerName {
		return nil, &requestError{status: 400, message: "unsupported quota provider"}
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	ctx, done, err := startOperation(30 * time.Second)
	if err != nil {
		return nil, err
	}
	defer done()
	endpoint := upstreamBase + "/v2/billing/meter/get-user-resource"
	body := []byte(`{"PageNumber":1,"PageSize":100,"ProductCode":"p_tcaca","Status":[0,3],"OnlyValidPeriod":true}`)
	enterprise := sa.Account.EnterpriseID != ""
	if enterprise {
		endpoint = upstreamBase + "/v2/billing/meter/get-enterprise-user-usage"
		body = []byte(`{}`)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	backendHeaders(request, sa)
	request.Header.Set("Accept", "application/json")
	if enterprise {
		request.Header.Set("X-Tenant-Id", sa.Account.EnterpriseID)
	}
	var resp *http.Response
	if req.HostCallbackID != "" {
		resp, err = doHostChatRequest(request, req.HostCallbackID)
	} else {
		resp, err = sharedHTTPClient().Do(request)
	}
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			err = cause
		}
		diagnostic("quota_failed", sa, err, 0)
		return nil, &requestError{status: errorStatus(err), message: publicError(err)}
	}
	defer resp.Body.Close()
	if err = upstreamStatusError(resp); err != nil {
		diagnostic("quota_failed", sa, err, 0)
		return nil, err
	}
	payload, err := readLimited(resp.Body, maxJSONResponseBytes)
	if err != nil {
		return nil, err
	}
	result, err := normalizeQuota(payload, enterprise, time.Now())
	if err != nil {
		diagnostic("quota_failed", sa, err, 0)
		return nil, err
	}
	return okEnvelope(result)
}

func normalizeQuota(raw []byte, enterprise bool, now time.Time) (quotaResponse, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return quotaResponse{}, fmt.Errorf("invalid WorkBuddy quota JSON")
	}
	data := root
	// Billing responses have appeared with both single and double data envelopes.
	for range 3 {
		if code, ok := number(data["code"]); ok && code != 0 && code != 200 {
			return quotaResponse{}, &upstreamAPIError{Status: 200, Code: int(code)}
		}
		next, ok := data["data"].(map[string]any)
		if !ok {
			break
		}
		data = next
	}
	if enterprise {
		total, hasTotal := firstNumber(data, "limit_num", "limitNum")
		used, hasUsed := firstNumber(data, "used_num", "usedNum", "credit")
		if !hasTotal || !hasUsed {
			return quotaResponse{}, fmt.Errorf("WorkBuddy enterprise quota response lacks limit or usage")
		}
		item := map[string]any{"PackageName": "企业额度", "CycleCapacitySize": total, "CycleCapacityRemain": math.Max(0, total-used), "Unlimited": total == -1}
		for _, k := range []string{"cycle_reset_time", "cycleResetTime", "cycle_end_time", "cycleEndTime"} {
			if data[k] != nil {
				item["CycleResetTime"] = data[k]
				break
			}
		}
		group, err := resourceQuota(item, now)
		if err != nil {
			return quotaResponse{}, err
		}
		return quotaResponse{Groups: []quotaGroup{*group}}, nil
	}
	page := data
	resources, ok := data["resources"].([]any)
	if !ok {
		response, _ := data["Response"].(map[string]any)
		inner, _ := response["Data"].(map[string]any)
		resources, ok = inner["Accounts"].([]any)
		page = inner
	}
	if !ok {
		return quotaResponse{}, fmt.Errorf("WorkBuddy quota response lacks resources/Accounts")
	}
	// Avoid silently treating the first page as a complete account snapshot.
	if total, found := firstNumber(page, "totalCount", "total_count", "TotalCount"); found && total > float64(len(resources)) {
		return quotaResponse{}, fmt.Errorf("WorkBuddy quota response is paginated; more than 100 resources are not supported")
	}
	groups := make([]quotaGroup, 0, len(resources))
	seen := map[string]bool{}
	for _, value := range resources {
		item, ok := value.(map[string]any)
		if !ok {
			return quotaResponse{}, fmt.Errorf("invalid WorkBuddy quota resource")
		}
		if id, ok := item["ResourceId"].(string); ok && id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		group, err := resourceQuota(item, now)
		if err != nil {
			return quotaResponse{}, err
		}
		if group != nil {
			groups = append(groups, *group)
		}
	}
	// An explicitly empty resource list is valid; the UI shows no active packages.
	return quotaResponse{Groups: groups}, nil
}
func resourceQuota(item map[string]any, now time.Time) (*quotaGroup, error) {
	if status, ok := firstNumber(item, "Status", "status"); ok && status != 0 && status != 3 {
		return nil, nil
	}
	expiry := firstQuotaTime(item, "DeductionEndTime", "ExpiredTime", "expire_at", "expireAt")
	if expiry != "" {
		t, _ := time.Parse(time.RFC3339, expiry)
		if !t.After(now) {
			return nil, nil
		}
	}
	name := firstString(item, "PackageName", "name", "package_name", "packageName", "PackageCode")
	if name == "" {
		name = "WorkBuddy 资源包"
	}
	name = cleanLabel(name)
	values := item
	if capacity, _ := firstNumber(item, "CapacityType"); capacity == 4 {
		if slices, ok := item["SlicePeriodUsageDetails"].([]any); ok && len(slices) > 0 {
			if slice, ok := slices[0].(map[string]any); ok {
				values = slice
			}
		}
	}
	total, hasTotal := firstNumber(values, "SlicePeriodCapacitySizePrecise", "CycleCapacitySizePrecise", "CycleCapacitySize", "CapacitySizePrecise", "CapacitySize", "total", "limit_num", "limitNum")
	remain, hasRemain := firstNumber(values, "SlicePeriodCapacityRemainPrecise", "CycleCapacityRemainPrecise", "CycleCapacityRemain", "CapacityRemainPrecise", "CapacityRemain", "remaining", "remain")
	unlimited := item["Unlimited"] == true || item["unlimited"] == true || total == -1
	if unlimited {
		return &quotaGroup{DisplayName: name, Buckets: []quotaBucket{{Window: "credits", RemainingFraction: 1, Description: "不限额（上游标记）"}}}, nil
	}
	if !hasTotal || !hasRemain || total < 0 || remain < 0 {
		return nil, fmt.Errorf("WorkBuddy resource lacks valid total or remaining quota")
	}
	fraction := 0.0
	if total > 0 {
		fraction = math.Max(0, math.Min(1, remain/total))
	}
	reset := firstQuotaTime(item, "CycleResetTime", "CycleEndTime", "cycle_reset_time", "cycleResetTime")
	if reset == "" {
		reset = expiry
	}
	description := fmt.Sprintf("剩余 %s / %s credits", strconv.FormatFloat(remain, 'f', -1, 64), strconv.FormatFloat(total, 'f', -1, 64))
	return &quotaGroup{DisplayName: name, Buckets: []quotaBucket{{Window: "credits", RemainingFraction: fraction, ResetTime: reset, Description: description}}}, nil
}
func number(value any) (float64, bool) {
	var n float64
	var err error
	switch v := value.(type) {
	case json.Number:
		n, err = v.Float64()
	case string:
		n, err = strconv.ParseFloat(strings.TrimSpace(v), 64)
	case float64:
		n = v
	default:
		return 0, false
	}
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func firstNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if n, ok := number(m[key]); ok {
			return n, true
		}
	}
	return 0, false
}
func firstQuotaTime(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if stamp := quotaTime(m[key]); stamp != "" {
			return stamp
		}
	}
	return ""
}
func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
func cleanLabel(s string) string { return truncate(strings.Join(strings.Fields(s), " "), 160) }
func quotaTime(value any) string {
	if n, ok := number(value); ok && n > 0 {
		if n > 1e12 {
			n /= 1000
		}
		if n > 253402300799 {
			return ""
		}
		return time.Unix(int64(n), 0).UTC().Format(time.RFC3339)
	}
	if v, ok := value.(string); ok {
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, v, time.FixedZone("CST", 8*3600)); err == nil {
				return t.UTC().Format(time.RFC3339)
			}
		}
	}
	return ""
}
