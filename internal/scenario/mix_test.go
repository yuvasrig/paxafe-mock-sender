package scenario

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func mixParams() Params {
	return Params{Account: 478, Points: 6, Now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), Seed: 11}
}

func TestMixNeedsKnownScenarios(t *testing.T) {
	if _, err := Mix(nil); err == nil {
		t.Fatal("nothing to mix")
	}
	if _, err := Mix([]string{"route", "nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("%v", err)
	}
	sc, err := Mix([]string{"route", " fault ", "route"}) // spaces tolerated, repeats ignored
	if err != nil || !sc.Load || sc.Next == nil || !strings.Contains(sc.Title, "Realistic route") || !strings.Contains(sc.Title, "Sensor fault") {
		t.Fatalf("%+v %v", sc, err)
	}
	if strings.Count(sc.Title, "Realistic route") != 1 {
		t.Fatalf("a repeated id counts once: %s", sc.Title)
	}
}

func TestMixDrawsEvenlyFromEveryScenario(t *testing.T) {
	sc, _ := Mix([]string{"route", "fault", "malformed"})
	p := mixParams()
	count := map[string]int{}
	for i := 0; i < 90; i++ {
		count[strings.SplitN(sc.Next(p, i).Name, ":", 2)[0]]++
	}
	if count["route"] != 30 || count["fault"] != 30 || count["malformed"] != 30 {
		t.Fatalf("an equal share each: %v", count)
	}
}

func TestMixKeepsTheHTTPExpectationButDropsOrderDependentOnes(t *testing.T) {
	sc, _ := Mix([]string{"route", "malformed", "auth"})
	p := mixParams()
	var sawReject, sawAuth bool
	for i := 0; i < 60; i++ {
		st := sc.Next(p, i)
		if len(st.Expect.Status) != 0 || len(st.Expect.Flags) != 0 || st.Expect.ExactFlags {
			t.Fatalf("status and flag checks depend on order: %+v", st.Expect)
		}
		if st.Expect.Rejection {
			sawReject = true
		}
		if strings.HasPrefix(st.Name, "auth:") && len(st.Expect.HTTP) > 0 && st.Expect.HTTP[0] == 401 {
			sawAuth = true
		}
	}
	if !sawReject || !sawAuth {
		t.Fatalf("expected rejections and 401s are kept: %v %v", sawReject, sawAuth)
	}
}

// A scripted scenario that is replayed pass after pass must not resend the same payloads.
func TestMixRegeneratesScriptedScenariosOnEachPass(t *testing.T) {
	sc, _ := Mix([]string{"route"})
	p := mixParams()
	seen := map[string]bool{}
	for i := 0; i < 5*p.Points; i++ { // five passes through a 6-step route
		b := string(sc.Next(p, i).Body)
		if seen[b] {
			t.Fatalf("request %d repeats an earlier payload", i)
		}
		seen[b] = true
	}
}

func TestMixIsSafeToCallConcurrentlyAndRepeatable(t *testing.T) {
	sc, _ := Mix([]string{"route", "fault", "load"})
	p := mixParams()
	want := make([][]byte, 120)
	for i := range want {
		want[i] = sc.Next(p, i).Body
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range want {
				if !bytes.Equal(sc.Next(p, i).Body, want[i]) {
					t.Errorf("request %d differs between calls", i)
					return
				}
			}
		}()
	}
	wg.Wait()
}
