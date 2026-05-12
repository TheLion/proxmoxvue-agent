// Package supabase wraps the two Supabase interactions the agent needs:
// access-token refresh (with rotated-refresh-token persistence) and
// inserting snapshot rows via PostgREST. Service Role keys never live
// here — the agent authenticates as its own per-host user.
package supabase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// refreshSkew refreshes a few seconds before actual expiry so an
// in-flight REST call never starts with a token that expires mid-flight.
const refreshSkew = 30 * time.Second

// PersistRefreshFunc writes a rotated refresh token back to config.yml.
// Called after every successful refresh so a crash between refreshes
// doesn't leave the agent with an invalidated token.
type PersistRefreshFunc func(newRefreshToken string) error

type Client struct {
	baseURL        string // e.g. "https://api.proxmoxvue.app", no trailing slash
	publishableKey string
	httpClient     *http.Client
	persist        PersistRefreshFunc
	authBase       string
	restBase       string
	realtimeURL    string // resolved once in New(); read by subscribeTable

	mu           sync.Mutex
	accessToken  string
	expiresAt    time.Time
	refreshToken string

	// refreshMu serializes freshAccessToken so the two Realtime
	// refresh-goroutines (one per channel, ~80ms apart) don't both
	// POST to /token/refresh with the same refresh_token — Supabase
	// rotates the refresh_token per call, so the second caller would
	// hit "refresh_token_already_used" → ErrRefreshRevoked → agent dies.
	refreshMu sync.Mutex

	// activeSubs registry: topic → active Realtime subscription. Used
	// by the central refresh-loop to broadcast access_token-events to
	// all connected channels in one go.
	activeSubsMu sync.RWMutex
	activeSubs   map[string]*activeSub
}

// New builds a Supabase client from a fully-qualified base URL (with
// scheme) and the project's publishable key. realtimeOverride is used
// verbatim for WebSocket connections when non-empty; otherwise it's
// derived from baseURL's host as wss://<host>/realtime/v1/websocket.
//
// Resolving realtimeURL once in New() avoids the race where
// subscribeTable previously wrote to it lazily from two goroutines
// (-race flagged it even though the value was identical).
func New(baseURL, publishableKey, realtimeOverride, initialRefreshToken string, persist PersistRefreshFunc) (*Client, error) {
	base := strings.TrimRight(baseURL, "/")
	resolvedRT := realtimeOverride
	if resolvedRT == "" {
		u, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("parse baseURL %q: %w", base, err)
		}
		resolvedRT = fmt.Sprintf("wss://%s/realtime/v1/websocket", u.Host)
	}
	return &Client{
		baseURL:        base,
		publishableKey: publishableKey,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		persist:        persist,
		authBase:       base + "/auth/v1",
		restBase:       base + "/rest/v1",
		realtimeURL:    resolvedRT,
		refreshToken:   initialRefreshToken,
		activeSubs:     map[string]*activeSub{},
	}, nil
}

// activeSub represents an active Realtime subscription that the central
// refresh-loop pushes access_token-events to. Lifetime: registered
// after a successful phx_join, unregistered in subscribeOnce's defer.
type activeSub struct {
	topic   string
	conn    *websocket.Conn
	nextRef func() string
	ctx     context.Context
}

// registerSubscription adds a sub to the registry, overwriting any
// existing entry with the same topic (handles reconnect cleanly: the
// new conn replaces the old).
func (c *Client) registerSubscription(sub *activeSub) {
	c.activeSubsMu.Lock()
	c.activeSubs[sub.topic] = sub
	c.activeSubsMu.Unlock()
}

// unregisterSubscription removes a sub from the registry by topic.
// No-op if not present (e.g. already replaced by reconnect).
func (c *Client) unregisterSubscription(topic string) {
	c.activeSubsMu.Lock()
	delete(c.activeSubs, topic)
	c.activeSubsMu.Unlock()
}

// ErrRefreshRevoked means the refresh token was rejected by Supabase,
// typically because token rotation detected reuse of an old token.
// The agent must exit and be re-enrolled.
var ErrRefreshRevoked = errors.New("refresh token revoked")

// accessToken returns a valid access token, refreshing if needed.
func (c *Client) access(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.accessToken != "" && time.Until(c.expiresAt) > refreshSkew {
		token := c.accessToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()
	return c.refresh(ctx)
}

// freshAccessToken returns a token guaranteed to be valid for at least
// ~30 minutes longer, suitable for the in-channel Realtime refresh push.
// access() returns the cached token whenever it's still valid beyond
// refreshSkew (30s) — too short for a 50-min push cadence, the
// "fresh" token we push is then near-expired and the server doesn't
// extend the WS-life. refreshMu serializes the two concurrent callers
// (one per channel, ~80ms apart) so we don't double-rotate the
// refresh_token and crash with ErrRefreshRevoked.
func (c *Client) freshAccessToken(ctx context.Context) (string, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.Lock()
	cached := c.accessToken
	timeLeft := time.Until(c.expiresAt)
	c.mu.Unlock()
	// 30min threshold: after a refresh, the new token has ~60min
	// validity. The second caller arriving ~80ms later sees ~60m left
	// and re-uses without an extra HTTP refresh. If the cached token
	// has < 30m left we force a refresh.
	if cached != "" && timeLeft > 30*time.Minute {
		return cached, nil
	}
	return c.refresh(ctx)
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	ErrorCode    string `json:"error_code"`
	ErrorMsg     string `json:"error_description"`
}

func (c *Client) refresh(ctx context.Context) (string, error) {
	c.mu.Lock()
	current := c.refreshToken
	c.mu.Unlock()

	body, err := json.Marshal(map[string]string{"refresh_token": current})
	if err != nil {
		return "", fmt.Errorf("marshal refresh body: %w", err)
	}

	url := c.authBase + "/token?grant_type=refresh_token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", c.publishableKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST token refresh: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read refresh response: %w", err)
	}

	var parsed refreshResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("parse refresh response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		if parsed.ErrorCode == "refresh_token_not_found" || parsed.ErrorCode == "refresh_token_already_used" {
			return "", ErrRefreshRevoked
		}
		msg := parsed.ErrorMsg
		if msg == "" {
			msg = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return "", fmt.Errorf("refresh failed: %s", msg)
	}

	if parsed.AccessToken == "" || parsed.RefreshToken == "" {
		return "", fmt.Errorf("refresh response missing tokens")
	}

	if err := c.persist(parsed.RefreshToken); err != nil {
		return "", fmt.Errorf("persist rotated refresh token: %w", err)
	}

	c.mu.Lock()
	c.accessToken = parsed.AccessToken
	c.refreshToken = parsed.RefreshToken
	c.expiresAt = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	c.mu.Unlock()

	return parsed.AccessToken, nil
}

// Ping verifies that the current refresh token can be used to obtain an
// access token. Call once at startup to fail fast on bad credentials.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.refresh(ctx)
	return err
}

// String redacts tokens so accidental fmt.Printf("%+v", client) in logs
// doesn't leak credentials. Mirrors the guard in internal/config.
func (c *Client) String() string {
	return fmt.Sprintf("{baseURL:%s accessToken:[REDACTED] refreshToken:[REDACTED]}", c.baseURL)
}
