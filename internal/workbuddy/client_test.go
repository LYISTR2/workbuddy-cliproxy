package workbuddy

import (
	"net/http"
	"strings"
	"testing"
)

func TestOfficialWorkbuddyClientProfile(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, BaseURL, nil)
	SetClientHeaders(req)
	if !strings.HasPrefix(req.UserAgent(), "WorkBuddy/"+AppVersion) || req.Header.Get("X-IDE-Version") != AppVersion || req.Header.Get("X-Product-Code") != "workbuddy" || req.Header.Get("Origin") != BaseURL || req.Header.Get("Authorization") != "" || req.Header.Get("X-Refresh-Token") != "" {
		t.Fatal("client profile or secret isolation regressed")
	}
}
