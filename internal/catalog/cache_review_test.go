package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestTokenRotationDoesNotGrowCache(t *testing.T) {
	manager := NewManager(func(context.Context, Credentials) ([]byte, error) { return []byte(fixture), nil }, Options{})
	for i := 0; i < 100; i++ {
		if _, err := manager.Get(context.Background(), Credentials{UID: "same-user", AccessToken: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(manager.entries) != 1 {
		t.Fatalf("cache grew with tokens: %d", len(manager.entries))
	}
}
func TestCacheExpiryEvictsAbandonedScopesAndBoundsAccounts(t *testing.T) {
	now := time.Now()
	manager := NewManager(func(context.Context, Credentials) ([]byte, error) { return []byte(fixture), nil }, Options{MaxStale: time.Minute})
	manager.now = func() time.Time { return now }
	for i := 0; i < 260; i++ {
		_, err := manager.View(context.Background(), Credentials{UID: fmt.Sprint(i), AccessToken: "token"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(manager.entries) > 256 {
		t.Fatal("unbounded accounts")
	}
	now = now.Add(2 * time.Minute)
	_, _ = manager.View(context.Background(), Credentials{UID: "new", AccessToken: "t"})
	if len(manager.entries) != 1 {
		t.Fatal("abandoned accounts retained")
	}
}
func TestUnrelatedReconfigurePreservesTTLAndDeepCopy(t *testing.T) {
	calls := 0
	manager := NewManager(func(context.Context, Credentials) ([]byte, error) { calls++; return []byte(fixture), nil }, Options{})
	auth := Credentials{UID: "one", AccessToken: "one"}
	snapshot, _ := manager.Get(context.Background(), auth)
	snapshot.Models[0].Reasoning["effort"] = "corrupt"
	manager.Reconfigure(Options{})
	next, err := manager.Get(context.Background(), auth)
	if err != nil || calls != 1 || next.Models[0].Reasoning["effort"] == "corrupt" {
		t.Fatal("TTL or defensive copy lost")
	}
	manager.Invalidate()
	_, _ = manager.View(context.Background(), auth)
	if calls != 2 {
		t.Fatal("explicit invalidate ignored")
	}
	if _, ok := next.Find("not-a-model"); ok {
		t.Fatal("invalid index")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.View(ctx, auth); err == nil {
		t.Fatal("canceled lookup succeeded")
	}
}
func BenchmarkCatalogHotPath(b *testing.B) {
	manager := NewManager(func(context.Context, Credentials) ([]byte, error) { return []byte(fixture), nil }, Options{})
	auth := Credentials{UID: "same", AccessToken: "same"}
	snapshot, _ := manager.Get(context.Background(), auth)
	b.Run("legacy_json_clone", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			raw, _ := json.Marshal(snapshot)
			var clone Snapshot
			_ = json.Unmarshal(raw, &clone)
			_, _ = clone.Find("hy4-preview-f")
		}
	})
	b.Run("immutable_view_index", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			view, _ := manager.View(context.Background(), auth)
			_, _ = view.Find("hy4-preview-f")
		}
	})
}
