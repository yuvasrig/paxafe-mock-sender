package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
	"github.com/yuvasrig/paxafe-mock-sender/internal/testfake"
)

func newServer(t *testing.T, serverKey string) (*Server, *testfake.Server, *httptest.Server) {
	t.Helper()
	fake := testfake.New(t)
	s, err := New(api.New(fake.URL), serverKey, 478, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, fake, ts
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(t *testing.T, ts *httptest.Server, form url.Values) (int, string) {
	t.Helper()
	resp, err := http.PostForm(ts.URL+"/run", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestIndexListsScenariosAndNeverShowsServerKey(t *testing.T) {
	_, _, ts := newServer(t, "pxk_test_SERVERSIDESECRET")
	code, body := get(t, ts.URL+"/")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{"Provided samples", "Realistic route", "Temperature excursion", "Sensor fault", "Duplicates and conflicts", "Malformed burst", "Auth failures", "Load burst", "hx-get=\"/wake\"", "htmx.org@1.9.12"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	if strings.Contains(body, "SERVERSIDESECRET") {
		t.Fatal("the server-side key must never reach the browser")
	}
	if !strings.Contains(body, "empty = the sender's own key") {
		t.Fatal("the form should say a server key is configured")
	}
}

func TestWake(t *testing.T) {
	_, _, ts := newServer(t, "")
	code, body := get(t, ts.URL+"/wake")
	if code != 200 || !strings.Contains(body, "API is up") || !strings.Contains(body, `id="status"`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestRunShowsSummaryRowsAndDetail(t *testing.T) {
	_, fake, ts := newServer(t, testfake.Key)
	code, body := post(t, ts, url.Values{"scenario": {"route"}, "points": {"4"}})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, want := range []string{"Realistic route", "All 4 checks passed and 0 missing", "Acknowledged", "Rejected (expected)", "Missing", "hx-get=\"/runs/web-"} {
		if !strings.Contains(body, want) {
			t.Errorf("report missing %q", want)
		}
	}
	for _, k := range fake.KeysSeen() {
		if k != testfake.Key {
			t.Fatalf("the server-side key should be used when none is typed, saw %q", k)
		}
	}

	// Follow the first row's detail link.
	i := strings.Index(body, `hx-get="/runs/`)
	link := body[i+len(`hx-get="`):]
	link = link[:strings.Index(link, `"`)]
	code, detail := get(t, ts.URL+link)
	if code != 200 || !strings.Contains(detail, "Tive request") || !strings.Contains(detail, "API response") || !strings.Contains(detail, "&#34;DeviceId&#34;") {
		t.Fatalf("detail %d: %s", code, detail)
	}
	if code, _ := get(t, ts.URL+"/runs/web-nope/0"); code != 404 {
		t.Fatalf("unknown run: %d", code)
	}
	if code, _ := get(t, ts.URL+strings.TrimSuffix(link, "/0")+"/999"); code != 404 {
		t.Fatalf("unknown row: %d", code)
	}
}

func TestRunWithTypedKeyDoesNotEchoIt(t *testing.T) {
	_, fake, ts := newServer(t, "")
	code, body := post(t, ts, url.Values{"scenario": {"route"}, "points": {"2"}, "key": {testfake.Key}})
	if code != 200 || strings.Contains(body, testfake.Key) {
		t.Fatalf("%d; the typed key must not appear in the page", code)
	}
	if k := fake.KeysSeen(); len(k) == 0 || k[0] != testfake.Key {
		t.Fatal("typed key was not used")
	}
	// A wrong key shows failures, not a crash.
	_, body = post(t, ts, url.Values{"scenario": {"route"}, "points": {"2"}, "key": {"pxk_test_WRONGWRONGWRONGWRONGWRONGWRONGWRONGWRONGWRONG"}})
	if !strings.Contains(body, "checks failed") || !strings.Contains(body, "HTTP 401") {
		t.Fatalf("wrong key should show failed checks: %s", body)
	}
}

func TestRunValidation(t *testing.T) {
	_, _, ts := newServer(t, "")
	if code, body := post(t, ts, url.Values{"scenario": {"route"}}); code != 400 || !strings.Contains(body, "No API key") {
		t.Fatalf("no key: %d %s", code, body)
	}
	if code, body := post(t, ts, url.Values{"scenario": {"nope"}, "key": {"k"}}); code != 400 || !strings.Contains(body, "Pick a scenario") {
		t.Fatalf("bad scenario: %d %s", code, body)
	}
}

func TestLoadIsCapped(t *testing.T) {
	_, fake, ts := newServer(t, testfake.Key)
	start := time.Now()
	code, body := post(t, ts, url.Values{"scenario": {"load"}, "rate": {"100000"}, "duration": {"1"}})
	if code != 200 || time.Since(start) > 10*time.Second {
		t.Fatalf("%d after %v", code, time.Since(start))
	}
	if n := len(fake.KeysSeen()); n > 110 { // 1 s at the 100/s cap
		t.Fatalf("sent %d requests; the web cap is 100/s", n)
	}
	for _, want := range []string{"Throughput", "p50", "p95", "p99", "Error rate"} {
		if !strings.Contains(body, want) {
			t.Errorf("load report missing %q", want)
		}
	}
}

func TestOneRunAtATime(t *testing.T) {
	s, _, ts := newServer(t, testfake.Key)
	if !s.acquire() {
		t.Fatal("first acquire")
	}
	code, body := post(t, ts, url.Values{"scenario": {"route"}})
	if code != 409 || !strings.Contains(body, "still in progress") {
		t.Fatalf("%d %s", code, body)
	}
	s.release()
	if code, _ := post(t, ts, url.Values{"scenario": {"route"}, "points": {"2"}}); code != 200 {
		t.Fatalf("after release: %d", code)
	}
}

func TestOnlyRecentRunsAreKept(t *testing.T) {
	s, _, ts := newServer(t, testfake.Key)
	for i := 0; i < keepRuns+3; i++ {
		post(t, ts, url.Values{"scenario": {"auth"}})
		time.Sleep(2 * time.Millisecond) // run ids are millisecond timestamps
	}
	s.mu.Lock()
	n := len(s.runs)
	s.mu.Unlock()
	if n != keepRuns {
		t.Fatalf("kept %d runs, want %d", n, keepRuns)
	}
}

func TestHealthz(t *testing.T) {
	_, _, ts := newServer(t, "")
	if code, body := get(t, ts.URL+"/healthz"); code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestReportListsTheDevicesAndOffersACSV(t *testing.T) {
	_, _, ts := newServer(t, testfake.Key)
	code, body := post(t, ts, url.Values{"scenario": {"route"}, "points": {"4"}})
	if code != 200 {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{"Devices used (1)", "Device name", "IMEI (DeviceId)", "Payloads page", "/devices.csv"} {
		if !strings.Contains(body, want) {
			t.Errorf("report missing %q", want)
		}
	}
	m := regexp.MustCompile(`href="(/runs/[^"]+/devices\.csv)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no CSV link")
	}
	resp, err := http.Get(ts.URL + m[1])
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || len(lines) != 2 ||
		lines[0] != "load,tenant,device_name,imei,requests,acknowledged" || !strings.HasSuffix(lines[1], ",4,4") {
		t.Fatalf("%d %v", resp.StatusCode, lines)
	}
	if code, _ := get(t, ts.URL+"/runs/web-nope/devices.csv"); code != 404 {
		t.Fatalf("unknown run: %d", code)
	}
	// A plan shows the tenant of each device.
	code, body = post(t, ts, url.Values{"load1_scenarios": {"route"}, "load1_rate": {"10"}, "load1_duration": {"1"}, "load1_label": {"Acme"}, "load1_keys": {testfake.Key}, "tzoffset": {"0"}})
	if code != 200 || !strings.Contains(body, "<th>Tenant</th><th>Device name</th>") || !strings.Contains(body, "Acme (") {
		t.Fatalf("a plan lists each device with its load and tenant: %d", code)
	}
}
