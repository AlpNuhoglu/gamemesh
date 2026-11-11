package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// apiClient talks to the HTTP API through the gateway. Every call waits on a
// shared limiter (the gateway allows 50 req/s per IP, burst 100) and retries
// 429/502/503/504 and transport errors with capped exponential backoff,
// honouring Retry-After.
type apiClient struct {
	base string
	hc   *http.Client
	lim  *rate.Limiter
}

func newAPIClient(base string, rps float64) *apiClient {
	return &apiClient{
		base: strings.TrimRight(base, "/"),
		hc:   &http.Client{Timeout: 30 * time.Second},
		lim:  rate.NewLimiter(rate.Limit(rps), 1),
	}
}

const maxAttempts = 8

// httpError is a final, non-retried HTTP status.
type httpError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// do sends one JSON request and decodes a 2xx JSON response into out (if
// non-nil). Non-2xx statuses that are not retried come back as *httpError.
func (a *apiClient) do(ctx context.Context, method, path, token string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	backoff := 250 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := a.lim.Wait(ctx); err != nil {
			return err
		}
		retryAfter, err := a.once(ctx, method, path, token, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err
		st := statusOf(err)
		retryable := st == 0 || st == http.StatusTooManyRequests || st == http.StatusBadGateway ||
			st == http.StatusServiceUnavailable || st == http.StatusGatewayTimeout
		if !retryable || ctx.Err() != nil || attempt == maxAttempts {
			break
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1)) // #nosec G404 -- jitter, not security
		if retryAfter > wait {
			wait = retryAfter
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(2*backoff, 5*time.Second)
	}
	return lastErr
}

func (a *apiClient) once(ctx context.Context, method, path, token string, payload []byte, out any) (time.Duration, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var ra time.Duration
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ra = time.Duration(s) * time.Second
		}
		return ra, &httpError{Method: method, Path: path, Status: resp.StatusCode, Body: truncate(string(raw), 200)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return 0, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// register creates the account. A 409 is accepted: usernames carry the run
// ID, so a conflict can only be this run's own earlier attempt whose response
// was lost (the retry of a register that did reach the server).
func (a *apiClient) register(ctx context.Context, p *player, password string) error {
	err := a.do(ctx, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": p.username, "email": p.email, "password": password,
	}, nil)
	if statusOf(err) == http.StatusConflict {
		return nil
	}
	return err
}

func (a *apiClient) login(ctx context.Context, p *player, password string) error {
	var out struct {
		Token  string `json:"token"`
		Player struct {
			ID string `json:"id"`
		} `json:"player"`
	}
	if err := a.do(ctx, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"identifier": p.username, "password": password,
	}, &out); err != nil {
		return err
	}
	if out.Token == "" || out.Player.ID == "" {
		return errors.New("login response has no token or player id")
	}
	p.token, p.id = out.Token, out.Player.ID
	p.jti = jwtID(out.Token)
	return nil
}

func (a *apiClient) joinQueue(ctx context.Context, p *player, rank int) error {
	return a.do(ctx, http.MethodPost, "/api/v1/queue", p.token, map[string]int{"rank": rank}, nil)
}

// leaveQueue withdraws the player's ticket. removed is false (and err nil)
// when the player was not queued, e.g. because the matcher took it first.
func (a *apiClient) leaveQueue(ctx context.Context, p *player) (removed bool, err error) {
	err = a.do(ctx, http.MethodDelete, "/api/v1/queue", p.token, nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// logout revokes the session. A 401 means it is already gone (revoked or
// expired), which is the goal.
func (a *apiClient) logout(ctx context.Context, p *player) error {
	err := a.do(ctx, http.MethodPost, "/api/v1/auth/logout", p.token, nil, nil)
	if statusOf(err) == http.StatusUnauthorized {
		return nil
	}
	return err
}

// jwtID reads the jti claim without verifying the token (we only need it to
// find this run's SessionRevoked outbox rows during cleanup).
func jwtID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		JTI string `json:"jti"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	return c.JTI
}
