// Package client simülatör, doğrulama ve yük testi araçlarının kullandığı küçük API istemcisi.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

func New(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

type Response struct {
	Status int
	Body   []byte
	Header http.Header
	Took   time.Duration
}

func (r Response) JSON(v any) error { return json.Unmarshal(r.Body, v) }

func (r Response) ErrorCode() string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

func (c *Client) Do(ctx context.Context, method, path string, body any, headers ...string) (Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Response{}, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return Response{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Body: b, Header: resp.Header, Took: time.Since(start)}, err
}

func (c *Client) Get(ctx context.Context, path string) (Response, error) {
	return c.Do(ctx, "GET", path, nil)
}

func (c *Client) Post(ctx context.Context, path string, body any, headers ...string) (Response, error) {
	return c.Do(ctx, "POST", path, body, headers...)
}

// Login yeni bir istemci kopyası döner (token'lı).
func (c *Client) Login(ctx context.Context, email, password string) (*Client, error) {
	r, err := c.Post(ctx, "/api/v1/auth/login", map[string]string{"email": email, "password": password})
	if err != nil {
		return nil, err
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("giriş %s: HTTP %d %s", email, r.Status, r.Body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := r.JSON(&out); err != nil {
		return nil, err
	}
	cp := *c
	cp.Token = out.AccessToken
	return &cp, nil
}

// WaitHealthy API /health 200 dönene kadar bekler.
func (c *Client) WaitHealthy(ctx context.Context, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		r, err := c.Get(ctx, "/health")
		if err == nil && r.Status == 200 {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("health %d", r.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
