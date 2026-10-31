// Package supabase implements rendezvous.Backend against a hosted Supabase
// project: GoTrue for auth, PostgREST for rows and RPCs, and the Phoenix
// realtime socket for change feeds and presence. Nothing here ever sees a
// plaintext message; the client encrypts before calling in.
package supabase

import (
	"bytes"
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

	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Config selects the project. AnonKey is the public anon JWT; AdminToken is
// the device token minted by admin_bootstrap_device / admin_approve_device
// and may be set later with SetAdminToken.
type Config struct {
	URL        string
	AnonKey    string
	AdminToken string
	HTTP       *http.Client
}

// Client is one user's connection to the project. It is safe for
// concurrent use.
type Client struct {
	base       string
	anon       string
	http       *http.Client
	now        func() time.Time
	refreshGap time.Duration

	mu         sync.RWMutex
	session    *rendezvous.Session
	adminToken string
	refreshMu  sync.Mutex

	rt *realtime
}

var _ rendezvous.Backend = (*Client)(nil)

// New builds a client; it performs no network calls.
func New(cfg Config) *Client {
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	c := &Client{
		base:       strings.TrimRight(cfg.URL, "/"),
		anon:       cfg.AnonKey,
		http:       h,
		now:        time.Now,
		refreshGap: 60 * time.Second,
		adminToken: cfg.AdminToken,
	}
	c.rt = newRealtime(c)
	return c
}

// SetAdminToken installs (or clears) the admin device token sent as
// x-pairdesk-admin-token on every request.
func (c *Client) SetAdminToken(tok string) {
	c.mu.Lock()
	c.adminToken = tok
	c.mu.Unlock()
}

// ---------------------------------------------------------------- errors

// apiError is the decoded body of a failed request; it never carries
// tokens, only the server's code and message.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("supabase: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("supabase: %d: %s", e.Status, e.Message)
}

// mapError turns a transport error into the rendezvous sentinel it stands
// for, keeping the original as the wrapped cause.
func mapError(e *apiError) error {
	var sentinel error
	switch e.Code {
	case "42501":
		sentinel = rendezvous.ErrForbidden
	case "23505":
		sentinel = rendezvous.ErrConflict
	case "P0001":
		sentinel = rendezvous.ErrRegClosed
	case "P0002":
		sentinel = rendezvous.ErrBadInvite
	case "P0003":
		sentinel = rendezvous.ErrMaintenance
	case "P0004":
		sentinel = rendezvous.ErrRateLimited
	case "PGRST116":
		sentinel = rendezvous.ErrNotFound
	}
	if sentinel == nil {
		switch e.Status {
		case http.StatusUnauthorized:
			sentinel = rendezvous.ErrUnauthorized
		case http.StatusForbidden:
			sentinel = rendezvous.ErrForbidden
		case http.StatusNotFound:
			sentinel = rendezvous.ErrNotFound
		case http.StatusConflict:
			sentinel = rendezvous.ErrConflict
		case http.StatusTooManyRequests:
			sentinel = rendezvous.ErrRateLimited
		case http.StatusServiceUnavailable:
			sentinel = rendezvous.ErrMaintenance
		}
	}
	if sentinel == nil {
		return e
	}
	return fmt.Errorf("%w: %w", sentinel, e)
}

func parseError(status int, body []byte) error {
	e := &apiError{Status: status}
	var raw struct {
		Code             json.RawMessage `json:"code"`
		Message          string          `json:"message"`
		Msg              string          `json:"msg"`
		Error            string          `json:"error"`
		ErrorCode        string          `json:"error_code"`
		ErrorDescription string          `json:"error_description"`
		Details          string          `json:"details"`
	}
	if json.Unmarshal(body, &raw) == nil {
		var s string
		if json.Unmarshal(raw.Code, &s) == nil {
			e.Code = s
		}
		if e.Code == "" {
			e.Code = raw.ErrorCode
		}
		for _, m := range []string{raw.Message, raw.Msg, raw.ErrorDescription, raw.Error, raw.Details} {
			if m != "" {
				e.Message = m
				break
			}
		}
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return mapError(e)
}

// ------------------------------------------------------------------- http

// do is the single HTTP path. path is absolute within the project
// ("/rest/v1/users"); query may be nil; body (if non-nil) is JSON encoded;
// out (if non-nil) receives the decoded JSON response. Writes to PostgREST
// tables ask for the representation back.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if !strings.HasPrefix(path, "/auth/") {
		if err := c.ensureFresh(ctx); err != nil {
			return err
		}
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	c.setHeaders(req.Header)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if strings.HasPrefix(path, "/rest/v1/") && !strings.HasPrefix(path, "/rest/v1/rpc/") && method != http.MethodGet {
		req.Header.Set("Prefer", "return=representation")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseError(resp.StatusCode, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("supabase: decode %s %s: %w", method, path, err)
	}
	return nil
}

// setHeaders adds the project key, the user's bearer token and the admin
// device token when present.
func (c *Client) setHeaders(h http.Header) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h.Set("apikey", c.anon)
	if c.session != nil && c.session.AccessToken != "" {
		h.Set("Authorization", "Bearer "+c.session.AccessToken)
	}
	if c.adminToken != "" {
		h.Set("x-pairdesk-admin-token", c.adminToken)
	}
}

// one fetches a single row: PostgREST always answers with an array, so
// an empty array is ErrNotFound.
func (c *Client) one(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var raw []json.RawMessage
	if err := c.do(ctx, method, path, query, body, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return rendezvous.ErrNotFound
	}
	return json.Unmarshal(raw[0], out)
}

// rpc calls a PostgREST function.
func (c *Client) rpc(ctx context.Context, name string, args, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	return c.do(ctx, http.MethodPost, "/rest/v1/rpc/"+name, nil, args, out)
}

// ------------------------------------------------------------------- auth

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

func (c *Client) adopt(t tokenResponse) rendezvous.Session {
	exp := time.Unix(t.ExpiresAt, 0)
	if t.ExpiresAt == 0 {
		exp = c.now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	s := rendezvous.Session{
		UserID:       t.User.ID,
		Email:        t.User.Email,
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    exp,
	}
	c.mu.Lock()
	c.session = &s
	c.mu.Unlock()
	c.rt.tokenChanged(s.AccessToken)
	return s
}

func (c *Client) RequestMagicLink(ctx context.Context, email string) error {
	return c.do(ctx, http.MethodPost, "/auth/v1/otp", nil,
		map[string]any{"email": email, "create_user": true}, nil)
}

func (c *Client) VerifyOTP(ctx context.Context, email, code string) (rendezvous.Session, error) {
	var t tokenResponse
	err := c.do(ctx, http.MethodPost, "/auth/v1/verify", nil,
		map[string]any{"type": "email", "email": email, "token": code}, &t)
	if err != nil {
		return rendezvous.Session{}, err
	}
	if t.AccessToken == "" {
		return rendezvous.Session{}, errors.New("supabase: verify returned no session")
	}
	return c.adopt(t), nil
}

func (c *Client) Restore(ctx context.Context, refreshToken string) (rendezvous.Session, error) {
	var t tokenResponse
	err := c.do(ctx, http.MethodPost, "/auth/v1/token", url.Values{"grant_type": {"refresh_token"}},
		map[string]any{"refresh_token": refreshToken}, &t)
	if err != nil {
		return rendezvous.Session{}, err
	}
	if t.AccessToken == "" {
		return rendezvous.Session{}, rendezvous.ErrUnauthorized
	}
	return c.adopt(t), nil
}

// ensureFresh refreshes the access token when it is about to expire.
func (c *Client) ensureFresh(ctx context.Context) error {
	c.mu.RLock()
	s := c.session
	var stale bool
	var rt string
	if s != nil {
		stale = !s.ExpiresAt.IsZero() && c.now().Add(c.refreshGap).After(s.ExpiresAt)
		rt = s.RefreshToken
	}
	c.mu.RUnlock()
	if !stale || rt == "" {
		return nil
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.RLock()
	s = c.session
	stillStale := s != nil && s.RefreshToken == rt
	c.mu.RUnlock()
	if !stillStale {
		return nil
	}
	_, err := c.Restore(ctx, rt)
	return err
}

func (c *Client) Session() *rendezvous.Session {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.session == nil {
		return nil
	}
	s := *c.session
	return &s
}

func (c *Client) SignOut(ctx context.Context) error {
	c.mu.RLock()
	has := c.session != nil
	c.mu.RUnlock()
	var err error
	if has {
		err = c.do(ctx, http.MethodPost, "/auth/v1/logout", nil, nil, nil)
		if errors.Is(err, rendezvous.ErrUnauthorized) || errors.Is(err, rendezvous.ErrNotFound) {
			err = nil
		}
	}
	c.mu.Lock()
	c.session = nil
	c.mu.Unlock()
	c.rt.tokenChanged("")
	return err
}

// uid returns the signed-in user's id or ErrUnauthorized.
func (c *Client) uid() (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.session == nil || c.session.UserID == "" {
		return "", rendezvous.ErrUnauthorized
	}
	return c.session.UserID, nil
}

func (c *Client) Close() error {
	return c.rt.close()
}
