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
		fmt.Fprintf(w, `{"event_id":"evt_%d","status":"accepted","flags":[],"errors":[]}`, n)
	})
	mux.HandleFunc("GET /api/v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != Key {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"status":"accepted"}`)
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
