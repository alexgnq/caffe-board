// caffe board is a coffee bar for the room: order a cup for the coding agent
// you work with, and see who else did. Every cup remembers the pod that
// poured it, so you can watch the cluster share the work.
package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

//go:embed templates/index.html
var templateFS embed.FS

// agent is a drink on the menu, in the same colours as caffe in the terminal.
type agent struct {
	ID    string
	Name  string
	Color string
}

var menu = []agent{
	{"claude", "Claude", "#de7d5a"},
	{"codex", "Codex", "#56a0f5"},
	{"copilot", "Copilot", "#a78bfa"},
	{"opencode", "OpenCode", "#e870a0"},
	{"pi", "Pi", "#6ec478"},
	{"human", "Just me", "#b98a5a"},
}

func findAgent(id string) (agent, bool) {
	for _, item := range menu {
		if item.ID == id {
			return item, true
		}
	}
	return agent{}, false
}

type cup struct {
	ID      int
	Fresh   bool
	Name    string
	Agent   agent
	Note    string
	Pod     string
	PodTint string
	Short   string
	Ago     string
}

type tally struct {
	Agent agent
	Count int
}

// sip is one session at a laptop's bar, as caffe reports it.
type sip struct {
	Agent agent
	State string
	For   string
}

// bar is a laptop running caffe with the board turned on.
type bar struct {
	Name     string
	Sessions []sip
}

// barista is a pod and how many cups it has poured.
type barista struct {
	Pod   string
	Tint  string
	Count int
}

type pageData struct {
	Menu     []agent
	Bars     []bar
	Baristas []barista
	Newest   *cup
	Cups     []cup
	Tally    []tally
	Total    int
	Pod      string
	PodTint  string
	Pods     int
	DBStatus string
	DBError  string
	Version  string
}

var (
	db       *sql.DB
	tmpl     *template.Template
	version  = envOr("APP_VERSION", "1.0.0")
	podName  = envOr("POD_NAME", hostname())
	listenOn = ":" + envOr("PORT", "8080")
	// boardToken, when set, is the bearer token caffe must send to update a bar.
	boardToken = os.Getenv("BOARD_TOKEN")
)

// barTimeout is how long a bar stays open after caffe last reported.
const barTimeout = time.Minute

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// hostname is the pod name in Kubernetes, so the page shows which pod
// answered even when POD_NAME is not set.
func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "local"
	}
	return name
}

var podTints = []string{"#e4572e", "#17a398", "#f3a712", "#6c5ce7", "#2e86de", "#c0392b", "#27ae60", "#d35400"}

// tint gives each pod its own colour, so a reload that lands on another
// replica is easy to spot.
func tint(pod string) string {
	h := fnv.New32a()
	h.Write([]byte(pod))
	return podTints[h.Sum32()%uint32(len(podTints))]
}

// databaseURL reads DATABASE_URL, or builds a DSN from the PG* variables a
// Helm chart or operator secret usually provides.
func databaseURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		envOr("PGHOST", "localhost"),
		envOr("PGPORT", "5432"),
		envOr("PGUSER", "postgres"),
		os.Getenv("PGPASSWORD"),
		envOr("PGDATABASE", "caffe"),
		envOr("PGSSLMODE", "disable"),
	)
}

const schema = `
CREATE TABLE IF NOT EXISTS cups (
  id      SERIAL PRIMARY KEY,
  name    TEXT NOT NULL,
  agent   TEXT NOT NULL,
  note    TEXT NOT NULL DEFAULT '',
  pod     TEXT NOT NULL,
  created TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS bars (
  name     TEXT PRIMARY KEY,
  sessions JSONB NOT NULL,
  updated  TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// schemaLock serialises the schema setup when several replicas start at once;
// CREATE TABLE IF NOT EXISTS alone can still race.
const schemaLock = 20261124

func migrate(ctx context.Context) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLock); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	return tx.Commit()
}

// connect retries because the app usually starts before the database accepts
// connections.
func connect(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= 30; attempt++ {
		handle, err := sql.Open("postgres", databaseURL())
		if err != nil {
			lastErr = err
		} else {
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = handle.PingContext(pingCtx)
			cancel()
			if err == nil {
				handle.SetMaxOpenConns(5)
				handle.SetConnMaxLifetime(5 * time.Minute)
				db = handle
				return migrate(ctx)
			}
			lastErr = err
			_ = handle.Close()
		}
		log.Printf("database not ready (attempt %d/30): %v", attempt, lastErr)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("database unreachable after 30 attempts: %w", lastErr)
}

func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
}

// short is the part of a pod name that tells replicas apart.
func short(pod string) string {
	if i := strings.LastIndex(pod, "-"); i >= 0 && i < len(pod)-1 {
		return pod[i+1:]
	}
	return pod
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return t.Format("2 Jan")
	}
}

func loadPage(ctx context.Context, data *pageData) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, agent, note, pod, created FROM cups ORDER BY id DESC LIMIT 60`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item cup
		var agentID string
		var created time.Time
		if err := rows.Scan(&item.ID, &item.Name, &agentID, &item.Note, &item.Pod, &created); err != nil {
			return err
		}
		item.Agent, _ = findAgent(agentID)
		item.PodTint = tint(item.Pod)
		item.Short = short(item.Pod)
		item.Ago = ago(created)
		item.Fresh = time.Since(created) < 2*time.Minute
		data.Cups = append(data.Cups, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	counts := map[string]int{}
	rows, err = db.QueryContext(ctx, `SELECT agent, count(*) FROM cups GROUP BY agent`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		counts[id] = n
		data.Total += n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range menu {
		data.Tally = append(data.Tally, tally{item, counts[item.ID]})
	}

	if len(data.Cups) > 0 {
		data.Newest = &data.Cups[0]
	}

	rows, err = db.QueryContext(ctx,
		`SELECT pod, count(*) FROM cups GROUP BY pod ORDER BY count(*) DESC, pod`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item barista
		if err := rows.Scan(&item.Pod, &item.Count); err != nil {
			return err
		}
		item.Tint = tint(item.Pod)
		data.Baristas = append(data.Baristas, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	data.Pods = len(data.Baristas)
	return loadBars(ctx, data)
}

// stored is a session as the bars table keeps it.
type stored struct {
	Agent string    `json:"agent"`
	State string    `json:"state"`
	Since time.Time `json:"since"`
}

func loadBars(ctx context.Context, data *pageData) error {
	rows, err := db.QueryContext(ctx,
		`SELECT name, sessions FROM bars WHERE updated > now() - $1::interval ORDER BY name`,
		fmt.Sprintf("%d seconds", int(barTimeout.Seconds())))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item bar
		var raw []byte
		if err := rows.Scan(&item.Name, &raw); err != nil {
			return err
		}
		var sessions []stored
		if err := json.Unmarshal(raw, &sessions); err != nil {
			return err
		}
		for _, s := range sessions {
			a, _ := findAgent(s.Agent)
			item.Sessions = append(item.Sessions, sip{a, s.State, span(time.Since(s.Since))})
		}
		data.Bars = append(data.Bars, item)
	}
	return rows.Err()
}

var states = map[string]bool{"brewing": true, "ready": true, "idle": true, "cold": true}

// report is what caffe sends: each session's agent, state, and seconds in it.
type report struct {
	Sessions []struct {
		Agent string  `json:"agent"`
		State string  `json:"state"`
		Since float64 `json:"since"`
	} `json:"sessions"`
}

func authorized(request *http.Request) bool {
	if boardToken == "" {
		return true
	}
	got := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(boardToken)) == 1
}

// handlePutBar replaces a bar's sessions with caffe's latest snapshot.
func handlePutBar(writer http.ResponseWriter, request *http.Request) {
	if !authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := clip(request.PathValue("name"), 40)
	var body report
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10)).Decode(&body); err != nil || name == "" {
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	now := time.Now()
	sessions := []stored{}
	for _, s := range body.Sessions {
		if _, ok := findAgent(s.Agent); !ok || !states[s.State] || s.Agent == "human" || len(sessions) == 24 {
			continue
		}
		since := now.Add(-time.Duration(max(s.Since, 0) * float64(time.Second)))
		sessions = append(sessions, stored{s.Agent, s.State, since})
	}
	raw, _ := json.Marshal(sessions)
	if _, err := db.ExecContext(request.Context(),
		`INSERT INTO bars (name, sessions, updated) VALUES ($1, $2, now())
		 ON CONFLICT (name) DO UPDATE SET sessions = EXCLUDED.sessions, updated = now()`,
		name, raw); err != nil {
		log.Printf("bar update failed: %v", err)
		http.Error(writer, "database error", http.StatusInternalServerError)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// handleDeleteBar closes a bar when caffe quits.
func handleDeleteBar(writer http.ResponseWriter, request *http.Request) {
	if !authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, err := db.ExecContext(request.Context(),
		`DELETE FROM bars WHERE name = $1`, clip(request.PathValue("name"), 40)); err != nil {
		http.Error(writer, "database error", http.StatusInternalServerError)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func clip(value string, limit int) string {
	value = strings.TrimSpace(value)
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func handleIndex(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}

	if request.Method == http.MethodPost {
		name := clip(request.FormValue("name"), 40)
		if name == "" {
			name = "anonymous"
		}
		note := clip(request.FormValue("note"), 80)
		if item, ok := findAgent(request.FormValue("agent")); ok {
			if _, err := db.ExecContext(request.Context(),
				`INSERT INTO cups (name, agent, note, pod) VALUES ($1, $2, $3, $4)`,
				name, item.ID, note, podName); err != nil {
				log.Printf("insert failed: %v", err)
			}
		}
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}

	data := pageData{Menu: menu, Pod: podName, PodTint: tint(podName), Version: version, DBStatus: "connected"}
	if err := loadPage(request.Context(), &data); err != nil {
		data.DBStatus = "error"
		data.DBError = err.Error()
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(writer, data); err != nil {
		log.Printf("render failed: %v", err)
	}
}

// handleHealthz is the readiness gate: not ready until the database answers.
func handleHealthz(writer http.ResponseWriter, request *http.Request) {
	if db == nil {
		http.Error(writer, "database not connected", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		http.Error(writer, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(writer, "ok")
}

func main() {
	var err error
	tmpl, err = template.ParseFS(templateFS, "templates/index.html")
	if err != nil {
		log.Fatalf("parse template: %v", err)
	}

	if err := connect(context.Background()); err != nil {
		log.Fatalf("startup: %v", err)
	}
	log.Printf("connected to postgres, serving on %s as pod %s", listenOn, podName)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("PUT /api/bars/{name}", handlePutBar)
	mux.HandleFunc("DELETE /api/bars/{name}", handleDeleteBar)

	server := &http.Server{
		Addr:              listenOn,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}
