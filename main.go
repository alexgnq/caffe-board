// caffe board puts caffe's table on the web: every terminal running caffe with
// CAFFE_BOARD_URL set shows its agent sessions, open apps and barista cat here,
// as they look in the terminal. The pods serving the page are listed as
// baristas, so you can watch the cluster share the work.
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
	"regexp"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

//go:embed templates/index.html
var templateFS embed.FS

// drinks are the agents and apps caffe serves, in caffe's colours.
var drinks = map[string]string{
	"claude":   "#de7d5a",
	"codex":    "#56a0f5",
	"copilot":  "#a78bfa",
	"opencode": "#e870a0",
	"pi":       "#6ec478",
	"vscode":   "#28bed2",
	"t3":       "#8a8a96",
}

// states are what caffe says a card is doing.
var states = map[string]bool{
	"brewing": true, "ready": true, "idle": true, "cold": true, "pouring": true, "served": true,
	"busy": true, "open": true, "closed": true,
}

// card is one card at a bar, with the same lines as in the terminal.
type card struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	Where  string `json:"where"`
	State  string `json:"state"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

// cat is the bar's barista, coat and all. Kind is "cat", or "capybara" for
// caffe's capybara mode, where it rides a turtle with a Shell of this colour.
type cat struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Title string `json:"title"`
	Coat  string `json:"coat"`
	Shade string `json:"shade"`
	Eyes  string `json:"eyes"`
	Shell string `json:"shell"`
}

// snapshot is what caffe sends every 10 seconds.
type snapshot struct {
	Cat   cat    `json:"cat"`
	Tally string `json:"tally"`
	Awake int    `json:"awake"`
	Cards []card `json:"cards"`
}

type bar struct {
	Name string
	snapshot
}

// barista is a pod serving the board, and how many caffe updates it took.
type barista struct {
	Pod     string
	Tint    string
	Updates int
}

type pageData struct {
	Bars     []bar
	Baristas []barista
	Pod      string
	PodTint  string
	BoardURL string
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
	hexColor   = regexp.MustCompile(`^#[0-9a-f]{6}$`)
)

const (
	// barTimeout is how long a bar stays open after caffe last reported.
	barTimeout = time.Minute
	// heartbeat is how often a pod tells the others it is still serving.
	heartbeat = 15 * time.Second
)

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
CREATE TABLE IF NOT EXISTS bars (
  name     TEXT PRIMARY KEY,
  snapshot JSONB NOT NULL,
  updated  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS pods (
  name    TEXT PRIMARY KEY,
  updates BIGINT NOT NULL DEFAULT 0,
  seen    TIMESTAMPTZ NOT NULL DEFAULT now()
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

// beat keeps this pod on the list of baristas.
func beat(ctx context.Context) {
	if _, err := db.ExecContext(ctx,
		`INSERT INTO pods (name, seen) VALUES ($1, now())
		 ON CONFLICT (name) DO UPDATE SET seen = now()`, podName); err != nil {
		log.Printf("heartbeat failed: %v", err)
	}
}

func clip(value string, limit int) string {
	value = strings.TrimSpace(value)
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func colorOr(value, fallback string) string {
	if hexColor.MatchString(value) {
		return value
	}
	return fallback
}

// clean keeps what the page can show from what caffe sent, and drops the rest.
func clean(in snapshot) snapshot {
	out := snapshot{
		Cat: cat{
			Kind:  "cat",
			Name:  clip(in.Cat.Name, 24),
			Title: clip(in.Cat.Title, 48),
			Coat:  colorOr(in.Cat.Coat, "#36363e"),
			Shade: colorOr(in.Cat.Shade, "#36363e"),
			Eyes:  colorOr(in.Cat.Eyes, "#e8c846"),
			Shell: colorOr(in.Cat.Shell, "#568ccc"),
		},
		Tally: clip(in.Tally, 60),
		Awake: max(in.Awake, 0),
		Cards: []card{},
	}
	if in.Cat.Kind == "capybara" {
		out.Cat.Kind = "capybara"
	}
	for _, c := range in.Cards {
		if _, ok := drinks[c.Agent]; !ok || !states[c.State] || (c.Kind != "session" && c.Kind != "app") {
			continue
		}
		out.Cards = append(out.Cards, card{
			ID:     clip(c.ID, 16),
			Kind:   c.Kind,
			Agent:  c.Agent,
			Name:   clip(c.Name, 24),
			Where:  clip(c.Where, 48),
			State:  c.State,
			Status: clip(c.Status, 60),
			Note:   clip(c.Note, 80),
		})
		if len(out.Cards) == 24 {
			break
		}
	}
	return out
}

func loadPage(ctx context.Context, data *pageData) error {
	rows, err := db.QueryContext(ctx,
		`SELECT name, snapshot FROM bars WHERE updated > now() - make_interval(secs => $1) ORDER BY name`,
		barTimeout.Seconds())
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
		if err := json.Unmarshal(raw, &item.snapshot); err != nil {
			return err
		}
		data.Bars = append(data.Bars, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = db.QueryContext(ctx,
		`SELECT name, updates FROM pods WHERE seen > now() - make_interval(secs => $1) ORDER BY name`,
		(4 * heartbeat).Seconds())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item barista
		if err := rows.Scan(&item.Pod, &item.Updates); err != nil {
			return err
		}
		item.Tint = tint(item.Pod)
		data.Baristas = append(data.Baristas, item)
	}
	return rows.Err()
}

// boardURL is this page's address, for the command that points caffe here.
func boardURL(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil || request.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + request.Host
}

func handleIndex(writer http.ResponseWriter, request *http.Request) {
	data := pageData{
		Pod: podName, PodTint: tint(podName), BoardURL: boardURL(request),
		Version: version, DBStatus: "connected",
	}
	if err := loadPage(request.Context(), &data); err != nil {
		data.DBStatus = "error"
		data.DBError = err.Error()
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(writer, data); err != nil {
		log.Printf("render failed: %v", err)
	}
}

func authorized(request *http.Request) bool {
	if boardToken == "" {
		return true
	}
	got := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(boardToken)) == 1
}

// handlePutBar stores caffe's latest snapshot of a bar.
func handlePutBar(writer http.ResponseWriter, request *http.Request) {
	if !authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := clip(request.PathValue("name"), 40)
	var body snapshot
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10)).Decode(&body); err != nil || name == "" {
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(clean(body))
	if _, err := db.ExecContext(request.Context(),
		`INSERT INTO bars (name, snapshot, updated) VALUES ($1, $2, now())
		 ON CONFLICT (name) DO UPDATE SET snapshot = EXCLUDED.snapshot, updated = now()`,
		name, raw); err != nil {
		log.Printf("bar update failed: %v", err)
		http.Error(writer, "database error", http.StatusInternalServerError)
		return
	}
	if _, err := db.ExecContext(request.Context(),
		`UPDATE pods SET updates = updates + 1 WHERE name = $1`, podName); err != nil {
		log.Printf("count update failed: %v", err)
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

// span is a duration the way caffe writes it.
func span(seconds int) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", seconds)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func main() {
	var err error
	tmpl, err = template.New("index.html").Funcs(template.FuncMap{
		"color": func(agent string) string { return drinks[agent] },
		"span":  span,
	}).ParseFS(templateFS, "templates/index.html")
	if err != nil {
		log.Fatalf("parse template: %v", err)
	}

	ctx := context.Background()
	if err := connect(ctx); err != nil {
		log.Fatalf("startup: %v", err)
	}
	beat(ctx)
	go func() {
		for range time.Tick(heartbeat) {
			beat(ctx)
		}
	}()
	log.Printf("connected to postgres, serving on %s as pod %s", listenOn, podName)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("GET /healthz", handleHealthz)
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
