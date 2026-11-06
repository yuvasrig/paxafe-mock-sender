// Package runner executes a scenario against the API and checks completeness afterwards:
// every payload the API acknowledged must be found again with a final status.
package runner

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
	"github.com/yuvasrig/paxafe-mock-sender/internal/scenario"
)

// refusals are explicit "not now" answers: the payload was not taken, and the sender knows it.
var refusals = map[int]bool{401: true, 413: true, 429: true, 503: true}

// terminal statuses: the API has finished with the event.
var terminal = map[string]bool{"accepted": true, "flagged": true, "rejected": true, "duplicate": true, "conflict": true}

const (
	defaultVerifyCap   = 300
	defaultConcurrency = 32
	maxDetail          = 8 << 10
	loadDetailKept     = 20
)

// Config describes one run.
type Config struct {
	Client      *api.Client
	Key         string
	RunID       string
	Params      scenario.Params
	VerifyCap   int           // most events to look up afterwards (default 300); a load run samples evenly
	AsyncWait   time.Duration // how long to wait for workers to finish a queued event (default 60s)
	Concurrency int           // load workers (default 32)
}

// Result is one request and what came back.
type Result struct {
	Index        int      `json:"index"`
	Name         string   `json:"name"`
	RequestID    string   `json:"request_id"`
	HTTP         int      `json:"http"`
	Status       string   `json:"status"`
	EventID      string   `json:"event_id"`
	Flags        []string `json:"flags"`
	Errors       []string `json:"errors"`
	LatencyMs    float64  `json:"latency_ms"`
	Pass         bool     `json:"pass"`
	Reason       string   `json:"reason,omitempty"`
	Note         string   `json:"note,omitempty"`
	NetworkError string   `json:"network_error,omitempty"`
	Retries      int      `json:"retries,omitempty"`
	Sent         *Sample  `json:"sent,omitempty"`      // the temperature and position we sent
	Got          *Sample  `json:"processed,omitempty"` // what the API stored for it
	Compare      string   `json:"compare,omitempty"`   // match, differs, dropped or not stored
	CompareNote  string   `json:"compare_note,omitempty"`
	Queued       bool     `json:"queued,omitempty"` // the API answered 202: an async tenant, processed by a worker later

	Verified     bool   `json:"verified"`
	LookupError  string `json:"lookup_error,omitempty"`
	StoredStatus string `json:"stored_status,omitempty"`
	Found        bool   `json:"found"`

	Expect          scenario.Expect `json:"-"`
	ExpectAck       bool            `json:"expect_ack"`
	ExpectRejection bool            `json:"expect_rejection"`
	Request         string          `json:"request,omitempty"`
	Response        string          `json:"response,omitempty"`
}

// Summary is the completeness and performance picture of a run.
type Summary struct {
	Sent               int     `json:"sent"`
	Acknowledged       int     `json:"acknowledged"`
	Compared           int     `json:"compared"` // events whose stored values were compared with what was sent
	Matched            int     `json:"matched"`
	Differed           int     `json:"differed"` // stored differently with nothing to explain it
	Fixed              int     `json:"fixed"`    // stored differently, and a flag says a rule changed it
	Dropped            int     `json:"dropped"`  // sent, but nulled by a rule
	Queued             int     `json:"queued"`   // acknowledged with 202 and judged after the workers finished them
	Processed          int     `json:"processed"`
	ExpectedRejections int     `json:"expected_rejections"`
	Missing            int     `json:"missing"`
	Refused            int     `json:"refused"`
	NotProcessed       int     `json:"not_processed"`
	Verified           int     `json:"verified"`
	LookupErrors       int     `json:"lookup_errors"`
	Passed             int     `json:"passed"`
	Failed             int     `json:"failed"`
	P50Ms              float64 `json:"p50_ms"`
	P95Ms              float64 `json:"p95_ms"`
	P99Ms              float64 `json:"p99_ms"`
	MaxMs              float64 `json:"max_ms"`
	ThroughputPerSec   float64 `json:"throughput_per_sec"`
	ErrorRate          float64 `json:"error_rate"`
}

// OK is true when every check passed and nothing is missing or unprocessed.
func (s Summary) OK() bool {
	return s.Failed == 0 && s.Missing == 0 && s.NotProcessed == 0 && s.LookupErrors == 0
}

// Report is a finished run.
type Report struct {
	RunID      string        `json:"run_id"`
	ScenarioID string        `json:"scenario"`
	Title      string        `json:"title"`
	Started    time.Time     `json:"started"`
	Elapsed    time.Duration `json:"elapsed"`
	Results    []Result      `json:"results"`
	Summary    Summary       `json:"summary"`
	Notes      []string      `json:"notes,omitempty"`
}

// Run executes the scenario and verifies the outcome.
func Run(ctx context.Context, cfg Config, sc scenario.Scenario) (*Report, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("runner: no API client")
	}
	if cfg.VerifyCap <= 0 {
		cfg.VerifyCap = defaultVerifyCap
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.Params.Now.IsZero() {
		cfg.Params.Now = time.Now().UTC().Truncate(time.Second)
	}
	if cfg.Params.Seed == 0 {
		cfg.Params.Seed = time.Now().UnixNano()
	}

	rep := &Report{RunID: cfg.RunID, ScenarioID: sc.ID, Title: sc.Title, Started: time.Now()}
	if sc.Load {
		rep.Results = runLoad(ctx, cfg, sc)
	} else {
		steps := sc.Build(cfg.Params)
		rep.Results = make([]Result, 0, len(steps))
		for i, st := range steps {
			if ctx.Err() != nil {
				break
			}
			rep.Results = append(rep.Results, execute(ctx, cfg, i, st, true, true))
		}
	}
	rep.Elapsed = time.Since(rep.Started)

	verified := verify(ctx, cfg, rep.Results)
	rep.Summary = summarize(rep.Results, rep.Elapsed)
	rep.Summary.Verified = verified - rep.Summary.LookupErrors
	if rep.Summary.LookupErrors > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d lookups failed, so those events are unverified", rep.Summary.LookupErrors))
	}
	if verified < rep.Summary.Acknowledged {
		rep.Notes = append(rep.Notes, fmt.Sprintf("completeness checked on %d of %d acknowledged events (an even sample)", verified, rep.Summary.Acknowledged))
	}
	if err := ctx.Err(); err != nil {
		rep.Notes = append(rep.Notes, "run stopped early: "+err.Error())
	}
	return rep, nil
}

func keyFor(mode scenario.KeyMode, runKey string) string {
	switch mode {
	case scenario.KeyNone:
		return ""
	case scenario.KeyInvalid:
		return "pxk_test_" + strings.Repeat("A", 43) // well-formed, but not a key the API knows
	case scenario.KeyMalformed:
		return "this-is-not-an-api-key"
	}
	return runKey
}

// maxAttempts is how often a scripted step is tried when the API answers 429. A load run never retries:
// a 429 there is a measurement, not an obstacle.
const maxAttempts = 8

func execute(ctx context.Context, cfg Config, i int, st scenario.Step, keepDetail, retry bool) Result {
	reqID := fmt.Sprintf("%s-%d", cfg.RunID, i)
	var resp api.Response
	if retry {
		resp = cfg.Client.PostRetry(ctx, keyFor(st.Key, cfg.Key), reqID, st.Body, maxAttempts)
	} else {
		resp = cfg.Client.Post(ctx, keyFor(st.Key, cfg.Key), reqID, st.Body)
	}

	r := Result{
		Index: i, Name: st.Name, RequestID: reqID, HTTP: resp.HTTP, LatencyMs: float64(resp.Latency.Microseconds()) / 1000,
		Note: st.Expect.Note, Expect: st.Expect, ExpectAck: st.Expect.Ack, ExpectRejection: st.Expect.Rejection,
		Flags: []string{}, Errors: []string{},
	}
	if resp.Err != nil {
		r.NetworkError = resp.Err.Error()
	}
	if resp.Retries > 0 {
		r.Retries = resp.Retries
		if r.Note != "" {
			r.Note += "; "
		}
		r.Note += fmt.Sprintf("rate limited (429), succeeded after %d retr%s", resp.Retries, map[bool]string{true: "y", false: "ies"}[resp.Retries == 1])
	}
	if smp, ok := sampleOfRequest(st.Body); ok {
		r.Sent = &smp
	}
	ack, parsed := resp.Parse()
	if parsed {
		r.Status, r.EventID = ack.Status, ack.EventID
		r.Flags, r.Errors = ack.FlagCodes(), ack.ErrorCodes()
	}
	r.Queued = resp.HTTP == http.StatusAccepted && ack.Status == "queued"
	r.Pass, r.Reason = evaluate(st.Expect, resp, ack, parsed)
	if keepDetail || !r.Pass {
		r.Request, r.Response = clip(st.Body), clip(resp.Body)
	}
	return r
}

func clip(b []byte) string {
	if len(b) > maxDetail {
		return string(b[:maxDetail]) + "\n… (truncated)"
	}
	return string(b)
}

func evaluate(e scenario.Expect, resp api.Response, ack api.Ack, parsed bool) (bool, string) {
	if resp.Err != nil {
		return false, "network error: " + resp.Err.Error()
	}
	if resp.HTTP == http.StatusAccepted && ack.Status == "queued" {
		// An async tenant: the payload is saved and queued, and the real result is not known yet. All that can be
		// checked now is that it was acknowledged; verify() waits for the workers and judges the outcome.
		if !parsed || ack.EventID == "" {
			return false, "202 without an event_id: the payload was not acknowledged"
		}
		return true, ""
	}
	var why []string
	wantHTTP := e.HTTP
	if len(wantHTTP) == 0 {
		wantHTTP = []int{200}
	}
	if !containsInt(wantHTTP, resp.HTTP) {
		why = append(why, fmt.Sprintf("HTTP %d, want %v", resp.HTTP, wantHTTP))
	}
	if e.Ack && (!parsed || ack.EventID == "") {
		why = append(why, "no event_id in the response: the payload was not acknowledged")
	}
	if len(e.Status) > 0 && !containsStr(e.Status, ack.Status) {
		why = append(why, fmt.Sprintf("status %q, want %v", ack.Status, e.Status))
	}
	got := ack.FlagCodes()
	for _, want := range e.Flags {
		if !containsStr(got, want) {
			why = append(why, fmt.Sprintf("missing flag %s (got %v)", want, got))
		}
	}
	if e.ExactFlags {
		for _, g := range got {
			if !containsStr(e.Flags, g) {
				why = append(why, fmt.Sprintf("unexpected flag %s (want %v)", g, e.Flags))
			}
		}
	}
	return len(why) == 0, strings.Join(why, "; ")
}

func containsInt(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func containsStr(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// runLoad sends Rate requests per second for Duration, with a bounded pool of workers. If the API (or the
// network) cannot keep up the pacer waits, so the achieved throughput comes out lower than the target.
func runLoad(ctx context.Context, cfg Config, sc scenario.Scenario) []Result {
	rate, dur := cfg.Params.Rate, cfg.Params.Duration
	if rate <= 0 {
		rate = 10
	}
	if dur <= 0 {
		dur = 10 * time.Second
	}
	total := int(rate * dur.Seconds())
	if total < 1 {
		total = 1
	}

	jobs := make(chan int)
	results := make([]Result, total)
	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = execute(ctx, cfg, i, sc.Next(cfg.Params, i), i < loadDetailKept, false)
			}
		}()
	}

	start := time.Now()
	interval := time.Duration(float64(time.Second) / rate)
pace:
	for i := 0; i < total; i++ {
		if wait := time.Until(start.Add(time.Duration(i) * interval)); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				total = i
				break pace
			}
		}
		select {
		case jobs <- i:
		case <-ctx.Done():
			total = i
			break pace
		}
	}
	close(jobs)
	wg.Wait()
	return results[:total]
}

// verify looks events up again and records what the API stored. It returns how many lookups it attempted.
func verify(ctx context.Context, cfg Config, results []Result) int {
	var idx []int
	for i := range results {
		if results[i].EventID != "" && results[i].NetworkError == "" {
			idx = append(idx, i)
		}
	}
	if len(idx) > cfg.VerifyCap { // an even sample
		step := float64(len(idx)) / float64(cfg.VerifyCap)
		sampled := make([]int, 0, cfg.VerifyCap)
		for k := 0; k < cfg.VerifyCap; k++ {
			sampled = append(sampled, idx[int(float64(k)*step)])
		}
		idx = sampled
	}

	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, i := range idx {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if results[i].Queued {
				awaitWorkers(ctx, cfg, &results[i])
				return
			}
			info, found, err := cfg.Client.Event(ctx, cfg.Key, results[i].EventID)
			if err != nil {
				results[i].LookupError = err.Error()
				return
			}
			results[i].Verified, results[i].Found, results[i].StoredStatus = true, found, info.Status
			if found {
				results[i].attach(info)
			}
		}(i)
	}
	wg.Wait()
	return len(idx)
}

const defaultAsyncWait = 60 * time.Second

// awaitWorkers follows a queued event until a worker has given it a final status (or the wait runs out), then
// judges it by the scenario's expectations, which could not be checked at acknowledgement time.
func awaitWorkers(ctx context.Context, cfg Config, r *Result) {
	wait := cfg.AsyncWait
	if wait <= 0 {
		wait = defaultAsyncWait
	}
	deadline := time.Now().Add(wait)
	pause := 100 * time.Millisecond
	var info api.EventInfo
	var found bool
	for {
		var err error
		info, found, err = cfg.Client.Event(ctx, cfg.Key, r.EventID)
		if err != nil {
			r.LookupError = err.Error()
			return
		}
		if !found || terminal[info.Status] || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(pause):
		}
		if pause < time.Second {
			pause *= 2
		}
	}
	r.Verified, r.Found, r.StoredStatus = true, found, info.Status
	if !found || !terminal[info.Status] {
		return // the summary reports it as missing or not processed
	}
	r.Status, r.Flags = info.Status, info.Flags
	r.attach(info)
	var why []string
	if len(r.Expect.Status) > 0 && !containsStr(r.Expect.Status, info.Status) {
		why = append(why, fmt.Sprintf("final status %q, want %v", info.Status, r.Expect.Status))
	}
	for _, want := range r.Expect.Flags {
		if !containsStr(info.Flags, want) {
			why = append(why, fmt.Sprintf("missing flag %s (got %v)", want, info.Flags))
		}
	}
	if r.Expect.Rejection && info.Status != "rejected" {
		why = append(why, fmt.Sprintf("expected a rejection, final status %q", info.Status))
	}
	if len(why) > 0 {
		r.Pass, r.Reason = false, strings.Join(why, "; ")
	}
}

func summarize(results []Result, elapsed time.Duration) Summary {
	s := Summary{Sent: len(results)}
	var lat []float64
	errs := 0
	for _, r := range results {
		if r.Pass {
			s.Passed++
		} else {
			s.Failed++
		}
		if r.EventID != "" {
			s.Acknowledged++
		}
		if r.Queued {
			s.Queued++
		}
		switch r.Compare {
		case "":
		case VerdictMatch:
			s.Compared++
			s.Matched++
		case VerdictDiffers:
			s.Compared++
			s.Differed++
		case VerdictFixed:
			s.Compared++
			s.Fixed++
		case VerdictDropped:
			s.Compared++
			s.Dropped++
		default: // not stored: nothing to compare
		}
		if r.Pass && r.ExpectRejection {
			s.ExpectedRejections++
		}
		if r.HTTP > 0 {
			lat = append(lat, r.LatencyMs)
		}
		if r.HTTP >= 500 || r.NetworkError != "" {
			errs++
		}
		if r.ExpectAck && r.EventID == "" && refusals[r.HTTP] {
			s.Refused++ // the API said no (429, 503, ...): the sender knows it must retry, nothing was lost silently
		} else if r.ExpectAck && (r.EventID == "" || (r.Verified && !r.Found)) {
			s.Missing++
		}
		if r.LookupError != "" {
			s.LookupErrors++
		}
		if r.Verified && r.Found {
			if terminal[r.StoredStatus] {
				s.Processed++
			} else {
				s.NotProcessed++
			}
		}
	}
	sort.Float64s(lat)
	s.P50Ms, s.P95Ms, s.P99Ms = Percentile(lat, 50), Percentile(lat, 95), Percentile(lat, 99)
	if n := len(lat); n > 0 {
		s.MaxMs = lat[n-1]
	}
	if elapsed > 0 {
		s.ThroughputPerSec = float64(s.Sent) / elapsed.Seconds()
	}
	if s.Sent > 0 {
		s.ErrorRate = float64(errs) / float64(s.Sent)
	}
	return s
}

// Percentile is the nearest-rank percentile of an ascending slice (0 for an empty slice).
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p/100*float64(len(sorted)) + 0.999999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
