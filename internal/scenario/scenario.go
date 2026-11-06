// Package scenario defines the test scenarios the sender can run: the payloads to send and what the
// API is expected to answer for each.
package scenario

import (
	"sort"
	"time"
)

// KeyMode says which API key a step is sent with.
type KeyMode int

const (
	KeyDefault   KeyMode = iota // the key given for the run
	KeyNone                     // no X-API-Key header
	KeyInvalid                  // a well-formed key that does not exist
	KeyMalformed                // not a key at all
)

// Expect is what a step should produce.
type Expect struct {
	HTTP       []int    // allowed HTTP statuses (default 200)
	Status     []string // allowed response "status" values; empty = not checked
	Flags      []string // flag codes the response must contain
	ExactFlags bool     // the response must contain exactly Flags, no others
	Ack        bool     // the API must have saved the payload (response carries an event_id)
	Rejection  bool     // an expected rejection (422 or 400): counted separately in the summary
	Note       string   // shown in the results table
}

// Step is one request.
type Step struct {
	Name   string
	Body   []byte
	Key    KeyMode
	Expect Expect
}

// Params are the knobs a scenario can use.
type Params struct {
	Account  int64         // Tive AccountId put in generated payloads; 0 leaves it out
	Points   int           // length of generated routes
	Rate     float64       // load: requests per second
	Duration time.Duration // load: how long
	Now      time.Time     // generated timestamps end here
	Seed     int64         // makes device identity and noise repeatable
}

// Scenario is a named list of steps, or a generator for load.
type Scenario struct {
	ID          string
	Title       string
	Description string
	Load        bool
	Build       func(Params) []Step    // non-load scenarios
	Next        func(Params, int) Step // load scenarios: the i-th request
}

var registry = map[string]Scenario{}

func register(s Scenario) { registry[s.ID] = s }

// Get returns the scenario with the given id.
func Get(id string) (Scenario, bool) { s, ok := registry[id]; return s, ok }

// All lists scenarios in a stable, sensible order.
func All() []Scenario {
	order := []string{"samples", "route", "excursion", "fault", "duplicates", "malformed", "auth", "load"}
	var out []Scenario
	seen := map[string]bool{}
	for _, id := range order {
		if s, ok := registry[id]; ok {
			out = append(out, s)
			seen[id] = true
		}
	}
	var rest []string
	for id := range registry {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		out = append(out, registry[id])
	}
	return out
}
