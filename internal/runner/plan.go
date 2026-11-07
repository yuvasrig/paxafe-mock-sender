package runner

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/scenario"
)

// Load is one load in a plan: the scenarios its requests are drawn from, how fast, for how long, and when it starts.
type Load struct {
	// Key is the API key this load sends with, so each load can target a different tenant. Empty uses the run's key.
	// It is used for this run only and never shown. Label names the load in the report (default: the key's prefix).
	Key       string
	Label     string
	Scenarios []string      // one or more scenario ids; requests are drawn evenly from them
	Rate      float64       // requests per second
	Duration  time.Duration // how long it runs
	Start     time.Time     // when it starts; the zero time means now
}

// LoadSummary is what one load of a plan did.
type LoadSummary struct {
	Index            int       `json:"index"`  // 1-based
	Target           string    `json:"target"` // the label or key prefix of the tenant this load sent to
	Title            string    `json:"title"`
	Rate             float64   `json:"rate"`
	Duration         float64   `json:"duration_seconds"`
	StartedAt        time.Time `json:"started_at"`
	Sent             int       `json:"sent"`
	Failed           int       `json:"failed"`
	ThroughputPerSec float64   `json:"throughput_per_sec"`
	P95Ms            float64   `json:"p95_ms"`
}

// RunPlan runs several loads, each on its own schedule, and reports them together: one table of requests, one
// summary and one completeness check, plus a line per load. Loads that overlap in time run at the same time.
func RunPlan(ctx context.Context, cfg Config, loads []Load) (*Report, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("runner: no API client")
	}
	if len(loads) == 0 {
		return nil, fmt.Errorf("runner: a plan needs at least one load")
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

	// Check every load before starting any, so a typo in the last one does not waste the others.
	scs := make([]scenario.Scenario, len(loads))
	for i, l := range loads {
		sc, err := scenario.Mix(l.Scenarios)
		if err != nil {
			return nil, fmt.Errorf("load %d: %w", i+1, err)
		}
		if l.Rate <= 0 || l.Duration <= 0 {
			return nil, fmt.Errorf("load %d: rate and duration must be above zero", i+1)
		}
		if l.Key == "" && cfg.Key == "" {
			return nil, fmt.Errorf("load %d: no API key (give the load a key, or the run one)", i+1)
		}
		scs[i] = sc
	}

	started := time.Now()
	results := make([][]Result, len(loads))
	summaries := make([]LoadSummary, len(loads))
	var wg sync.WaitGroup
	for i := range loads {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := loads[i]
			start := l.Start
			if start.IsZero() {
				start = started
			}
			if wait := time.Until(start); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
			}
			lc := cfg
			lc.Params.Rate, lc.Params.Duration = l.Rate, l.Duration
			lc.Params.Seed = cfg.Params.Seed + int64(i)*104729 // each load has its own simulated trackers
			lc.RunID = fmt.Sprintf("%s-L%d", cfg.RunID, i+1)
			if l.Key != "" {
				lc.Key = l.Key
			}
			ran := time.Now()
			res := runLoad(ctx, lc, scs[i])
			for j := range res {
				res[j].Load = i + 1
				res[j].Key = lc.Key
			}
			results[i] = res
			summaries[i] = loadSummary(i+1, scs[i].Title, l, lc.Key, ran, time.Since(ran), res)
		}(i)
	}
	wg.Wait()

	// One table: the loads' requests in the order they started, numbered from 1.
	var all []Result
	for _, r := range results {
		all = append(all, r...)
	}
	sort.SliceStable(all, func(a, b int) bool {
		return summaries[all[a].Load-1].StartedAt.Before(summaries[all[b].Load-1].StartedAt)
	})
	for i := range all {
		all[i].Index = i
	}

	rep := &Report{RunID: cfg.RunID, ScenarioID: "plan", Title: planTitle(loads), Started: started, Results: all}
	rep.Elapsed = time.Since(started)
	var kept []LoadSummary
	for _, s := range summaries {
		if s.Index != 0 { // a load cancelled before it started has no summary
			kept = append(kept, s)
		}
	}
	rep.Loads = kept

	verified := verify(ctx, cfg, rep.Results)
	rep.Summary = summarize(rep.Results, rep.Elapsed)
	rep.Summary.Verified = verified - rep.Summary.LookupErrors
	if rep.Summary.LookupErrors > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d lookups failed, so those events are unverified", rep.Summary.LookupErrors))
	}
	if verified < rep.Summary.Acknowledged {
		rep.Notes = append(rep.Notes, fmt.Sprintf("completeness checked on %d of %d acknowledged events (an even sample)", verified, rep.Summary.Acknowledged))
	}
	return rep, nil
}

func planTitle(loads []Load) string {
	if len(loads) == 1 {
		return "Load: " + strings.Join(loads[0].Scenarios, " + ")
	}
	return fmt.Sprintf("Load plan: %d loads", len(loads))
}

func loadSummary(index int, title string, l Load, key string, at time.Time, took time.Duration, res []Result) LoadSummary {
	s := LoadSummary{Index: index, Target: l.Target(key), Title: title, Rate: l.Rate, Duration: l.Duration.Seconds(), StartedAt: at, Sent: len(res)}
	var lat []float64
	for _, r := range res {
		if !r.Pass {
			s.Failed++
		}
		if r.HTTP > 0 {
			lat = append(lat, r.LatencyMs)
		}
	}
	sort.Float64s(lat)
	s.P95Ms = Percentile(lat, 95)
	if took > 0 {
		s.ThroughputPerSec = float64(len(res)) / took.Seconds()
	}
	return s
}

// ParseLoads reads one load written as words, for the CLI: "scenarios=route,fault rate=20 duration=30s start=+10s".
// scenarios is required; rate defaults to 10 a second, duration to 10s, and start to now. A start is "now", an offset
// from now ("+30s"), a time of day on this machine's clock ("14:30" or "14:30:15", today) or an RFC 3339 time.
//
// key (or keys, comma separated) says which tenant the load sends to, by API key. A key may be written env:NAME to read
// it from an environment variable, so a secret need not appear in the command line. Several keys give one load per key,
// all with the same scenarios, rate, duration and start, which is how one spec triggers the same load on several tenants
// at once. With no key the load uses the run's key. label names the load in the report.
func ParseLoads(spec string, now time.Time) ([]Load, error) {
	l := Load{Rate: 10, Duration: 10 * time.Second}
	var keys []string
	seen := map[string]bool{}
	for _, field := range strings.Fields(spec) {
		k, v, ok := strings.Cut(field, "=")
		if !ok || v == "" {
			return nil, fmt.Errorf("%q: write it as key=value (scenarios, rate, duration, start, key, label)", field)
		}
		if k == "key" {
			k = "keys"
		}
		if seen[k] {
			return nil, fmt.Errorf("%s given twice", k)
		}
		seen[k] = true
		switch k {
		case "scenarios", "scenario":
			for _, id := range strings.Split(v, ",") {
				if id = strings.TrimSpace(id); id != "" {
					l.Scenarios = append(l.Scenarios, id)
				}
			}
		case "rate":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 {
				return nil, fmt.Errorf("rate %q must be a number above zero", v)
			}
			l.Rate = f
		case "duration":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("duration %q must be like 30s or 2m", v)
			}
			l.Duration = d
		case "start":
			at, err := parseStart(v, now)
			if err != nil {
				return nil, err
			}
			l.Start = at
		case "label":
			l.Label = v
		case "keys":
			for _, ref := range strings.Split(v, ",") {
				key, err := resolveKey(strings.TrimSpace(ref))
				if err != nil {
					return nil, err
				}
				keys = append(keys, key)
			}
		default:
			return nil, fmt.Errorf("unknown key %q (use scenarios, rate, duration, start, key, label)", k)
		}
	}
	if len(l.Scenarios) == 0 {
		return nil, fmt.Errorf("scenarios is required, for example scenarios=route,fault")
	}
	if len(keys) == 0 {
		return []Load{l}, nil
	}
	out := make([]Load, 0, len(keys))
	for _, key := range keys {
		lk := l
		lk.Key = key
		out = append(out, lk)
	}
	return out, nil
}

// resolveKey returns an API key, reading it from the environment when written env:NAME. The key itself is never put in
// an error message.
func resolveKey(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("an empty key")
	}
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return "", fmt.Errorf("the environment variable %s is not set", name)
		}
		return v, nil
	}
	return ref, nil
}

func parseStart(v string, now time.Time) (time.Time, error) {
	switch {
	case v == "now":
		return time.Time{}, nil
	case strings.HasPrefix(v, "+"):
		d, err := time.ParseDuration(v[1:])
		if err != nil || d < 0 {
			return time.Time{}, fmt.Errorf("start %q: an offset looks like +30s", v)
		}
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range []string{"15:04:05", "15:04"} {
		if tod, err := time.Parse(layout, v); err == nil {
			local := now.Local()
			at := time.Date(local.Year(), local.Month(), local.Day(), tod.Hour(), tod.Minute(), tod.Second(), 0, time.Local)
			if at.Before(now.Add(-time.Second)) {
				return time.Time{}, fmt.Errorf("start %s has already passed today", v)
			}
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("start %q: use now, +30s, a time of day like 14:30:00, or an RFC 3339 time", v)
}

// KeyPrefix is the start of an API key that is safe to show: the type and the first characters, as the API itself lists
// keys. The rest of the key is never shown.
func KeyPrefix(key string) string {
	const shown = 13 // "pxk_test_" and four more characters
	if len(key) <= shown {
		return "key " + strings.Repeat("*", 4)
	}
	return key[:shown] + "…"
}

// Target names what a load sends to in the report: its label, with the key's prefix, or just the prefix.
func (l Load) Target(key string) string {
	switch {
	case l.Label != "" && key != "":
		return l.Label + " (" + KeyPrefix(key) + ")"
	case l.Label != "":
		return l.Label
	case key != "":
		return KeyPrefix(key)
	}
	return "run's key"
}
