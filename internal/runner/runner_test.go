package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
	"github.com/yuvasrig/paxafe-mock-sender/internal/scenario"
)

const goodKey = "pxk_test_" + "GOODGOODGOODGOODGOODGOODGOODGOODGOODGOODGOO"

// fake is a stand-in API: it authenticates, answers per fn, and remembers events.
type fake struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	events  map[string]string // event id -> stored status
	headers []http.Header
	posts   atomic.Int64
	// answer decides the webhook response for the n-th request (0-based): HTTP code, status, flags.
	answer func(n int, body []byte) (code int, status string, flags []string)
	// eventStatus overrides the stored status returned by GET (nil = what the webhook said). "" = 404.
	eventStatus func(id, said string) string
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, events: map[string]string{}}
	f.answer = func(int, []byte) (int, string, []string) { return 200, "accepted", nil }
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/webhook/tive", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		if r.Header.Get("X-API-Key") != goodKey {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"request_id":"x","errors":[{"code":"UNAUTHORIZED","message":"no"}]}`)
			return
		}
		n := int(f.posts.Add(1)) - 1
		var body json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		code, status, flags := f.answer(n, body)
		id := fmt.Sprintf("evt_%d", n)
		f.mu.Lock()
		f.events[id] = status
		f.mu.Unlock()
		fl := []map[string]string{}
		for _, c := range flags {
			fl = append(fl, map[string]string{"code": c})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": id, "request_id": r.Header.Get("X-Request-ID"), "status": status, "flags": fl, "errors": []any{}})
	})
	mux.HandleFunc("GET /api/v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != goodKey {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		said, ok := f.events[r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		if f.eventStatus != nil {
			said = f.eventStatus(r.PathValue("id"), said)
		}
		if said == "" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"status":%q}`, said)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) cfg() Config {
	return Config{Client: api.New(f.srv.URL), Key: goodKey, RunID: "run1", Params: scenario.Params{Account: 478, Points: 5, Seed: 7}}
}

func mustGet(t *testing.T, id string) scenario.Scenario {
	t.Helper()
	s, ok := scenario.Get(id)
	if !ok {
		t.Fatalf("no scenario %s", id)
	}
	return s
}

func TestAuthScenarioPasses(t *testing.T) {
	f := newFake(t)
	rep, err := Run(context.Background(), f.cfg(), mustGet(t, "auth"))
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Summary
	if s.Sent != 3 || s.Passed != 3 || s.Failed != 0 || s.Acknowledged != 0 || s.Missing != 0 || !s.OK() {
		t.Fatalf("summary %+v", s)
	}
	for _, r := range rep.Results {
		if r.HTTP != 401 {
			t.Errorf("%s: HTTP %d", r.Name, r.HTTP)
		}
	}
	// None of the three used the run's key.
	for _, h := range f.headers {
		if h.Get("X-API-Key") == goodKey {
			t.Fatal("auth steps must not send the real key")
		}
	}
	if f.headers[0].Get("X-API-Key") != "" || !strings.HasPrefix(f.headers[1].Get("X-API-Key"), "pxk_test_") || f.headers[2].Get("X-API-Key") != "this-is-not-an-api-key" {
		t.Fatalf("key modes: %q %q %q", f.headers[0].Get("X-API-Key"), f.headers[1].Get("X-API-Key"), f.headers[2].Get("X-API-Key"))
	}
}

// Every request carries an X-Request-ID the run can trace.
func TestRequestIDs(t *testing.T) {
	f := newFake(t)
	_, _ = Run(context.Background(), f.cfg(), mustGet(t, "route"))
	for i, h := range f.headers {
		if got, want := h.Get("X-Request-ID"), fmt.Sprintf("run1-%d", i); got != want {
			t.Fatalf("request %d id = %q, want %q", i, got, want)
		}
		if h.Get("Content-Type") != "application/json" {
			t.Fatal("content type")
		}
	}
}

// The checker must notice wrong answers, not just accept everything.
func TestWrongAnswersFail(t *testing.T) {
	f := newFake(t) // answers "accepted" with no flags to everything
	rep, _ := Run(context.Background(), f.cfg(), mustGet(t, "samples"))
	s := rep.Summary
	if s.Sent != 13 || s.Failed == 0 || s.OK() {
		t.Fatalf("summary %+v", s)
	}
	byIndex := map[int]Result{}
	for _, r := range rep.Results {
		byIndex[r.Index] = r
	}
	if r := byIndex[2]; !r.Pass { // sample 2 really is "accepted" with no flags
		t.Errorf("sample 2 should pass: %s", r.Reason)
	}
	if r := byIndex[0]; r.Pass || !strings.Contains(r.Reason, "status") || !strings.Contains(r.Reason, "IDENTITY_MISMATCH") {
		t.Errorf("sample 0 should fail on status and flag: %+v", r)
	}
	if r := byIndex[12]; r.Pass || !strings.Contains(r.Reason, "HTTP 200, want [422]") {
		t.Errorf("sample 12 should fail on HTTP: %+v", r)
	}
}

func TestExactFlagsAndRerunTolerance(t *testing.T) {
	steps, _ := scenario.Samples()
	e := steps[0].Expect // flagged with exactly IDENTITY_MISMATCH; duplicate allowed on a re-run
	mk := func(status string, flags ...string) (bool, string) {
		a := api.Ack{Status: status, EventID: "e"}
		for _, c := range flags {
			a.Flags = append(a.Flags, struct {
				Code string `json:"code"`
			}{c})
		}
		return evaluate(e, api.Response{HTTP: 200}, a, true)
	}
	if ok, why := mk("flagged", "IDENTITY_MISMATCH"); !ok {
		t.Fatal(why)
	}
	if ok, why := mk("duplicate", "IDENTITY_MISMATCH"); !ok {
		t.Fatal("a re-run duplicate must pass: " + why)
	}
	if ok, _ := mk("flagged", "IDENTITY_MISMATCH", "EXTRA"); ok {
		t.Fatal("an extra flag must fail an exact expectation")
	}
	if ok, _ := mk("flagged"); ok {
		t.Fatal("a missing flag must fail")
	}
	if ok, _ := mk("accepted", "IDENTITY_MISMATCH"); ok {
		t.Fatal("wrong status must fail")
	}
}

// Completeness: sent, acknowledged, processed, expected rejections and missing.
func TestCompleteness(t *testing.T) {
	f := newFake(t)
	f.answer = func(n int, _ []byte) (int, string, []string) {
		if n == 4 {
			return 422, "rejected", nil
		}
		return 200, "accepted", nil
	}
	cfg := f.cfg()
	cfg.Params.Points = 6
	rep, _ := Run(context.Background(), cfg, mustGet(t, "route"))
	s := rep.Summary
	// The route expects 200/accepted, so step 4 fails its check, but it was acknowledged and processed.
	if s.Sent != 6 || s.Acknowledged != 6 || s.Processed != 6 || s.Missing != 0 || s.Failed != 1 || s.Verified != 6 {
		t.Fatalf("summary %+v", s)
	}

	// Events the API says it saved but cannot find again are missing; non-final ones are not processed.
	f2 := newFake(t)
	f2.eventStatus = func(id, said string) string {
		switch id {
		case "evt_1":
			return "" // 404
		case "evt_2":
			return "pending"
		}
		return said
	}
	cfg = f2.cfg()
	cfg.Params.Points = 4
	rep, _ = Run(context.Background(), cfg, mustGet(t, "route"))
	s = rep.Summary
	if s.Missing != 1 || s.NotProcessed != 1 || s.Processed != 2 || s.OK() {
		t.Fatalf("summary %+v", s)
	}
}

func TestExpectedRejectionsAreCounted(t *testing.T) {
	f := newFake(t)
	f.answer = func(n int, body []byte) (int, string, []string) {
		if !json.Valid(body) {
			return 400, "rejected", nil
		}
		return 200, "accepted", nil
	}
	// Reuse the malformed scenario; most steps will "fail" against this simple fake, which is fine here.
	rep, _ := Run(context.Background(), f.cfg(), mustGet(t, "malformed"))
	if rep.Summary.ExpectedRejections != 1 { // only the garbage-text step gets exactly 400/rejected from the fake
		t.Fatalf("expected rejections = %d", rep.Summary.ExpectedRejections)
	}
}

func TestNetworkErrorIsMissing(t *testing.T) {
	f := newFake(t)
	cfg := f.cfg()
	f.srv.Close()
	rep, _ := Run(context.Background(), cfg, mustGet(t, "route"))
	s := rep.Summary
	if s.Failed != s.Sent || s.Missing != s.Sent || s.Acknowledged != 0 || s.ErrorRate != 1 {
		t.Fatalf("summary %+v", s)
	}
	if rep.Results[0].NetworkError == "" || !strings.Contains(rep.Results[0].Reason, "network error") {
		t.Fatalf("result %+v", rep.Results[0])
	}
}

func TestLoadRun(t *testing.T) {
	f := newFake(t)
	f.answer = func(n int, _ []byte) (int, string, []string) {
		time.Sleep(time.Duration(n%5) * time.Millisecond)
		if n%10 == 9 {
			return 500, "failed", nil
		}
		return 200, "accepted", nil
	}
	cfg := f.cfg()
	cfg.Params.Rate, cfg.Params.Duration = 100, time.Second
	cfg.VerifyCap = 25
	rep, err := Run(context.Background(), cfg, mustGet(t, "load"))
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Summary
	if s.Sent != 100 || s.Verified != 25 || s.Passed+s.Failed != 100 {
		t.Fatalf("summary %+v", s)
	}
	if s.ErrorRate < 0.09 || s.ErrorRate > 0.11 {
		t.Fatalf("error rate %.3f, want 0.10", s.ErrorRate)
	}
	if !(s.P50Ms <= s.P95Ms && s.P95Ms <= s.P99Ms && s.P99Ms <= s.MaxMs) || s.P50Ms <= 0 {
		t.Fatalf("percentiles %+v", s)
	}
	if s.ThroughputPerSec < 60 || s.ThroughputPerSec > 110 {
		t.Fatalf("throughput %.1f/s for a 100/s target", s.ThroughputPerSec)
	}
	if len(rep.Notes) == 0 || !strings.Contains(rep.Notes[0], "25 of") {
		t.Fatalf("a sampled check must say so: %v", rep.Notes)
	}
	// Memory: bodies are kept only for the first few results and for failures.
	kept := 0
	for _, r := range rep.Results {
		if r.Request != "" {
			kept++
		}
	}
	if kept > loadDetailKept+s.Failed {
		t.Fatalf("kept %d bodies", kept)
	}
}

func TestLoadStopsWhenContextEnds(t *testing.T) {
	f := newFake(t)
	cfg := f.cfg()
	cfg.Params.Rate, cfg.Params.Duration = 50, 10*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rep, _ := Run(ctx, cfg, mustGet(t, "load"))
	if rep.Summary.Sent == 0 || rep.Summary.Sent > 40 || len(rep.Notes) == 0 {
		t.Fatalf("sent %d notes %v", rep.Summary.Sent, rep.Notes)
	}
}

func TestPercentile(t *testing.T) {
	var s []float64
	for i := 1; i <= 100; i++ {
		s = append(s, float64(i))
	}
	for p, want := range map[float64]float64{50: 50, 95: 95, 99: 99, 100: 100, 1: 1} {
		if got := Percentile(s, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if Percentile(nil, 50) != 0 || Percentile([]float64{7}, 99) != 7 {
		t.Fatal("edge cases")
	}
	if got := Percentile([]float64{1, 2, 3, 4}, 50); got != 2 {
		t.Fatalf("p50 of 4 = %v (nearest rank)", got)
	}
}

func TestRunNeedsClient(t *testing.T) {
	if _, err := Run(context.Background(), Config{}, mustGet(t, "auth")); err == nil {
		t.Fatal("no client must be an error")
	}
}

// A scripted step that hits the rate limit is retried and still checked; the report says so.
func TestScriptedStepsRetryOn429(t *testing.T) {
	var posts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/webhook/tive", func(w http.ResponseWriter, r *http.Request) {
		n := posts.Add(1)
		if n == 2 { // the second request is rate limited once
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		fmt.Fprintf(w, `{"event_id":"evt_%d","status":"accepted","flags":[],"errors":[]}`, n)
	})
	mux.HandleFunc("GET /api/v1/events/{id}", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"status":"accepted"}`) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{Client: api.New(srv.URL), Key: goodKey, RunID: "r", Params: scenario.Params{Points: 3, Seed: 1}}
	rep, _ := Run(context.Background(), cfg, mustGet(t, "route"))
	if !rep.Summary.OK() || rep.Summary.Sent != 3 {
		t.Fatalf("%+v", rep.Summary)
	}
	r := rep.Results[1]
	if r.Retries != 1 || !strings.Contains(r.Note, "rate limited (429), succeeded after 1 retry") {
		t.Fatalf("result %+v", r)
	}

	// A load run does not retry: a 429 there is a measurement.
	posts.Store(0)
	lcfg := cfg
	lcfg.Params.Rate, lcfg.Params.Duration = 10, time.Second
	lrep, _ := Run(context.Background(), lcfg, mustGet(t, "load"))
	found429 := false
	for _, x := range lrep.Results {
		found429 = found429 || x.HTTP == 429
		if x.Retries != 0 {
			t.Fatal("load must not retry")
		}
	}
	if !found429 || lrep.Summary.OK() {
		t.Fatal("a load run must surface the 429 as a failure")
	}
}

// Lookups that fail are reported, not silently skipped.
func TestLookupErrorsFailTheRun(t *testing.T) {
	mux := http.NewServeMux()
	var n atomic.Int32
	mux.HandleFunc("POST /api/webhook/tive", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"event_id":"evt_%d","status":"accepted","flags":[],"errors":[]}`, n.Add(1))
	})
	mux.HandleFunc("GET /api/v1/events/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rep, _ := Run(context.Background(), Config{Client: api.New(srv.URL), Key: goodKey, RunID: "r", Params: scenario.Params{Points: 3, Seed: 1}}, mustGet(t, "route"))
	s := rep.Summary
	if s.LookupErrors != 3 || s.Verified != 0 || s.OK() || s.Missing != 0 {
		t.Fatalf("%+v", s)
	}
	if len(rep.Notes) == 0 || !strings.Contains(strings.Join(rep.Notes, " "), "lookups failed") {
		t.Fatalf("notes %v", rep.Notes)
	}
}

// An explicit refusal (429) is not a lost payload: it is counted as refused, still fails the check, and is not "missing".
func TestRefusedIsNotMissing(t *testing.T) {
	mux := http.NewServeMux()
	var n atomic.Int32
	mux.HandleFunc("POST /api/webhook/tive", func(w http.ResponseWriter, _ *http.Request) {
		if k := n.Add(1); k%2 == 0 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"errors":[{"code":"RATE_LIMITED"}]}`)
			return
		} else {
			fmt.Fprintf(w, `{"event_id":"evt_%d","status":"accepted","flags":[],"errors":[]}`, k)
		}
	})
	mux.HandleFunc("GET /api/v1/events/{id}", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"status":"accepted"}`) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{Client: api.New(srv.URL), Key: goodKey, RunID: "r", Params: scenario.Params{Rate: 20, Duration: time.Second, Seed: 1}}
	rep, _ := Run(context.Background(), cfg, mustGet(t, "load"))
	s := rep.Summary
	if s.Sent != 20 || s.Refused != 10 || s.Missing != 0 || s.Acknowledged != 10 || s.Failed != 10 || s.OK() {
		t.Fatalf("%+v", s)
	}
}
