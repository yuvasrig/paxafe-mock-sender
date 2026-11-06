package runner

import (
	"context"
	"strings"
	"testing"
	"time"
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

func TestParseLoad(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)
	l, err := ParseLoad("scenarios=route,fault", now)
	if err != nil || len(l.Scenarios) != 2 || l.Rate != 10 || l.Duration != 10*time.Second || !l.Start.IsZero() {
		t.Fatalf("defaults: rate 10, 10s, start now: %+v %v", l, err)
	}
	l, err = ParseLoad("scenarios=malformed rate=25.5 duration=2m start=+45s", now)
	if err != nil || l.Rate != 25.5 || l.Duration != 2*time.Minute || !l.Start.Equal(now.Add(45*time.Second)) || l.Scenarios[0] != "malformed" {
		t.Fatalf("%+v %v", l, err)
	}
	if l, err = ParseLoad("scenario=auth start=now", now); err != nil || !l.Start.IsZero() || l.Scenarios[0] != "auth" {
		t.Fatalf("%+v %v", l, err)
	}
	if l, err = ParseLoad("scenarios=route start=12:30:15", now); err != nil || !l.Start.Equal(time.Date(2026, 11, 6, 12, 30, 15, 0, time.Local)) {
		t.Fatalf("time of day on this machine's clock: %+v %v", l, err)
	}
	if l, err = ParseLoad("scenarios=route start=12:05", now); err != nil || !l.Start.Equal(time.Date(2026, 11, 6, 12, 5, 0, 0, time.Local)) {
		t.Fatalf("%+v %v", l, err)
	}
	abs := "2026-11-06T13:00:00Z"
	if l, err = ParseLoad("scenarios=route start="+abs, now); err != nil || l.Start.UTC().Format(time.RFC3339) != abs {
		t.Fatalf("%+v %v", l, err)
	}
	for name, spec := range map[string]string{
		"no scenarios":    "rate=5",
		"empty":           "",
		"not key=value":   "scenarios=route fast",
		"unknown key":     "scenarios=route speed=5",
		"given twice":     "scenarios=route rate=5 rate=6",
		"zero rate":       "scenarios=route rate=0",
		"word rate":       "scenarios=route rate=fast",
		"bad duration":    "scenarios=route duration=soon",
		"zero duration":   "scenarios=route duration=0s",
		"bad offset":      "scenarios=route start=+soon",
		"negative offset": "scenarios=route start=+-5s",
		"past time":       "scenarios=route start=11:00",
		"nonsense start":  "scenarios=route start=tomorrow",
		"empty value":     "scenarios=",
	} {
		if _, err := ParseLoad(spec, now); err == nil {
			t.Errorf("%s should be refused: %q", name, spec)
		}
	}
}
