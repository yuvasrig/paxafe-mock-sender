// Package web is the sender's browser UI: pick a scenario, run it against the API, see every request,
// its answer and whether every acknowledged payload can be found again. Server-rendered with html/template;
// htmx swaps the result fragments in.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
	"github.com/yuvasrig/paxafe-mock-sender/internal/runner"
	"github.com/yuvasrig/paxafe-mock-sender/internal/scenario"
)

//go:embed templates/*.html
var templateFS embed.FS

const (
	maxLoadRate     = 100.0
	maxLoadDuration = 30 * time.Second
	maxPoints       = 60
	maxRows         = 200
	keepRuns        = 20
	runTimeout      = 100 * time.Second
	wakeTimeout     = 90 * time.Second
)

// Server serves the UI.
type Server struct {
	Client     *api.Client
	DefaultKey string // SENDER_API_KEY: used server-side only, never sent to the browser
	Account    int64  // Tive AccountId put in generated payloads
	Log        *slog.Logger

	tmpl *template.Template
	mu   sync.Mutex
	busy bool
	runs map[string]*runner.Report
	ord  []string
}

// New builds a server.
func New(client *api.Client, defaultKey string, account int64, log *slog.Logger) (*Server, error) {
	funcs := template.FuncMap{
		"ms":      func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
		"pct":     func(f float64) string { return strconv.FormatFloat(f*100, 'f', 1, 64) + "%" },
		"rate":    func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
		"seconds": func(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) },
		"inc":     func(i int) int { return i + 1 },
		"pretty":  pretty,
		"join":    strings.Join,
	}
	t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	return &Server{Client: client, DefaultKey: defaultKey, Account: account, Log: log, tmpl: t, runs: map[string]*runner.Report{}}, nil
}

// pretty indents JSON for display; anything else is shown as it is.
func pretty(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(b)
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /wake", s.wake)
	mux.HandleFunc("POST /run", s.run)
	mux.HandleFunc("GET /runs/{id}/{n}", s.detail)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.Log.Error("render", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(buf.String()))
}

type indexData struct {
	APIBase      string
	Scenarios    []scenario.Scenario
	HasServerKey bool
	MaxRate      float64
	MaxSeconds   int
	MaxPoints    int
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "index.html", indexData{
		APIBase: s.Client.Base, Scenarios: scenario.All(), HasServerKey: s.DefaultKey != "",
		MaxRate: maxLoadRate, MaxSeconds: int(maxLoadDuration.Seconds()), MaxPoints: maxPoints,
	})
}

// wake pings the API's /healthz so a sleeping free-tier instance starts before the first run.
func (s *Server) wake(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), wakeTimeout)
	defer cancel()
	took, err := s.Client.Wake(ctx)
	s.render(w, "wake.html", map[string]any{"OK": err == nil, "Took": took, "Err": errText(err)})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.message(w, http.StatusBadRequest, "Could not read the form.")
		return
	}
	sc, ok := scenario.Get(r.FormValue("scenario"))
	if !ok {
		s.message(w, http.StatusBadRequest, "Pick a scenario.")
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		key = s.DefaultKey
	}
	if key == "" {
		s.message(w, http.StatusBadRequest, "No API key: enter one, or configure SENDER_API_KEY on the sender.")
		return
	}

	p := scenario.Params{
		Account:  s.Account,
		Points:   clampInt(formInt(r, "points", 12), 2, maxPoints),
		Rate:     clampFloat(formFloat(r, "rate", 10), 1, maxLoadRate),
		Duration: time.Duration(clampFloat(formFloat(r, "duration", 10), 1, maxLoadDuration.Seconds()) * float64(time.Second)),
		Now:      time.Now().UTC().Truncate(time.Second),
		Seed:     time.Now().UnixNano(),
	}

	if !s.acquire() {
		s.message(w, http.StatusConflict, "Another run is still in progress. Wait for it to finish.")
		return
	}
	defer s.release()

	ctx, cancel := context.WithTimeout(r.Context(), runTimeout)
	defer cancel()
	runID := "web-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	rep, err := runner.Run(ctx, runner.Config{Client: s.Client, Key: key, RunID: runID, Params: p}, sc)
	if err != nil {
		s.Log.Error("run failed", "scenario", sc.ID, "error", err)
		s.message(w, http.StatusInternalServerError, "The run could not be started.")
		return
	}
	s.store(rep)
	s.Log.Info("run finished", "run_id", runID, "scenario", sc.ID, "sent", rep.Summary.Sent, "failed", rep.Summary.Failed, "missing", rep.Summary.Missing)

	rows := rep.Results
	truncated := false
	if len(rows) > maxRows {
		rows, truncated = rows[:maxRows], true
	}
	s.render(w, "report.html", map[string]any{"R": rep, "Rows": rows, "Truncated": truncated, "Load": sc.Load})
}

func (s *Server) message(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	s.render(w, "message.html", msg)
}

func (s *Server) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	return true
}

func (s *Server) release() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

func (s *Server) store(rep *runner.Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[rep.RunID] = rep
	s.ord = append(s.ord, rep.RunID)
	for len(s.ord) > keepRuns {
		delete(s.runs, s.ord[0])
		s.ord = s.ord[1:]
	}
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rep := s.runs[r.PathValue("id")]
	s.mu.Unlock()
	n, err := strconv.Atoi(r.PathValue("n"))
	if rep == nil || err != nil || n < 0 || n >= len(rep.Results) {
		s.message(w, http.StatusNotFound, "That run is no longer available.")
		return
	}
	s.render(w, "detail.html", rep.Results[n])
}

func formInt(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.FormValue(name)); err == nil {
		return v
	}
	return def
}

func formFloat(r *http.Request, name string, def float64) float64 {
	if v, err := strconv.ParseFloat(r.FormValue(name), 64); err == nil {
		return v
	}
	return def
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
