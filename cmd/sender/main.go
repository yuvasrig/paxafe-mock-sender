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
	id := fs.String("scenario", "", "scenario id (see `sender list`)")
	url := fs.String("url", os.Getenv("API_BASE_URL"), "API base URL")
	key := fs.String("key", os.Getenv("SENDER_API_KEY"), "API key")
	rate := fs.Float64("rate", 10, "load: requests per second")
	dur := fs.Duration("duration", 10*time.Second, "load: duration")
	points := fs.Int("points", 12, "route length")
	account := fs.Int64("account", accountFromEnv(), "Tive AccountId in payloads (0 = omit)")
	verify := fs.Int("verify", 300, "most events to look up afterwards")
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sc, ok := scenario.Get(*id)
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
		Client: client, Key: *key, RunID: "cli-" + strconv.FormatInt(time.Now().UnixMilli(), 36), VerifyCap: *verify,
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

	s := rep.Summary
	fmt.Fprintf(w, "\n%d sent · %d acknowledged · %d processed · %d rejected (expected) · %d missing", s.Sent, s.Acknowledged, s.Processed, s.ExpectedRejections, s.Missing)
	if s.Refused > 0 {
		fmt.Fprintf(w, " · %d refused (429/503/401/413: not taken, not lost)", s.Refused)
	}
	if s.NotProcessed > 0 {
		fmt.Fprintf(w, " · %d not processed", s.NotProcessed)
	}
	fmt.Fprintf(w, "\nchecks: %d passed, %d failed\n", s.Passed, s.Failed)
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
