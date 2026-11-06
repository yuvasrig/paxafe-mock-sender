package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostSendsKeyAndRequestID(t *testing.T) {
	var gotKey, gotID, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotID, gotType = r.Header.Get("X-API-Key"), r.Header.Get("X-Request-ID"), r.Header.Get("Content-Type")
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		fmt.Fprint(w, `{"event_id":"evt_1","status":"flagged","flags":[{"code":"A"},{"code":"B"}],"errors":[{"code":"E"}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL + "/") // a trailing slash must not double up
	resp := c.Post(context.Background(), "k1", "rid-1", []byte(`{"x":1}`))
	if resp.Err != nil || resp.HTTP != 200 || resp.Latency <= 0 {
		t.Fatalf("%+v", resp)
	}
	if gotKey != "k1" || gotID != "rid-1" || gotType != "application/json" || gotBody != `{"x":1}` {
		t.Fatalf("headers/body: %q %q %q %q", gotKey, gotID, gotType, gotBody)
	}
	ack, ok := resp.Parse()
	if !ok || ack.EventID != "evt_1" || ack.Status != "flagged" || fmt.Sprint(ack.FlagCodes()) != "[A B]" || fmt.Sprint(ack.ErrorCodes()) != "[E]" {
		t.Fatalf("%+v %v", ack, ok)
	}

	c.Post(context.Background(), "", "rid-2", nil)
	if gotKey != "" {
		t.Fatal("an empty key sends no header")
	}
	if _, ok := (Response{Body: []byte("<html>")}).Parse(); ok {
		t.Fatal("non-JSON must not parse")
	}
}

func TestPostNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if r := New(url).Post(context.Background(), "k", "r", []byte(`{}`)); r.Err == nil || r.HTTP != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestEventStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/events/ok":
			fmt.Fprint(w, `{"status":"conflict"}`)
		case "/api/v1/events/bad":
			fmt.Fprint(w, `not json`)
		case "/api/v1/events/boom":
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	if s, found, err := c.EventStatus(context.Background(), "k", "ok"); err != nil || !found || s != "conflict" {
		t.Fatalf("%q %v %v", s, found, err)
	}
	if _, found, err := c.EventStatus(context.Background(), "k", "missing"); err != nil || found {
		t.Fatalf("404 is 'not found', not an error: %v %v", found, err)
	}
	for _, id := range []string{"bad", "boom"} {
		if _, _, err := c.EventStatus(context.Background(), "k", id); err == nil {
			t.Fatalf("%s should be an error", id)
		}
	}
}

func TestWake(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(503) // still waking
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	took, err := New(srv.URL).Wake(context.Background())
	if err != nil || calls.Load() != 2 || took < time.Second {
		t.Fatalf("took %v calls %d err %v", took, calls.Load(), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer down.Close()
	if _, err := New(down.URL).Wake(ctx); err == nil {
		t.Fatal("a service that never answers 200 must time out")
	}
}

// A sender that is rate limited waits and retries instead of giving up (or hammering the API).
func TestPostRetryWaitsOut429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"event_id":"e","status":"accepted"}`)
	}))
	defer srv.Close()
	start := time.Now()
	resp := New(srv.URL).PostRetry(context.Background(), "k", "r", []byte(`{}`), 5)
	if resp.HTTP != 200 || resp.Retries != 2 || calls.Load() != 3 || time.Since(start) < 2*time.Second {
		t.Fatalf("%+v after %v, %d calls", resp, time.Since(start), calls.Load())
	}

	// Giving up: the last 429 is returned, not swallowed.
	calls.Store(-100)
	always := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }))
	defer always.Close()
	if r := New(always.URL).PostRetry(context.Background(), "k", "r", nil, 2); r.HTTP != 429 || r.Retries != 1 {
		t.Fatalf("%+v", r)
	}
	// Plain Post never retries.
	if r := New(always.URL).Post(context.Background(), "k", "r", nil); r.HTTP != 429 || r.RetryAfter != time.Second {
		t.Fatalf("%+v", r)
	}
}

func TestEventStatusWaitsOut429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"status":"accepted"}`)
	}))
	defer srv.Close()
	s, found, err := New(srv.URL).EventStatus(context.Background(), "k", "e")
	if err != nil || !found || s != "accepted" || calls.Load() != 2 {
		t.Fatalf("%q %v %v calls=%d", s, found, err, calls.Load())
	}
}
