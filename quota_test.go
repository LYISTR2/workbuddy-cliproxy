//go:build cgo

package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQuotaNormalizationAndBoundaries(t *testing.T) {
	now := time.Unix(1700000000, 0)
	samples := []struct {
		name, raw  string
		enterprise bool
		count      int
		fraction   float64
		contains   string
	}{
		{"official", `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"Pro","Status":0,"CycleCapacitySizePrecise":"100.5","CycleCapacityRemainPrecise":"25.125","CycleResetTime":1800000000000}]}}}}`, false, 1, .25, "25.125 / 100.5"},
		{"double", `{"code":200,"data":{"data":{"resources":[{"name":"Bonus","total":"40","remaining":"0","expire_at":1800000000}]}}}`, false, 1, 0, "0 / 40"},
		{"enterprise", `{"code":0,"data":{"limit_num":"100","credit":"20","cycleResetTime":1800000000}}`, true, 1, .8, "80 / 100"},
		{"enterprise_unlimited", `{"code":0,"data":{"data":{"limitNum":-1,"usedNum":50}}}`, true, 1, 1, "不限额"},
		{"slice", `{"data":{"resources":[{"CapacityType":4,"SlicePeriodUsageDetails":[{"SlicePeriodCapacitySizePrecise":"10","SlicePeriodCapacityRemainPrecise":"3"}],"Status":3}]}}`, false, 1, .3, "3 / 10"},
		{"expired", `{"data":{"resources":[{"total":10,"remaining":3,"expireAt":1600000000}]}}`, false, 0, 0, ""},
		{"inactive", `{"data":{"resources":[{"Status":2,"total":10,"remaining":3}]}}`, false, 0, 0, ""},
		{"empty", `{"data":{"resources":[]}}`, false, 0, 0, ""},
		{"dedup", `{"data":{"resources":[{"ResourceId":"one","total":10,"remaining":3},{"ResourceId":"one","total":10,"remaining":3}]}}`, false, 1, .3, "3 / 10"},
	}
	for _, sample := range samples {
		t.Run(sample.name, func(t *testing.T) {
			q, err := normalizeQuota([]byte(sample.raw), sample.enterprise, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(q.Groups) != sample.count {
				t.Fatalf("groups=%+v", q)
			}
			if sample.count > 0 {
				bucket := q.Groups[0].Buckets[0]
				if math.Abs(bucket.RemainingFraction-sample.fraction) > 1e-9 || !strings.Contains(bucket.Description, sample.contains) {
					t.Fatalf("bucket=%+v", bucket)
				}
			}
		})
	}
	for _, sample := range []struct {
		raw        string
		enterprise bool
	}{{`{`, false}, {`{"code":403,"msg":"secret"}`, false}, {`{"data":{}}`, false}, {`{"data":{"resources":[{}]}}`, false}, {`{"data":{"resources":[{"total":10,"remaining":"NaN"}]}}`, false}, {`{"data":{"resources":[],"TotalCount":101}}`, false}, {`{"data":{"Response":{"Data":{"Accounts":[],"TotalCount":101}}}}`, false}, {`{"data":{"limitNum":100}}`, true}} {
		if _, err := normalizeQuota([]byte(sample.raw), sample.enterprise, now); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid quota accepted or leaked: %s %v", sample.raw, err)
		}
	}
	for _, value := range []any{json.Number("1800000000"), "2027-01-01 00:00:00", "2027-01-01", "2027-01-01T00:00:00Z"} {
		if quotaTime(value) == "" {
			t.Fatal("valid quota reset time lost")
		}
	}
	if quotaTime("invalid") != "" {
		t.Fatal("invented quota reset")
	}
}
func TestNativeQuotaFetchUsesAccountContext(t *testing.T) {
	for _, enterprise := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "enterprise"}[enterprise], func(t *testing.T) {
			sa := testAuth()
			sa.Auth.RefreshToken = "do-not-send"
			if enterprise {
				sa.Account.EnterpriseID = "team"
			}
			mockWorkbuddyHTTP(t, func(r *http.Request) (int, string) {
				if r.Header.Get("Authorization") != "Bearer TEST_TOKEN" || r.Header.Get("X-Refresh-Token") != "" || r.Header.Get("X-User-Id") != "TEST_UID" {
					t.Error("unsafe quota identity")
				}
				if enterprise {
					if !strings.HasSuffix(r.URL.Path, "get-enterprise-user-usage") || r.Header.Get("X-Tenant-Id") != "team" {
						t.Error("wrong enterprise query")
					}
					return 200, `{"data":{"limit_num":100,"used_num":20}}`
				}
				return 200, `{"data":{"resources":[{"name":"资源包","total":100,"remaining":40}]}}`
			})
			storage, _ := json.Marshal(sa)
			req, _ := json.Marshal(quotaRequest{Provider: providerName, StorageJSON: storage})
			raw, err := handleMethod("quota.fetch", req)
			var result quotaResponse
			decodeAuthResult(t, raw, err, &result)
			if len(result.Groups) != 1 {
				t.Fatal("native quota missing")
			}
		})
	}
}
func TestQuotaFetchErrorsAreSafe(t *testing.T) {
	storage, _ := json.Marshal(testAuth())
	req, _ := json.Marshal(quotaRequest{StorageJSON: storage})
	for _, status := range []int{401, 429} {
		mockWorkbuddyHTTP(t, func(*http.Request) (int, string) { return status, "secret" })
		if _, err := handleQuotaFetch(req); err == nil || errorStatus(err) != status || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	if _, err := handleQuotaFetch([]byte(`{"provider":"foreign"}`)); errorStatus(err) != 400 {
		t.Fatal(err)
	}
	if _, err := handleMethod("quota.reset", nil); errorStatus(err) != 501 {
		t.Fatal("unsupported quota reset succeeded")
	}
}
