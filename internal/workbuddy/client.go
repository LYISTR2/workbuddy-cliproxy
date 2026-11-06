// Package workbuddy defines the shared WorkBuddy China client profile.
package workbuddy

import "net/http"

const (
	BaseURL    = "https://www.workbuddy.cn"
	AppVersion = "5.8.3"
	CLIVersion = "2.161.0"
	// WorkBuddy's gateway parses numeric versions from this native header shape.
	// See https://github.com/dingminhua/dsh-connect-workbuddy/blob/main/docs/PROMO_BADGE.md.
	UserAgent = "WorkBuddy/" + AppVersion + " WorkBuddy/" + AppVersion + " CLI/" + CLIVersion
)

// SetClientHeaders is shared by login, discovery and inference so their client
// identity cannot drift. Account-specific headers are added by the caller.
func SetClientHeaders(req *http.Request) {
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Product-Code", "workbuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Version", AppVersion)
	req.Header.Set("X-Domain", "www.workbuddy.cn")
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("Referer", BaseURL+"/")
}
