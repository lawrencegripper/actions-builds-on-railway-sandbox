package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTailscaleOAuthAndEphemeralKey(t *testing.T) {
	var oauthCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			oauthCalls.Add(1)
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
				t.Error("missing OAuth basic auth")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "oauth-access", "expires_in": 3600})
		case "/tailnet/example.ts.net/keys":
			if r.Header.Get("Authorization") != "Bearer oauth-access" {
				t.Errorf("key authorization = %q", r.Header.Get("Authorization"))
			}
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if int(payload["expirySeconds"].(float64)) != 600 {
				t.Errorf("expirySeconds = %#v", payload["expirySeconds"])
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "tskey-auth-short"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{
		TailscaleTailnet:       "example.ts.net",
		TailscaleOAuthClientID: "client",
		TailscaleOAuthSecret:   "secret",
		TailscaleTag:           "tag:runner",
		TailscaleKeyExpiry:     10 * time.Minute,
		TailscaleAPIBaseURL:    server.URL,
		TailscaleOAuthURL:      server.URL + "/oauth/token",
	}
	keys := NewTailscaleKeys(cfg, server.Client())
	for i := 0; i < 2; i++ {
		key, err := keys.AuthKey(context.Background())
		if err != nil || key != "tskey-auth-short" {
			t.Fatalf("key = %q, %v", key, err)
		}
	}
	if oauthCalls.Load() != 1 {
		t.Fatalf("OAuth calls = %d, want cached single call", oauthCalls.Load())
	}
}
