// Package api is a small HTTP client for the PAXAFE Integration API.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxBody = 1 << 20

// Client talks to one API instance.
type Client struct {
	Base string
	HTTP *http.Client
}

// New returns a client for base (for example https://paxafe-api.onrender.com).
func New(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Response is what came back from a webhook call.
type Response struct {
	HTTP       int
	Body       []byte
	Latency    time.Duration // of the final attempt
	Err        error         // network error; HTTP is 0
	RetryAfter time.Duration // from a 429 response
	Retries    int           // attempts repeated after a 429
}

// Ack is the part of the webhook response the sender checks.
type Ack struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
	Flags   []struct {
		Code string `json:"code"`
	} `json:"flags"`
	Errors []struct {
		Code string `json:"code"`
	} `json:"errors"`
}

// Parse decodes the webhook response body; ok is false when it is not the usual envelope.
func (r Response) Parse() (Ack, bool) {
	var a Ack
	if err := json.Unmarshal(r.Body, &a); err != nil {
		return Ack{}, false
	}
	return a, true
}

// FlagCodes lists the flag codes in the response.
func (a Ack) FlagCodes() []string {
	out := make([]string, 0, len(a.Flags))
	for _, f := range a.Flags {
		out = append(out, f.Code)
	}
	return out
}

// ErrorCodes lists the error codes in the response.
func (a Ack) ErrorCodes() []string {
	out := make([]string, 0, len(a.Errors))
	for _, e := range a.Errors {
		out = append(out, e.Code)
	}
	return out
}

// Post sends one payload to the webhook. key may be empty (no header).
func (c *Client) Post(ctx context.Context, key, requestID string, body []byte) Response {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/webhook/tive", bytes.NewReader(body))
	if err != nil {
		return Response{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", requestID)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{Latency: time.Since(start), Err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return Response{HTTP: resp.StatusCode, Body: b, Latency: time.Since(start), RetryAfter: retryAfter(resp)}
}

// maxWait caps how long the sender will wait on one Retry-After.
const maxWait = 5 * time.Second

func retryAfter(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
		return min(time.Duration(n)*time.Second, maxWait)
	}
	return time.Second
}

// PostRetry is Post that, like a well-behaved sender, waits out a 429 (honouring Retry-After) and tries again,
// up to attempts times in total. The final response is returned with the number of retries it took.
func (c *Client) PostRetry(ctx context.Context, key, requestID string, body []byte, attempts int) Response {
	var resp Response
	for try := 0; ; try++ {
		resp = c.Post(ctx, key, requestID, body)
		resp.Retries = try
		if resp.HTTP != http.StatusTooManyRequests || try+1 >= attempts {
			return resp
		}
		select {
		case <-ctx.Done():
			return resp
		case <-time.After(resp.RetryAfter):
		}
	}
}

// EventStatus returns the stored status of an event ("pending" if it has no outcome yet).
// found is false for 404 (never saved, or not visible to this key).
func (c *Client) EventStatus(ctx context.Context, key, eventID string) (status string, found bool, err error) {
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/v1/events/"+eventID, nil)
		if err != nil {
			return "", false, err
		}
		req.Header.Set("X-API-Key", key)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return "", false, err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && try < 10 { // the API is rate limiting our own checks
			select {
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-time.After(retryAfter(resp)):
			}
			continue
		}
		return decodeEvent(resp.StatusCode, b)
	}
}

func decodeEvent(code int, b []byte) (status string, found bool, err error) {
	switch code {
	case http.StatusOK:
		var ev struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(b, &ev); err != nil {
			return "", false, fmt.Errorf("decode event: %w", err)
		}
		return ev.Status, true, nil
	case http.StatusNotFound:
		return "", false, nil
	}
	return "", false, fmt.Errorf("GET event: HTTP %d", code)
}

// Wake polls /healthz until it answers 200 or ctx ends. Free-tier services sleep when idle.
func (c *Client) Wake(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/healthz", nil)
		if resp, err := c.HTTP.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return time.Since(start), nil
			}
		}
		select {
		case <-ctx.Done():
			return time.Since(start), fmt.Errorf("API did not wake up: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
