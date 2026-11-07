package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yuvasrig/paxafe-mock-sender/internal/runner"
	"github.com/yuvasrig/paxafe-mock-sender/internal/testfake"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := realMain(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestList(t *testing.T) {
	code, out, _ := run(t, "list")
	if code != 0 || !strings.Contains(out, "samples") || !strings.Contains(out, "load") || strings.Count(out, "\n") != 8 {
		t.Fatalf("%d %q", code, out)
	}
}

func TestHelpAndUnknown(t *testing.T) {
	if code, out, _ := run(t, "help"); code != 0 || !strings.Contains(out, "sender run") {
		t.Fatalf("help: %d", code)
	}
	if code, _, errOut := run(t, "bogus"); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown: %d %q", code, errOut)
	}
}

func TestRunUsageErrors(t *testing.T) {
	t.Setenv("API_BASE_URL", "")
	t.Setenv("SENDER_API_KEY", "")
	for name, args := range map[string][]string{
		"no scenario":  {"run"},
		"bad scenario": {"run", "--scenario", "nope", "--url", "http://x", "--key", "k"},
		"no url":       {"run", "--scenario", "route", "--key", "k"},
		"no key":       {"run", "--scenario", "route", "--url", "http://x"},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestRunPassesAndPrintsSummary(t *testing.T) {
	fake := testfake.New(t)
	code, out, errOut := run(t, "run", "--scenario", "route", "--url", fake.URL, "--key", testfake.Key, "--points", "3", "--account", "0")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"Realistic route", "3 sent · 3 acknowledged · 3 processed · 0 rejected (expected) · 0 missing", "checks: 3 passed, 0 failed", "RESULT: ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "up in") {
		t.Errorf("should report waking the API: %q", errOut)
	}
}

// A failing check must fail the command, so it can gate a deploy.
func TestRunExitsNonZeroOnFailure(t *testing.T) {
	fake := testfake.New(t)
	code, out, _ := run(t, "run", "--scenario", "samples", "--url", fake.URL, "--key", testfake.Key)
	if code != 1 || !strings.Contains(out, "RESULT: FAILED") || !strings.Contains(out, "FAIL") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	code, _, _ = run(t, "run", "--scenario", "route", "--url", fake.URL, "--key", "pxk_test_WRONGWRONGWRONGWRONGWRONGWRONGWRONGWRONGWRONG", "--points", "2")
	if code != 1 {
		t.Fatalf("wrong key exit %d", code)
	}
}

func TestRunAuthScenarioNeedsNoKey(t *testing.T) {
	t.Setenv("SENDER_API_KEY", "")
	fake := testfake.New(t)
	if code, out, _ := run(t, "run", "--scenario", "auth", "--url", fake.URL); code != 0 || !strings.Contains(out, "RESULT: ok") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestRunJSONAndLoad(t *testing.T) {
	fake := testfake.New(t)
	code, out, _ := run(t, "run", "--scenario", "load", "--url", fake.URL, "--key", testfake.Key, "--rate", "50", "--duration", "1s", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var rep runner.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json must print only the report: %v", err)
	}
	if rep.Summary.Sent != 50 || rep.Summary.P99Ms <= 0 || rep.ScenarioID != "load" {
		t.Fatalf("report %+v", rep.Summary)
	}
	code, text, _ := run(t, "run", "--scenario", "load", "--url", fake.URL, "--key", testfake.Key, "--rate", "20", "--duration", "1s")
	if code != 0 || !strings.Contains(text, "throughput") || !strings.Contains(text, "p99") {
		t.Fatalf("load text output:\n%s", text)
	}
}

func TestRunMixesSeveralScenarios(t *testing.T) {
	fake := testfake.New(t)
	var out, errb bytes.Buffer
	code := realMain([]string{"run", "--scenario", "route,fault", "--url", fake.URL, "--key", testfake.Key, "--rate", "10", "--duration", "1s"}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "Mixed load: Realistic route + Sensor fault") {
		t.Fatalf("%d\n%s\n%s", code, out.String(), errb.String())
	}
	code = realMain([]string{"run", "--scenario", "route,nope", "--url", fake.URL, "--key", testfake.Key}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "nope") {
		t.Fatalf("%d %s", code, errb.String())
	}
}

func TestRunPlanFromRepeatedLoadFlags(t *testing.T) {
	fake := testfake.New(t)
	var out, errb bytes.Buffer
	code := realMain([]string{"run", "--url", fake.URL, "--key", testfake.Key,
		"--load", "scenarios=route,fault rate=10 duration=1s",
		"--load", "scenarios=duplicates,route rate=10 duration=1s start=+1s"}, &out, &errb)
	text := out.String()
	if code != 0 || !strings.Contains(text, "Load plan: 2 loads") || !strings.Contains(text, "loads:") || strings.Count(text, " sent, ") < 2 {
		t.Fatalf("%d\n%s\n%s", code, text, errb.String())
	}
	if !strings.Contains(text, "started +0.0s") && !strings.Contains(text, "started +0.") {
		t.Fatalf("the first load starts now: %s", text)
	}
	if !strings.Contains(text, "started +1.") {
		t.Fatalf("the second load starts one second later: %s", text)
	}
	for name, args := range map[string][]string{
		"bad spec":   {"run", "--url", fake.URL, "--key", testfake.Key, "--load", "rate=5"},
		"unknown":    {"run", "--url", fake.URL, "--key", testfake.Key, "--load", "scenarios=nope"},
		"both flags": {"run", "--url", fake.URL, "--key", testfake.Key, "--scenario", "route", "--load", "scenarios=route"},
		"no key":     {"run", "--url", fake.URL, "--load", "scenarios=route"},
	} {
		var o, e bytes.Buffer
		if code := realMain(args, &o, &e); code != 2 {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, e.String())
		}
	}
}

func TestRunPlanOnSeveralTenantsWithTheirOwnKeys(t *testing.T) {
	fake := testfake.New(t)
	t.Setenv("SENDER_TEST_KEY", testfake.Key)
	var out, errb bytes.Buffer
	// No --key at all: every load brings its own, one of them from the environment.
	code := realMain([]string{"run", "--url", fake.URL,
		"--load", "scenarios=route rate=10 duration=1s key=env:SENDER_TEST_KEY label=Acme",
		"--load", "scenarios=route rate=10 duration=1s keys=" + testfake.Key + " label=BioCo"}, &out, &errb)
	text := out.String()
	if code != 0 || !strings.Contains(text, "→ Acme (") || !strings.Contains(text, "→ BioCo (") || strings.Contains(text, testfake.Key) {
		t.Fatalf("%d\n%s\n%s", code, text, errb.String())
	}
	var o2, e2 bytes.Buffer
	if code := realMain([]string{"run", "--url", fake.URL, "--load", "scenarios=route"}, &o2, &e2); code != 2 || !strings.Contains(e2.String(), "--key") {
		t.Fatalf("a load with no key and no --key is refused: %d %s", code, e2.String())
	}
	var o3, e3 bytes.Buffer
	if code := realMain([]string{"run", "--url", fake.URL, "--load", "scenarios=route key=env:NOT_SET_ANYWHERE_XYZ"}, &o3, &e3); code != 2 || !strings.Contains(e3.String(), "NOT_SET_ANYWHERE_XYZ") {
		t.Fatalf("%d %s", code, e3.String())
	}
}

func TestCLIListsTheDevicesUsed(t *testing.T) {
	fake := testfake.New(t)
	var out, errb bytes.Buffer
	code := realMain([]string{"run", "--scenario", "route", "--points", "5", "--url", fake.URL, "--key", testfake.Key}, &out, &errb)
	text := out.String()
	if code != 0 || !strings.Contains(text, "devices used (1):") || !strings.Contains(text, "5 requests") || !strings.Contains(text, "SIM") {
		t.Fatalf("%d\n%s", code, text)
	}
	out.Reset()
	realMain([]string{"run", "--url", fake.URL, "--load", "scenarios=load rate=60 duration=1s label=Acme key=" + testfake.Key}, &out, &errb)
	text = out.String()
	if !strings.Contains(text, "devices used (50):") || !strings.Contains(text, "… and 38 more") || !strings.Contains(text, "load 1 → Acme") {
		t.Fatalf("a long list is cut with a pointer to --json:\n%s", text)
	}
}
