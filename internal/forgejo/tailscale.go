package forgejo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type TailscaleKeys struct {
	cfg  Config
	http *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func NewTailscaleKeys(cfg Config, httpClient *http.Client) *TailscaleKeys {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &TailscaleKeys{cfg: cfg, http: httpClient}
}

func (t *TailscaleKeys) access(ctx context.Context) (string, error) {
	if t.cfg.TailscaleAPIToken != "" {
		return t.cfg.TailscaleAPIToken, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.accessToken != "" && time.Now().Add(time.Minute).Before(t.expiresAt) {
		return t.accessToken, nil
	}
	form := strings.NewReader("grant_type=client_credentials")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.TailscaleOAuthURL, form)
	if err != nil {
		return "", err
	}
	credentials := base64.StdEncoding.EncodeToString([]byte(t.cfg.TailscaleOAuthClientID + ":" + t.cfg.TailscaleOAuthSecret))
	req.Header.Set("Authorization", "Basic "+credentials)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("Tailscale OAuth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("Tailscale OAuth %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode Tailscale OAuth response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("Tailscale OAuth response did not contain access_token")
	}
	if body.ExpiresIn == 0 {
		body.ExpiresIn = 3600
	}
	t.accessToken = body.AccessToken
	t.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return t.accessToken, nil
}

func (t *TailscaleKeys) AuthKey(ctx context.Context) (string, error) {
	token, err := t.access(ctx)
	if err != nil {
		return "", err
	}
	rawURL := strings.TrimRight(t.cfg.TailscaleAPIBaseURL, "/") + "/tailnet/" + url.PathEscape(t.cfg.TailscaleTailnet) + "/keys"
	payload := map[string]any{
		"capabilities": map[string]any{
			"devices": map[string]any{
				"create": map[string]any{
					"reusable":      false,
					"ephemeral":     true,
					"preauthorized": true,
					"tags":          []string{t.cfg.TailscaleTag},
				},
			},
		},
		"expirySeconds": int(t.cfg.TailscaleKeyExpiry.Seconds()),
		"description":   "Railway Forgejo ephemeral runner",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(string(encoded)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create Tailscale auth key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("create Tailscale auth key %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode Tailscale key response: %w", err)
	}
	if body.Key == "" {
		return "", fmt.Errorf("Tailscale key response did not contain key")
	}
	return body.Key, nil
}
