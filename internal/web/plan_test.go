package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/testfake"
)

var planNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func load(extra url.Values) url.Values {
	f := url.Values{"load1_scenarios": {"route"}, "load1_rate": {"20"}, "load1_duration": {"10"}}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func TestParsePlanDefaultsToNowWithEveryTickedScenario(t *testing.T) {
	loads, err := parsePlan(load(url.Values{"load1_scenarios": {"route", "fault", "malformed"}}), planNow)
	if err != nil || len(loads) != 1 {
		t.Fatalf("%v %+v", err, loads)
	}
	l := loads[0]
	if !l.Start.IsZero() || len(l.Scenarios) != 3 || l.Rate != 20 || l.Duration != 10*time.Second {
		t.Fatalf("an empty start means now, and every ticked scenario is kept: %+v", l)
	}
}

func TestParsePlanReadsSeveralLoadsAndSkipsRemovedRows(t *testing.T) {
	f := load(url.Values{
		"load3_scenarios": {"duplicates"}, "load3_rate": {"5"}, "load3_duration": {"8"}, // row 2 was removed
		"load5_scenarios": {"auth"}, "load5_rate": {"1"}, "load5_duration": {"3"},
	})
	loads, err := parsePlan(f, planNow)
	if err != nil || len(loads) != 3 || loads[1].Scenarios[0] != "duplicates" || loads[2].Scenarios[0] != "auth" {
		t.Fatalf("%v %+v", err, loads)
	}
}

func TestParsePlanClampsRateAndDuration(t *testing.T) {
	loads, _ := parsePlan(load(url.Values{"load1_rate": {"100000"}, "load1_duration": {"9999"}}), planNow)
	if loads[0].Rate != maxLoadRate || loads[0].Duration != maxLoadDuration {
		t.Fatalf("%+v", loads[0])
	}
	loads, _ = parsePlan(load(url.Values{"load1_rate": {"0"}, "load1_duration": {"-3"}}), planNow)
	if loads[0].Rate != 1 || loads[0].Duration != time.Second {
		t.Fatalf("%+v", loads[0])
	}
	loads, _ = parsePlan(load(url.Values{"load1_rate": {"x"}, "load1_duration": {""}}), planNow)
	if loads[0].Rate != 10 || loads[0].Duration != 10*time.Second {
		t.Fatalf("unreadable numbers fall back to defaults: %+v", loads[0])
	}
}

// The start time is a time of day on the browser's clock; tzoffset is what the browser reports (minutes behind UTC).
func TestParsePlanStartTimeUsesTheBrowsersClock(t *testing.T) {
	// 12:00:00 UTC is 07:00:00 for a browser five hours behind UTC (getTimezoneOffset() = 300).
	loads, err := parsePlan(load(url.Values{"load1_start": {"07:00:30"}, "tzoffset": {"300"}}), planNow)
	if err != nil || !loads[0].Start.Equal(planNow.Add(30*time.Second)) {
		t.Fatalf("%v %+v", err, loads)
	}
	// A browser ahead of UTC (Berlin in winter: offset -60): 13:00:45 local is 12:00:45 UTC.
	loads, err = parsePlan(load(url.Values{"load1_start": {"13:00:45"}, "tzoffset": {"-60"}}), planNow)
	if err != nil || !loads[0].Start.Equal(planNow.Add(45*time.Second)) {
		t.Fatalf("%v %+v", err, loads)
	}
	// Minutes only is fine too.
	if loads, err = parsePlan(load(url.Values{"load1_start": {"12:01"}, "tzoffset": {"0"}}), planNow); err != nil || !loads[0].Start.Equal(planNow.Add(time.Minute)) {
		t.Fatalf("%v %+v", err, loads)
	}
	// A time that has just passed means now.
	if loads, err = parsePlan(load(url.Values{"load1_start": {"11:59:40"}, "tzoffset": {"0"}}), planNow); err != nil || !loads[0].Start.IsZero() {
		t.Fatalf("%v %+v", err, loads)
	}
}

func TestParsePlanRefusesWhatCannotRun(t *testing.T) {
	cases := map[string]url.Values{
		"no scenario ticked":   load(url.Values{"load1_scenarios": nil}),
		"unreadable time":      load(url.Values{"load1_start": {"tomorrow"}}),
		"time long past":       load(url.Values{"load1_start": {"09:00:00"}, "tzoffset": {"0"}}),
		"starts too far ahead": load(url.Values{"load1_start": {"12:05:00"}, "tzoffset": {"0"}}),
	}
	for name, f := range cases {
		if _, err := parsePlan(f, planNow); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
	if _, err := parsePlan(url.Values{"key": {"x"}}, planNow); err == nil {
		t.Error("a plan with no load rows should be refused")
	}
	_, err := parsePlan(url.Values{"load1_scenarios": {"route"}, "load1_rate": {"5"}, "load2_rate": {"5"}}, planNow)
	if err == nil || !strings.Contains(err.Error(), "load 2") {
		t.Errorf("an error names the load: %v", err)
	}
	// Exactly at the limits is allowed.
	if _, err := parsePlan(load(url.Values{"load1_start": {"12:01:00"}, "load1_duration": {"30"}, "tzoffset": {"0"}}), planNow); err != nil {
		t.Errorf("a load starting in 60 s and lasting 30 s fits: %v", err)
	}
}

func TestPlanRequested(t *testing.T) {
	if planRequested(url.Values{"scenario": {"route"}}) || !planRequested(url.Values{"load2_rate": {"5"}}) {
		t.Fatal("rows mean a plan; a single scenario does not")
	}
}

func TestPlanRunsFromTheForm(t *testing.T) {
	_, _, ts := newServer(t, testfake.Key)
	code, body := post(t, ts, url.Values{
		"load1_scenarios": {"route", "fault"}, "load1_rate": {"20"}, "load1_duration": {"1"},
		"load2_scenarios": {"malformed"}, "load2_rate": {"10"}, "load2_duration": {"1"}, "load2_start": {""},
		"tzoffset": {"0"},
	})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, want := range []string{"Load plan: 2 loads", "Loads in this plan", "Realistic route", "Sensor fault", "Malformed burst", "Throughput", "route: ", "malformed: "} {
		if !strings.Contains(body, want) {
			t.Errorf("plan report missing %q", want)
		}
	}
	if code, body := post(t, ts, url.Values{"load1_rate": {"5"}}); code != 400 || !strings.Contains(body, "tick at least one scenario") {
		t.Fatalf("%d %s", code, body)
	}
	if code, body := post(t, ts, url.Values{"load1_scenarios": {"nope"}, "load1_rate": {"5"}}); code != 400 || !strings.Contains(body, "nope") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestIndexOffersThePlanBuilderAndMoreRows(t *testing.T) {
	_, _, ts := newServer(t, testfake.Key)
	_, body := get(t, ts.URL+"/")
	for _, want := range []string{"Load generator", `name="load1_scenarios" value="malformed"`, `name="load1_start"`, `name="load1_duration"`, `hx-get="/load-row"`, "Add another load", "leave empty = now"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	// The button lives inside the form, so it inherits the form's hx-disabled-elt ("find button.go"), which finds nothing
	// relative to that button and makes htmx abort the request. It must override it.
	if !strings.Contains(body, `hx-get="/load-row" hx-target="#loads" hx-swap="beforeend" hx-disabled-elt="this"`) {
		t.Error("the add-a-load button must override the inherited hx-disabled-elt")
	}
	code, row := get(t, ts.URL+"/load-row?n=3")
	if code != 200 || !strings.Contains(row, `name="load3_scenarios" value="route"`) || !strings.Contains(row, `name="load3_start"`) || !strings.Contains(row, "Remove this load") {
		t.Fatalf("an extra row is a full, removable load: %d", code)
	}
	if strings.Contains(row, "checked") {
		t.Fatal("an added row starts with nothing ticked; only the first row defaults to route")
	}
	for _, n := range []string{"1", "7", "x", ""} {
		if code, _ := get(t, ts.URL+"/load-row?n="+n); code != 400 {
			t.Errorf("n=%q: %d", n, code)
		}
	}
}
