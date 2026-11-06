// Package testfake is a stand-in for the Integration API, used by the sender's own tests. It authenticates
// with one key, acknowledges every JSON body as accepted, rejects bodies that are not JSON objects, and
// serves the events it created.
package testfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Key is the only key the fake accepts.
const Key = "pxk_test_" + "FAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAK"

// Server is a running fake.
type Server struct {
	*httptest.Server
	mu    sync.Mutex
	seen  []string // API keys received by the webhook
	count int

	// asyncPolls > 0 makes the fake behave like an async tenant: the webhook answers 202 queued, and each event
	// reads back as "pending" that many times before it becomes "accepted".
	asyncPolls int
	polls      map[string]int
}

// Async turns the fake into an async tenant whose events need n lookups before a worker has "processed" them.
func (s *Server) Async(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asyncPolls = n
	s.polls = map[string]int{}
}

// New starts a fake API and stops it when the test ends.
func New(t *testing.T) *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/webhook/tive", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Get("X-API-Key"))
		s.count++
		n := s.count
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("X-API-Key") != Key {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`)
			return
		}
		var m map[string]any
		if json.NewDecoder(r.Body).Decode(&m) != nil {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"event_id":"evt_%d","status":"rejected","flags":[],"errors":[{"code":"INVALID_JSON"}]}`, n)
			return
		}
		s.mu.Lock()
		async := s.asyncPolls > 0
		s.mu.Unlock()
		if async {
			w.WriteHeader(202)
			fmt.Fprintf(w, `{"event_id":"evt_%d","status":"queued","flags":[],"errors":[]}`, n)
			return
		}
		fmt.Fprintf(w, `{"event_id":"evt_%d","status":"accepted","flags":[],"errors":[]}`, n)
	})
	mux.HandleFunc("GET /api/v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != Key {
			w.WriteHeader(401)
			return
		}
		s.mu.Lock()
		pending := s.asyncPolls > 0 && s.polls[r.PathValue("id")] < s.asyncPolls
		if s.asyncPolls > 0 {
			s.polls[r.PathValue("id")]++
		}
		s.mu.Unlock()
		if pending {
			fmt.Fprint(w, `{"status":"pending","flags":[]}`)
			return
		}
		fmt.Fprint(w, `{"status":"accepted","flags":[]}`)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// KeysSeen returns the X-API-Key value of every webhook request so far.
func (s *Server) KeysSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}
