package catalog

import (
	"context"
	"testing"
	"time"
)

func TestChannelFiltersDisabledAndNonChatModels(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"models":[{"id":"new-chat"},{"id":"disabled","enabled":false},{"id":"image","tags":["text-to-image"]},{"id":"vectors","tags":["embedding"]},{"id":"completion-only"}],"agents":[{"name":"cli","models":["new-chat","disabled","image","vectors","completion-only"]}]}}`)
	snapshot, err := Parse(raw, "cli", time.Now())
	if err != nil || len(snapshot.Models) != 1 || snapshot.Models[0].ID != "new-chat" {
		t.Fatalf("unexpected models: %+v, error: %v", snapshot.Models, err)
	}
}

func TestBusinessAuthFailureInvalidatesLastGoodCatalog(t *testing.T) {
	for _, code := range []string{"401", "403"} {
		t.Run(code, func(t *testing.T) {
			response := []byte(fixture)
			manager := NewManager(func(context.Context, Credentials) ([]byte, error) { return response, nil }, Options{})
			credentials := Credentials{UID: "test", AccessToken: "test"}
			if _, err := manager.Get(context.Background(), credentials); err != nil {
				t.Fatal(err)
			}
			response = []byte(`{"code":` + code + `,"data":{}}`)
			manager.Invalidate()
			if _, err := manager.Get(context.Background(), credentials); err == nil {
				t.Fatal("business authentication error reused a stale catalog")
			}
		})
	}
}
