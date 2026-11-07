package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
	"github.com/yuvasrig/paxafe-mock-sender/internal/scenario"
)

func TestPlanRunsEachLoadOnItsOwnSchedule(t *testing.T) {
	f := newFake(t)
	cfg := f.cfg()
	start := time.Now()
	loads := []Load{
		{Scenarios: []string{"route", "fault"}, Rate: 20, Duration: time.Second},                                       // starts now
		{Scenarios: []string{"malformed"}, Rate: 10, Duration: time.Second, Start: start.Add(1200 * time.Millisecond)}, // starts later
	}
	rep, err := RunPlan(context.Background(), cfg, loads)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Loads) != 2 || rep.Loads[0].Index != 1 || rep.Loads[1].Index != 2 {
		t.Fatalf("a summary per load: %+v", rep.Loads)
	}
	if gap := rep.Loads[1].StartedAt.Sub(rep.Loads[0].StartedAt); gap < 1000*time.Millisecond {
		t.Fatalf("the second load waits for its own start time: %s after the first", gap)
	}
	if rep.Loads[0].Sent != 20 || rep.Loads[1].Sent != 10 || rep.Summary.Sent != 30 {
		t.Fatalf("each load sends rate x duration: %+v sent %d", rep.Loads, rep.Summary.Sent)
	}
	if !strings.Contains(rep.Loads[0].Title, "Realistic route") || !strings.Contains(rep.Loads[0].Title, "Sensor fault") || !strings.Contains(rep.Loads[1].Title, "Malformed") {
		t.Fatalf("each load draws from its own scenarios: %q %q", rep.Loads[0].Title, rep.Loads[1].Title)
	}
	// One table, numbered from 1, in the order the loads started.
	seen := map[int]int{}
	for i, r := range rep.Results {
		if r.Index != i {
			t.Fatalf("result %d has index %d", i, r.Index)
		}
		seen[r.Load]++
	}
	if seen[1] != 20 || seen[2] != 10 {
		t.Fatalf("results are tagged with their load: %v", seen)
	}
	if rep.Results[0].Load != 1 || rep.Results[len(rep.Results)-1].Load != 2 {
		t.Fatal("ordered by when each load started")
	}
	if rep.Title != "Load plan: 2 loads" || rep.ScenarioID != "plan" {
		t.Fatalf("%q %q", rep.Title, rep.ScenarioID)
	}
	ids := map[string]bool{}
	for _, r := range rep.Results {
		if ids[r.RequestID] {
			t.Fatalf("request id %s repeats across loads", r.RequestID)
		}
		ids[r.RequestID] = true
	}
}

func TestLoadsOfAPlanOverlapInTime(t *testing.T) {
	f := newFake(t)
	loads := []Load{
		{Scenarios: []string{"route"}, Rate: 10, Duration: 1500 * time.Millisecond},
		{Scenarios: []string{"fault"}, Rate: 10, Duration: 1500 * time.Millisecond},
	}
	began := time.Now()
	rep, err := RunPlan(context.Background(), f.cfg(), loads)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 2800*time.Millisecond {
		t.Fatalf("two simultaneous 1.5 s loads must not run one after the other: %s", took)
	}
	if rep.Summary.Sent != 30 {
		t.Fatalf("sent %d", rep.Summary.Sent)
	}
}

func TestPlanIsCheckedBeforeAnythingIsSent(t *testing.T) {
	f := newFake(t)
	cases := map[string][]Load{
		"empty":            nil,
		"unknown scenario": {{Scenarios: []string{"route"}, Rate: 5, Duration: time.Second}, {Scenarios: []string{"nope"}, Rate: 5, Duration: time.Second}},
		"no scenario":      {{Scenarios: nil, Rate: 5, Duration: time.Second}},
		"zero rate":        {{Scenarios: []string{"route"}, Rate: 0, Duration: time.Second}},
		"zero duration":    {{Scenarios: []string{"route"}, Rate: 5}},
	}
	for name, loads := range cases {
		if _, err := RunPlan(context.Background(), f.cfg(), loads); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
	if f.posts.Load() != 0 {
		t.Fatalf("a bad plan sends nothing, but %d requests went out", f.posts.Load())
	}
	if _, err := RunPlan(context.Background(), Config{}, []Load{{Scenarios: []string{"route"}, Rate: 1, Duration: time.Second}}); err == nil {
		t.Fatal("no client")
	}
}

func TestPlanStopsWaitingWhenCancelled(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	rep, err := RunPlan(ctx, f.cfg(), []Load{{Scenarios: []string{"route"}, Rate: 5, Duration: time.Second, Start: time.Now().Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(began) > 3*time.Second || rep.Summary.Sent != 0 || len(rep.Loads) != 0 {
		t.Fatalf("a load that never started sends nothing and does not hold the run: %s %+v", time.Since(began), rep.Summary)
	}
}

func TestASingleLoadPlanReadsLikeALoad(t *testing.T) {
	f := newFake(t)
	rep, err := RunPlan(context.Background(), f.cfg(), []Load{{Scenarios: []string{"route"}, Rate: 10, Duration: time.Second}})
	if err != nil || rep.Title != "Load: route" || len(rep.Loads) != 1 || rep.Summary.Sent != 10 {
		t.Fatalf("%v %q %+v", err, rep.Title, rep.Loads)
	}
}

func one(t *testing.T, spec string, now time.Time) Load {
	t.Helper()
	ls, err := ParseLoads(spec, now)
	if err != nil || len(ls) != 1 {
		t.Fatalf("%q: %v %+v", spec, err, ls)
	}
	return ls[0]
}

func TestParseLoads(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)
	l := one(t, "scenarios=route,fault", now)
	if len(l.Scenarios) != 2 || l.Rate != 10 || l.Duration != 10*time.Second || !l.Start.IsZero() || l.Key != "" {
		t.Fatalf("defaults: rate 10, 10s, start now, the run's key: %+v", l)
	}
	l = one(t, "scenarios=malformed rate=25.5 duration=2m start=+45s", now)
	if l.Rate != 25.5 || l.Duration != 2*time.Minute || !l.Start.Equal(now.Add(45*time.Second)) || l.Scenarios[0] != "malformed" {
		t.Fatalf("%+v", l)
	}
	if l = one(t, "scenario=auth start=now", now); !l.Start.IsZero() || l.Scenarios[0] != "auth" {
		t.Fatalf("%+v", l)
	}
	if l = one(t, "scenarios=route start=12:30:15", now); !l.Start.Equal(time.Date(2026, 11, 6, 12, 30, 15, 0, time.Local)) {
		t.Fatalf("time of day on this machine's clock: %+v", l)
	}
	if l = one(t, "scenarios=route start=12:05", now); !l.Start.Equal(time.Date(2026, 11, 6, 12, 5, 0, 0, time.Local)) {
		t.Fatalf("%+v", l)
	}
	abs := "2026-11-06T13:00:00Z"
	if l = one(t, "scenarios=route start="+abs, now); l.Start.UTC().Format(time.RFC3339) != abs {
		t.Fatalf("%+v", l)
	}
	for name, spec := range map[string]string{
		"no scenarios": "rate=5", "empty": "", "not key=value": "scenarios=route fast", "unknown key": "scenarios=route speed=5",
		"given twice": "scenarios=route rate=5 rate=6", "zero rate": "scenarios=route rate=0", "word rate": "scenarios=route rate=fast",
		"bad duration": "scenarios=route duration=soon", "zero duration": "scenarios=route duration=0s", "bad offset": "scenarios=route start=+soon",
		"negative offset": "scenarios=route start=+-5s", "past time": "scenarios=route start=11:00", "nonsense start": "scenarios=route start=tomorrow",
		"empty value": "scenarios=", "key and keys": "scenarios=route key=a keys=b", "empty key in a list": "scenarios=route keys=a,,b",
	} {
		if _, err := ParseLoads(spec, now); err == nil {
			t.Errorf("%s should be refused: %q", name, spec)
		}
	}
}

func TestParseLoadsTargetsTenantsByKey(t *testing.T) {
	now := time.Now()
	l := one(t, "scenarios=route key=pxk_test_AAAA1111secret label=Acme", now)
	if l.Key != "pxk_test_AAAA1111secret" || l.Label != "Acme" {
		t.Fatalf("%+v", l)
	}
	// Several keys: one load per key, everything else the same, so one spec triggers the same load on several tenants.
	ls, err := ParseLoads("scenarios=route,fault rate=15 duration=20s start=+5s keys=pxk_test_AAAA1111,pxk_test_BBBB2222,pxk_test_CCCC3333", now)
	if err != nil || len(ls) != 3 {
		t.Fatalf("%v %+v", err, ls)
	}
	for i, k := range []string{"pxk_test_AAAA1111", "pxk_test_BBBB2222", "pxk_test_CCCC3333"} {
		if ls[i].Key != k || ls[i].Rate != 15 || ls[i].Duration != 20*time.Second || !ls[i].Start.Equal(ls[0].Start) || len(ls[i].Scenarios) != 2 {
			t.Fatalf("load %d: %+v", i, ls[i])
		}
	}
	// A key can come from the environment, so the secret need not be on the command line.
	t.Setenv("ACME_KEY", "pxk_test_FROMENV0000")
	t.Setenv("BIOCO_KEY", "pxk_test_FROMENV1111")
	ls, err = ParseLoads("scenarios=route keys=env:ACME_KEY,env:BIOCO_KEY", now)
	if err != nil || len(ls) != 2 || ls[0].Key != "pxk_test_FROMENV0000" || ls[1].Key != "pxk_test_FROMENV1111" {
		t.Fatalf("%v %+v", err, ls)
	}
	_, err = ParseLoads("scenarios=route key=env:NO_SUCH_VARIABLE_SET", now)
	if err == nil || !strings.Contains(err.Error(), "NO_SUCH_VARIABLE_SET") {
		t.Fatalf("a missing variable is named: %v", err)
	}
	_, err = ParseLoads("scenarios=route rate=x key=pxk_test_SECRETSECRET", now)
	if err == nil || strings.Contains(err.Error(), "SECRETSECRET") {
		t.Fatalf("an error must not contain the key: %v", err)
	}
}

func TestKeyPrefixAndTarget(t *testing.T) {
	if got := KeyPrefix("pxk_test_DbixqLRh6iJxiWaFl4NfSjj1FOreitCl4T0wdvOUDmx"); got != "pxk_test_Dbix…" || strings.Contains(got, "qLRh") {
		t.Fatalf("only the prefix is shown: %q", got)
	}
	if got := KeyPrefix("short"); strings.Contains(got, "short") {
		t.Fatalf("a short key is not shown at all: %q", got)
	}
	k := "pxk_test_DbixqLRh6iJxiWaFl4NfSjj1F"
	for _, c := range []struct {
		l    Load
		want string
	}{
		{Load{Label: "Acme"}, "Acme (pxk_test_Dbix…)"}, {Load{}, "pxk_test_Dbix…"},
	} {
		if got := c.l.Target(k); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
	if (Load{}).Target("") != "run's key" || (Load{Label: "x"}).Target("") != "x" {
		t.Fatal("no key")
	}
}

// tenantAPI is a stand-in API for several tenants: each key is a tenant, a webhook is stored under the tenant of its
// key, and an event can only be read back with the key that stored it, as in the real API.
type tenantAPI struct {
	*httptest.Server
	mu     sync.Mutex
	owner  map[string]string // event id -> key
	posts  map[string]int    // key -> webhooks received
	keys   map[string]bool
	nextID int
}

func newTenantAPI(t *testing.T, keys ...string) *tenantAPI {
	f := &tenantAPI{owner: map[string]string{}, posts: map[string]int{}, keys: map[string]bool{}}
	for _, k := range keys {
		f.keys[k] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/webhook/tive", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if !f.keys[key] {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("evt_%d", f.nextID)
		f.owner[id] = key
		f.posts[key]++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": id, "status": "accepted", "flags": []any{}, "errors": []any{}})
	})
	mux.HandleFunc("GET /api/v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		owner, ok := f.owner[r.PathValue("id")]
		f.mu.Unlock()
		if !ok || owner != r.Header.Get("X-API-Key") { // another tenant's event is invisible
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"status":"accepted","flags":[]}`)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestLoadsTriggeredOnSeveralTenantsAtTheSameTime(t *testing.T) {
	keyA, keyB := "pxk_test_AAAAsecretA0000", "pxk_test_BBBBsecretB1111"
	f := newTenantAPI(t, keyA, keyB)
	cfg := Config{Client: api.New(f.URL), RunID: "multi", Params: scenario.Params{Account: 1, Points: 6, Seed: 5}} // no run-level key
	began := time.Now()
	rep, err := RunPlan(context.Background(), cfg, []Load{
		{Key: keyA, Label: "Acme", Scenarios: []string{"route"}, Rate: 20, Duration: 1500 * time.Millisecond},
		{Key: keyB, Label: "BioCo", Scenarios: []string{"route", "fault"}, Rate: 10, Duration: 1500 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 2800*time.Millisecond {
		t.Fatalf("the tenants' loads run at the same time, not one after the other: %s", took)
	}
	if f.posts[keyA] != 30 || f.posts[keyB] != 15 {
		t.Fatalf("each tenant received its own load: %v", f.posts)
	}
	// Every event is read back with the key of the tenant that stored it, so none is missing.
	if rep.Summary.Sent != 45 || rep.Summary.Missing != 0 || rep.Summary.NotProcessed != 0 || rep.Summary.LookupErrors != 0 || rep.Summary.Processed != 45 {
		t.Fatalf("%+v", rep.Summary)
	}
	if rep.Loads[0].Target != "Acme (pxk_test_AAAA…)" || rep.Loads[1].Target != "BioCo (pxk_test_BBBB…)" {
		t.Fatalf("the report names each tenant by label and key prefix: %q %q", rep.Loads[0].Target, rep.Loads[1].Target)
	}
	body, _ := json.Marshal(rep)
	if strings.Contains(string(body), "secretA0000") || strings.Contains(string(body), "secretB1111") {
		t.Fatal("the report must never contain an API key")
	}
}

func TestAWrongKeyForOneTenantIsCaughtWithoutHidingTheOthers(t *testing.T) {
	good, other := "pxk_test_GOODGOODGOOD0", "pxk_test_OTHEROTHER111"
	f := newTenantAPI(t, good, other)
	f.keys["pxk_test_UNKNOWNUNKNOWN"] = false
	rep, err := RunPlan(context.Background(), Config{Client: api.New(f.URL), RunID: "wrong", Params: scenario.Params{Seed: 5, Points: 4}}, []Load{
		{Key: good, Scenarios: []string{"route"}, Rate: 10, Duration: time.Second},
		{Key: "pxk_test_UNKNOWNUNKNOWN", Scenarios: []string{"route"}, Rate: 10, Duration: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Loads[0].Failed != 0 || rep.Loads[1].Failed != 10 {
		t.Fatalf("the bad key fails only its own load: %+v", rep.Loads)
	}
}

func TestEveryLoadNeedsAKeyOfItsOwnOrTheRuns(t *testing.T) {
	f := newTenantAPI(t, "k")
	_, err := RunPlan(context.Background(), Config{Client: api.New(f.URL)}, []Load{{Scenarios: []string{"route"}, Rate: 5, Duration: time.Second}})
	if err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("%v", err)
	}
	if len(f.posts) != 0 {
		t.Fatal("nothing is sent when a load has no key")
	}
}
