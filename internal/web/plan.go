package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/runner"
	"github.com/yuvasrig/paxafe-mock-sender/internal/scenario"
)

// The load generator is a plan of up to maxLoads loads. Each load has its own scenarios (several can be ticked: its
// requests are drawn evenly from them), rate, duration and start time. Loads run on their own schedules, at the same
// time when they overlap, and are reported together.

const (
	maxLoads        = 6                // rows in the form
	maxLoadsInPlan  = 12               // after a row with several keys is expanded to one load per key
	maxStartDelay   = 60 * time.Second // a load may start at most this long from now; with maxLoadDuration this bounds a plan to 90 s
	startTimeLayout = "15:04:05"
)

// loadRowData is one load row of the form.
type loadRowData struct {
	N          int
	Scenarios  []scenario.Scenario
	MaxRate    float64
	MaxSeconds int
	Removable  bool
}

func (s *Server) loadRow(n int) loadRowData {
	return loadRowData{N: n, Scenarios: scenario.All(), MaxRate: maxLoadRate, MaxSeconds: int(maxLoadDuration.Seconds()), Removable: n > 1}
}

// loadRowHandler returns one more empty load row, added to the form by the "Add another load" button.
func (s *Server) loadRowHandler(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil || n < 2 || n > maxLoads {
		s.message(w, http.StatusBadRequest, fmt.Sprintf("A plan has at most %d loads.", maxLoads))
		return
	}
	s.render(w, "loadrow", s.loadRow(n))
}

// planRequested is true when the form carries load rows (the load generator) rather than a single scenario.
func planRequested(form url.Values) bool {
	for k := 1; k <= maxLoads; k++ {
		if form.Has(fmt.Sprintf("load%d_rate", k)) {
			return true
		}
	}
	return false
}

// parsePlan reads the load rows. Anything out of range is an error that names the load, not silently changed, except
// rate and duration, which are clamped like the single-scenario form's.
func parsePlan(form url.Values, now time.Time) ([]runner.Load, error) {
	tzOffset, _ := strconv.Atoi(form.Get("tzoffset")) // minutes behind UTC, as the browser reports it
	var loads []runner.Load
	for k := 1; k <= maxLoads; k++ {
		field := func(name string) string { return strings.TrimSpace(form.Get(fmt.Sprintf("load%d_%s", k, name))) }
		if !form.Has(fmt.Sprintf("load%d_rate", k)) {
			continue // a row that was removed
		}
		ids := form[fmt.Sprintf("load%d_scenarios", k)]
		if len(ids) == 0 {
			return nil, fmt.Errorf("load %d: tick at least one scenario", k)
		}
		l := runner.Load{Scenarios: ids, Label: field("label")}
		if v, err := strconv.ParseFloat(field("rate"), 64); err == nil {
			l.Rate = clampFloat(v, 1, maxLoadRate)
		} else {
			l.Rate = 10
		}
		secs := 10.0
		if v, err := strconv.ParseFloat(field("duration"), 64); err == nil {
			secs = clampFloat(v, 1, maxLoadDuration.Seconds())
		}
		l.Duration = time.Duration(secs * float64(time.Second))
		if st := field("start"); st != "" {
			at, err := startAt(st, now, tzOffset)
			if err != nil {
				return nil, fmt.Errorf("load %d: %w", k, err)
			}
			l.Start = at
		}
		// The keys say which tenants this row sends to; several keys make one load per tenant, all started together.
		// They are used as typed: a value is never looked up (an env: reference works only on the CLI, where the
		// person already owns the environment).
		keys := strings.FieldsFunc(form.Get(fmt.Sprintf("load%d_keys", k)), func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
		if len(keys) == 0 {
			loads = append(loads, l) // the run's key
			continue
		}
		for _, key := range keys {
			lk := l
			lk.Key = key
			loads = append(loads, lk)
		}
	}
	if len(loads) > maxLoadsInPlan {
		return nil, fmt.Errorf("a plan may have at most %d loads once every key is counted (this one has %d)", maxLoadsInPlan, len(loads))
	}
	if len(loads) == 0 {
		return nil, fmt.Errorf("add at least one load")
	}
	for k, l := range loads {
		start := l.Start
		if start.IsZero() {
			start = now
		}
		if start.Sub(now) > maxStartDelay {
			return nil, fmt.Errorf("load %d: start within the next %d seconds", k+1, int(maxStartDelay.Seconds()))
		}
	}
	return loads, nil
}

// startAt turns the time of day typed in the browser (its own clock) into an instant. A time that has just passed
// means now; one that is further back is an error rather than tomorrow.
func startAt(text string, now time.Time, tzOffsetMinutes int) (time.Time, error) {
	var tod time.Time
	var err error
	for _, layout := range []string{startTimeLayout, "15:04"} {
		if tod, err = time.Parse(layout, text); err == nil {
			break
		}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("start time %q is not a time of day like 14:30:00", text)
	}
	local := now.Add(-time.Duration(tzOffsetMinutes) * time.Minute) // the browser's wall clock
	day := time.Date(local.Year(), local.Month(), local.Day(), tod.Hour(), tod.Minute(), tod.Second(), 0, time.UTC)
	at := day.Add(time.Duration(tzOffsetMinutes) * time.Minute) // back to UTC
	switch {
	case at.Before(now.Add(-time.Minute)):
		return time.Time{}, fmt.Errorf("start time %s has already passed", text)
	case at.Before(now):
		return time.Time{}, nil // just now: start now
	}
	return at, nil
}

// runPlan runs the load generator form and renders the report.
func (s *Server) runPlan(w http.ResponseWriter, r *http.Request) {
	loads, err := parsePlan(r.Form, time.Now())
	if err != nil {
		s.message(w, http.StatusBadRequest, "Load generator: "+err.Error()+".")
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		key = s.DefaultKey
	}
	for i, l := range loads {
		if l.Key == "" && key == "" {
			s.message(w, http.StatusBadRequest, fmt.Sprintf("Load generator: load %d has no API key: enter one for it, enter a default key, or configure SENDER_API_KEY on the sender.", i+1))
			return
		}
	}
	if !s.acquire() {
		s.message(w, http.StatusConflict, "Another run is still in progress. Wait for it to finish.")
		return
	}
	defer s.release()

	ctx, cancel := context.WithTimeout(r.Context(), planTimeout)
	defer cancel()
	runID := "web-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	cfg := runner.Config{
		Client: s.Client, Key: key, RunID: runID,
		Params: scenario.Params{Account: s.Account, Points: 12, Now: time.Now().UTC().Truncate(time.Second), Seed: time.Now().UnixNano()},
	}
	rep, err := runner.RunPlan(ctx, cfg, loads)
	if err != nil {
		s.message(w, http.StatusBadRequest, "Load generator: "+err.Error()+".")
		return
	}
	s.store(rep)
	s.Log.Info("plan finished", "run_id", runID, "loads", len(loads), "sent", rep.Summary.Sent, "failed", rep.Summary.Failed, "missing", rep.Summary.Missing)
	rows := rep.Results
	truncated := false
	if len(rows) > maxRows {
		rows, truncated = rows[:maxRows], true
	}
	cmp := compared(rep.Results)
	if len(cmp) > maxCompareRows {
		cmp = cmp[:maxCompareRows]
	}
	s.render(w, "report.html", map[string]any{"R": rep, "Rows": rows, "Truncated": truncated, "Load": true, "Cmp": cmp})
}
