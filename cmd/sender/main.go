// Command sender is the mock Tive sender: a web UI (serve) and a CLI (run, list) that send test payloads to the
// PAXAFE Integration API and verify that every acknowledged payload can be found again.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
	"github.com/yuvasrig/paxafe-mock-sender/internal/runner"
	"github.com/yuvasrig/paxafe-mock-sender/internal/scenario"
	"github.com/yuvasrig/paxafe-mock-sender/internal/web"
)

const usage = `usage:
  sender serve                                   web UI (PORT, API_BASE_URL, SENDER_API_KEY, SENDER_ACCOUNT_ID)
  sender list                                    list scenarios
  sender run --scenario ID [flags]               run a scenario and print the results
  sender run --scenario ID,ID,... [flags]        generate load from several scenarios at once (uses --rate and --duration)
  sender run --load "scenarios=a,b rate=20 duration=30s start=+10s key=env:ACME_KEY" --load "..."
                                                 a plan: each load has its own scenarios, rate, duration, start and tenant (key=, several keys = one load each)
      --url URL        API base URL           (default $API_BASE_URL)
      --key KEY        API key                (default $SENDER_API_KEY)
      --rate N         load: requests per second (default 10)
      --duration D     load: how long, e.g. 30s  (default 10s)
      --points N       route length             (default 12)
      --account N      Tive AccountId in payloads, 0 = omit (default $SENDER_ACCOUNT_ID or 478)
      --verify N       most events to look up afterwards (default 300)
      --json           print the full report as JSON
Exit status is 1 when any check fails or any acknowledged payload is missing.
`

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	cmd, rest := "serve", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, rest = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(stdout, stderr)
	case "list":
		for _, s := range scenario.All() {
			fmt.Fprintf(stdout, "%-11s %s\n", s.ID, s.Title)
		}
		return 0
	case "run":
		return runCmd(rest, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "unknown command %q\n%s", cmd, usage)
	return 2
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func accountFromEnv() int64 {
	if v, err := strconv.ParseInt(os.Getenv("SENDER_ACCOUNT_ID"), 10, 64); err == nil {
		return v
	}
	return 478
}

func serve(stdout, stderr io.Writer) int {
	base := os.Getenv("API_BASE_URL")
	if base == "" {
		fmt.Fprintln(stderr, "API_BASE_URL is required")
		return 1
	}
	log := slog.New(slog.NewJSONHandler(stdout, nil))
	srv, err := web.New(api.New(base), os.Getenv("SENDER_API_KEY"), accountFromEnv(), log)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	httpSrv := &http.Server{
		Addr: ":" + envOr("PORT", "8080"), Handler: srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 130 * time.Second, IdleTimeout: 60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpSrv.Addr, "api", base, "server_key", os.Getenv("SENDER_API_KEY") != "")
		errCh <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		fmt.Fprintln(stderr, "server:", err)
		return 1
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shut); err != nil {
			fmt.Fprintln(stderr, "shutdown:", err)
			return 1
		}
		return 0
	}
}

func runCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("scenario", "", "scenario id, or several separated by commas for a mixed load (see `sender list`)")
	url := fs.String("url", os.Getenv("API_BASE_URL"), "API base URL")
	key := fs.String("key", os.Getenv("SENDER_API_KEY"), "API key")
	rate := fs.Float64("rate", 10, "load: requests per second")
	dur := fs.Duration("duration", 10*time.Second, "load: duration")
	points := fs.Int("points", 12, "route length")
	account := fs.Int64("account", accountFromEnv(), "Tive AccountId in payloads (0 = omit)")
	verify := fs.Int("verify", 300, "most events to look up afterwards")
	wait := fs.Duration("wait", time.Minute, "async tenants: how long to wait for workers to finish each queued event")
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	var loadSpecs stringList
	fs.Var(&loadSpecs, "load", "a load of a plan, repeatable: \"scenarios=route,fault rate=20 duration=30s start=+10s\" (start: now, +30s, 14:30:00 or an RFC 3339 time; default now); key=KEY or key=env:NAME picks the tenant, key=A,B runs it on both at once; label= names it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(loadSpecs) > 0 {
		return runPlanCmd(loadSpecs, *id, *url, *key, *account, *verify, *wait, *asJSON, stdout, stderr)
	}
	var sc scenario.Scenario
	var ok bool
	if strings.Contains(*id, ",") { // several scenarios: a mixed load, driven by --rate and --duration
		var err error
		if sc, err = scenario.Mix(strings.Split(*id, ",")); err != nil {
			fmt.Fprintf(stderr, "--scenario: %v; run `sender list`\n", err)
			return 2
		}
		ok = true
	} else {
		sc, ok = scenario.Get(*id)
	}
	switch {
	case !ok:
		fmt.Fprintf(stderr, "unknown or missing --scenario %q; run `sender list`\n", *id)
		return 2
	case *url == "":
		fmt.Fprintln(stderr, "--url (or API_BASE_URL) is required")
		return 2
	case *key == "" && sc.ID != "auth":
		fmt.Fprintln(stderr, "--key (or SENDER_API_KEY) is required")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := api.New(*url)
	fmt.Fprintf(stderr, "waking %s … ", *url)
	wakeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	took, err := client.Wake(wakeCtx)
	cancel()
	if err != nil {
		fmt.Fprintln(stderr, "failed:", err)
		return 1
	}
	fmt.Fprintf(stderr, "up in %.1fs\n", took.Seconds())

	rep, err := runner.Run(ctx, runner.Config{
		Client: client, Key: *key, RunID: "cli-" + strconv.FormatInt(time.Now().UnixMilli(), 36), VerifyCap: *verify, AsyncWait: *wait,
		Params: scenario.Params{Account: *account, Points: *points, Rate: *rate, Duration: *dur, Now: time.Now().UTC().Truncate(time.Second), Seed: time.Now().UnixNano()},
	}, sc)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printReport(stdout, rep, sc.Load)
	}
	if !rep.Summary.OK() {
		return 1
	}
	return 0
}

// stringList is a flag that can be given several times.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, "; ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// runPlanCmd runs a plan of loads given with --load, each with its own scenarios, rate, duration and start.
func runPlanCmd(specs []string, scenarioFlag, url, key string, account int64, verify int, wait time.Duration, asJSON bool, stdout, stderr io.Writer) int {
	now := time.Now()
	var loads []runner.Load
	for i, spec := range specs {
		ls, err := runner.ParseLoads(spec, now)
		if err != nil {
			fmt.Fprintf(stderr, "--load %d: %v\n", i+1, err)
			return 2
		}
		loads = append(loads, ls...)
	}
	everyLoadHasAKey := true
	for _, l := range loads {
		everyLoadHasAKey = everyLoadHasAKey && l.Key != ""
	}
	switch {
	case scenarioFlag != "":
		fmt.Fprintln(stderr, "use either --scenario or --load, not both")
		return 2
	case url == "":
		fmt.Fprintln(stderr, "--url (or API_BASE_URL) is required")
		return 2
	case key == "" && !everyLoadHasAKey:
		fmt.Fprintln(stderr, "--key (or SENDER_API_KEY) is required, unless every load gives its own key=")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := api.New(url)
	fmt.Fprintf(stderr, "waking %s … ", url)
	wakeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	took, err := client.Wake(wakeCtx)
	cancel()
	if err != nil {
		fmt.Fprintln(stderr, "failed:", err)
		return 1
	}
	fmt.Fprintf(stderr, "up in %.1fs\n", took.Seconds())

	rep, err := runner.RunPlan(ctx, runner.Config{
		Client: client, Key: key, RunID: "cli-" + strconv.FormatInt(time.Now().UnixMilli(), 36), VerifyCap: verify, AsyncWait: wait,
		Params: scenario.Params{Account: account, Points: 12, Now: time.Now().UTC().Truncate(time.Second), Seed: time.Now().UnixNano()},
	}, loads)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printReport(stdout, rep, true)
	}
	if !rep.Summary.OK() {
		return 1
	}
	return 0
}

func printReport(w io.Writer, rep *runner.Report, load bool) {
	fmt.Fprintf(w, "\n%s (run %s, %.2fs)\n\n", rep.Title, rep.RunID, rep.Elapsed.Seconds())
	const maxRows = 60
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tREQUEST\tHTTP\tSTATUS\tFLAGS\tMS\tCHECK")
	shown := 0
	for _, r := range rep.Results {
		if load && r.Pass {
			continue // a load run lists only the failures
		}
		if shown++; shown > maxRows {
			break
		}
		check := "ok"
		if !r.Pass {
			check = "FAIL " + r.Reason
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%.1f\t%s\n", r.Index+1, r.Name, r.HTTP, r.Status, strings.Join(r.Flags, ","), r.LatencyMs, check)
	}
	_ = tw.Flush()

	if len(rep.Loads) > 0 {
		fmt.Fprintln(w, "\nloads:")
		for _, l := range rep.Loads {
			fmt.Fprintf(w, "  %d  → %s · %s · %.0f/s for %.0fs · started +%.1fs · %d sent, %d failed · %.1f/s · p95 %.1f ms\n",
				l.Index, l.Target, l.Title, l.Rate, l.Duration, l.StartedAt.Sub(rep.Started).Seconds(), l.Sent, l.Failed, l.ThroughputPerSec, l.P95Ms)
		}
	}

	if len(rep.Devices) > 0 {
		const showDevices = 12
		fmt.Fprintf(w, "\ndevices used (%d):\n", len(rep.Devices))
		for i, d := range rep.Devices {
			if i == showDevices {
				fmt.Fprintf(w, "  … and %d more (--json lists them all)\n", len(rep.Devices)-showDevices)
				break
			}
			imei := d.IMEI
			if imei == "" {
				imei = "(no DeviceId)"
			}
			where := ""
			if d.Load > 0 {
				where = fmt.Sprintf("  load %d → %s", d.Load, d.Target)
			}
			fmt.Fprintf(w, "  %-12s %-16s %5d requests%s\n", d.Name, imei, d.Requests, where)
		}
	}

	s := rep.Summary
	fmt.Fprintf(w, "\n%d sent · %d acknowledged · %d processed · %d rejected (expected) · %d missing", s.Sent, s.Acknowledged, s.Processed, s.ExpectedRejections, s.Missing)
	if s.Queued > 0 {
		fmt.Fprintf(w, " · %d queued (async tenant: followed until a worker finished each)", s.Queued)
	}
	if s.Refused > 0 {
		fmt.Fprintf(w, " · %d refused (429/503/401/413: not taken, not lost)", s.Refused)
	}
	if s.NotProcessed > 0 {
		fmt.Fprintf(w, " · %d not processed", s.NotProcessed)
	}
	fmt.Fprintf(w, "\nchecks: %d passed, %d failed\n", s.Passed, s.Failed)
	if s.Compared > 0 {
		fmt.Fprintf(w, "sent vs stored: %d compared · %d match · %d fixed by a rule · %d dropped by a rule · %d differ (unexplained)\n", s.Compared, s.Matched, s.Fixed, s.Dropped, s.Differed)
	}
	if load {
		fmt.Fprintf(w, "throughput %.1f/s · p50 %.1f ms · p95 %.1f ms · p99 %.1f ms · max %.1f ms · errors %.1f%%\n",
			s.ThroughputPerSec, s.P50Ms, s.P95Ms, s.P99Ms, s.MaxMs, s.ErrorRate*100)
	}
	for _, n := range rep.Notes {
		fmt.Fprintln(w, "note:", n)
	}
	if s.OK() {
		fmt.Fprintln(w, "RESULT: ok")
	} else {
		fmt.Fprintln(w, "RESULT: FAILED")
	}
}
