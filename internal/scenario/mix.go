package scenario

import (
	"fmt"
	"strings"
	"sync"
)

// seedStep separates the random seeds of successive passes through a scripted scenario.
const seedStep = 7919

// Mix returns a load scenario whose requests come from several scenarios at once: request i is taken from scenario
// i mod n, so every chosen scenario gets an equal share. A scripted scenario (a fixed list of steps) is replayed
// pass after pass, each pass with a new seed so the payloads differ and a long run is not mostly duplicates; a load
// scenario simply continues its own sequence.
//
// Because requests interleave and run concurrently, the order a scenario expects is lost, so a mixed run checks only
// what is order independent: the HTTP answer, that the payload was acknowledged, and (afterwards) that every
// acknowledged event can be found again. Per-request status and flag expectations are dropped.
func Mix(ids []string) (Scenario, error) {
	if len(ids) == 0 {
		return Scenario{}, fmt.Errorf("pick at least one scenario to mix")
	}
	var scs []Scenario
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		s, ok := Get(id)
		switch {
		case !ok:
			return Scenario{}, fmt.Errorf("unknown scenario %q", id)
		case seen[id]:
			continue
		}
		seen[id] = true
		scs = append(scs, s)
	}
	var titles, names []string
	for _, s := range scs {
		titles, names = append(titles, s.Title), append(names, s.ID)
	}

	var mu sync.Mutex
	passes := map[[2]int][]Step{} // (scenario index, pass) -> that pass's steps
	steps := func(k, pass int, p Params) []Step {
		mu.Lock()
		defer mu.Unlock()
		key := [2]int{k, pass}
		if st, ok := passes[key]; ok {
			return st
		}
		pp := p
		pp.Seed += int64(pass) * seedStep
		st := scs[k].Build(pp)
		passes[key] = st
		return st
	}

	return Scenario{
		ID: "mix", Title: "Mixed load: " + strings.Join(titles, " + "), Load: true,
		Description: "Requests drawn evenly from " + strings.Join(names, ", ") + ".",
		Next: func(p Params, i int) Step {
			k, j := i%len(scs), i/len(scs)
			sc := scs[k]
			var st Step
			if sc.Load {
				st = sc.Next(p, j)
			} else {
				// Walk the scenario's steps; after the last step start a new pass with a new seed.
				first := steps(k, 0, p)
				pass, at := j/len(first), j%len(first)
				all := first
				if pass > 0 {
					all = steps(k, pass, p)
				}
				st = all[at%len(all)]
			}
			st.Name = sc.ID + ": " + st.Name
			e := st.Expect
			st.Expect = Expect{HTTP: e.HTTP, Ack: e.Ack, Rejection: e.Rejection, Note: e.Note}
			return st
		},
	}, nil
}
