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
