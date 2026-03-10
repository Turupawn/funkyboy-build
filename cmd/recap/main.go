package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var log *slog.Logger

func main() {
	log = slog.New(slog.NewTextHandler(os.Stdout, nil))

	required := []string{"GITHUB_TOKEN", "GITHUB_USER"}
	for _, k := range required {
		if os.Getenv(k) == "" {
			log.Error("missing env var", "key", k)
			os.Exit(1)
		}
	}

	port := envOr("PORT", "8080")

	// Start cron for daily recap
	go runRecapCron()

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/recap", handleRecap)
	mux.HandleFunc("POST /api/query", handleQuery)
	mux.HandleFunc("GET /health", handleHealth)

	log.Info("build started", "port", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}

// Cron — runs daily recap and pushes to notify URLs

func runRecapCron() {
	hour := envOr("RECAP_HOUR", "07:00")
	loc := loadTimezone()
	log.Info("recap cron started", "hour", hour, "tz", loc.String())

	for {
		now := time.Now().In(loc)
		next := nextSendTime(now, hour)
		wait := next.Sub(now)
		log.Info("next recap", "at", next.Format("2006-01-02 15:04"), "in", wait.Round(time.Minute))
		time.Sleep(wait)

		text, err := buildRecap(1)
		if err != nil {
			log.Error("recap failed", "err", err)
		} else if text != "" {
			notify(text)
		}

		time.Sleep(61 * time.Second)
	}
}

func nextSendTime(now time.Time, hour string) time.Time {
	parts := strings.Split(hour, ":")
	h, m := 7, 0
	if len(parts) == 2 {
		fmt.Sscanf(parts[0], "%d", &h)
		fmt.Sscanf(parts[1], "%d", &m)
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target
}

// Notify — push to all NOTIFY_URLS

func notify(text string) {
	urls := os.Getenv("NOTIFY_URLS")
	if urls == "" {
		log.Warn("no NOTIFY_URLS configured, skipping")
		return
	}

	body, _ := json.Marshal(map[string]string{"text": text})

	for _, u := range strings.Split(urls, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		resp, err := http.Post(u, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Error("notify failed", "url", u, "err", err)
			continue
		}
		resp.Body.Close()
		log.Info("notify sent", "url", u, "status", resp.StatusCode)
	}
}

// HTTP handlers

func handleRecap(w http.ResponseWriter, r *http.Request) {
	days := 1
	if d := r.URL.Query().Get("days"); d != "" {
		if parsed, err := strconv.Atoi(d); err == nil && parsed > 0 && parsed <= 30 {
			days = parsed
		}
	}

	text, err := buildRecap(days)
	if err != nil {
		log.Error("recap failed", "err", err)
		http.Error(w, "error", http.StatusBadGateway)
		return
	}

	if text == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"text": text})
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
		ChatID  string `json:"chat_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	days := parseDays(req.Message)
	log.Info("query", "message", req.Message, "days", days, "chat_id", req.ChatID)

	text, err := buildRecap(days)
	if err != nil {
		log.Error("query recap failed", "err", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"response": "Error consultando GitHub", "pending": false})
		return
	}

	if text == "" {
		text = fmt.Sprintf("No hubo actividad en los ultimos %d dias.", days)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"response": text, "pending": false})
}

func parseDays(msg string) int {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return 1
	}

	prompt := fmt.Sprintf(`El usuario quiere saber su actividad de GitHub. Extraé cuántos días hacia atrás quiere consultar.
Respondé SOLO con un número entero entre 1 y 30. Nada más.

Ejemplos:
- "ayer" → 1
- "que hice hoy" → 1
- "resumen de la semana" → 7
- "últimos 3 días" → 3
- "este mes" → 30
- "quincena" → 15
- "last week" → 7

Mensaje: "%s"`, msg)

	body, _ := json.Marshal(map[string]string{
		"prompt": prompt,
		"tier":   envOr("LLM_TIER", "fast"),
	})

	endpoint := envOr("LLM_ENDPOINT", "http://funkyboy-llm:8090")
	resp, err := http.Post(endpoint+"/v1/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Warn("parseDays llm failed, defaulting to 1", "err", err)
		return 1
	}
	defer resp.Body.Close()

	var result struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 1
	}

	n, err := strconv.Atoi(strings.TrimSpace(result.Response))
	if err != nil || n < 1 || n > 30 {
		return 1
	}
	return n
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

// Core recap logic

func buildRecap(days int) (string, error) {
	loc := loadTimezone()
	now := time.Now().In(loc)
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start := end.Add(-time.Duration(days) * 24 * time.Hour)

	log.Info("fetching activity", "days", days, "from", start.Format("2006-01-02"))

	events, err := fetchGitHubActivity(start, end)
	if err != nil {
		return "", fmt.Errorf("github: %w", err)
	}

	if len(events) == 0 {
		log.Info("no activity, skipping")
		return "", nil
	}

	raw := formatRawActivity(events, start, days)

	summary, err := summarizeWithLLM(raw)
	if err != nil {
		log.Warn("llm failed, using raw", "err", err)
		return raw, nil
	}

	return summary, nil
}

// Timezone

func loadTimezone() *time.Location {
	tz := envOr("TZ", "America/Costa_Rica")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Warn("invalid timezone, using UTC", "tz", tz)
		return time.UTC
	}
	return loc
}

// GitHub

type GitHubEvent struct {
	Type      string          `json:"type"`
	Repo      GitHubRepo      `json:"repo"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

type GitHubRepo struct {
	Name string `json:"name"`
}

type PushPayload struct {
	Commits []PushCommit `json:"commits"`
}

type PushCommit struct {
	Message string `json:"message"`
}

type PRPayload struct {
	Action string `json:"action"`
	PR     struct {
		Title string `json:"title"`
	} `json:"pull_request"`
}

type IssuePayload struct {
	Action string `json:"action"`
	Issue  struct {
		Title string `json:"title"`
	} `json:"issue"`
}

type CreatePayload struct {
	RefType string `json:"ref_type"`
	Ref     string `json:"ref"`
}

func fetchGitHubActivity(start, end time.Time) ([]GitHubEvent, error) {
	user := os.Getenv("GITHUB_USER")
	token := os.Getenv("GITHUB_TOKEN")

	var allEvents []GitHubEvent

	for page := 1; page <= 3; page++ {
		url := fmt.Sprintf("https://api.github.com/users/%s/events?per_page=100&page=%d", user, page)
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
		}

		var events []GitHubEvent
		if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()

		for _, e := range events {
			if e.CreatedAt.Before(start) {
				return allEvents, nil
			}
			if !e.CreatedAt.Before(start) && e.CreatedAt.Before(end) {
				allEvents = append(allEvents, e)
			}
		}

		if len(events) < 100 {
			break
		}
	}

	return allEvents, nil
}

func formatRawActivity(events []GitHubEvent, start time.Time, days int) string {
	var b strings.Builder
	if days == 1 {
		fmt.Fprintf(&b, "GitHub activity for %s:\n\n", start.Format("2006-01-02"))
	} else {
		fmt.Fprintf(&b, "GitHub activity (%d days from %s):\n\n", days, start.Format("2006-01-02"))
	}

	for _, e := range events {
		repo := shortRepo(e.Repo.Name)
		switch e.Type {
		case "PushEvent":
			var p PushPayload
			json.Unmarshal(e.Payload, &p)
			for _, c := range p.Commits {
				firstLine := strings.Split(c.Message, "\n")[0]
				fmt.Fprintf(&b, "- [%s] commit: %s\n", repo, firstLine)
			}
		case "PullRequestEvent":
			var p PRPayload
			json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "- [%s] PR %s: %s\n", repo, p.Action, p.PR.Title)
		case "IssuesEvent":
			var p IssuePayload
			json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "- [%s] issue %s: %s\n", repo, p.Action, p.Issue.Title)
		case "CreateEvent":
			var p CreatePayload
			json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "- [%s] created %s: %s\n", repo, p.RefType, p.Ref)
		default:
			fmt.Fprintf(&b, "- [%s] %s\n", repo, e.Type)
		}
	}

	return b.String()
}

func shortRepo(full string) string {
	parts := strings.Split(full, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return full
}

// LLM gateway

func summarizeWithLLM(raw string) (string, error) {
	endpoint := envOr("LLM_ENDPOINT", "http://funkyboy-llm:8090")
	tier := envOr("LLM_TIER", "fast")

	prompt := fmt.Sprintf(`Sos un asistente que resume actividad de desarrollo.
Resumi esta actividad de GitHub en español, conciso, agrupado por repo.
No uses emojis. Formato texto plano, corto.

%s`, raw)

	body, _ := json.Marshal(map[string]string{
		"prompt": prompt,
		"tier":   tier,
	})

	resp, err := http.Post(endpoint+"/v1/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("llm status %d", resp.StatusCode)
	}

	var result struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.Response, nil
}

// Helpers

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
