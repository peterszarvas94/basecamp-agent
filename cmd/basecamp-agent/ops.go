package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed static/datastar.js
var datastarJS []byte

type notifyWriter struct {
	io.Writer
	notify func()
}

func (w notifyWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 && w.notify != nil {
		w.notify()
	}
	return n, err
}

type JobStatus struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Agent     string `json:"agent"`
	EventID   int64  `json:"event_id"`
	Target    string `json:"target"`
	Repo      string `json:"repo,omitempty"`
	Worktree  string `json:"worktree,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Base      string `json:"base,omitempty"`
	PGID      int    `json:"pgid,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	PRURL     string `json:"pr_url,omitempty"`
	Message   string `json:"message,omitempty"`
}

func (s *Server) registerOpsRoutes(mux *http.ServeMux) {
	if !s.cfg.Ops.Enabled {
		return
	}
	mux.HandleFunc("/ops", s.opsIndex)
	mux.HandleFunc("/ops/datastar.js", s.opsDatastar)
	mux.HandleFunc("/ops/stream", s.opsStream)
	mux.HandleFunc("/ops/", s.opsRoute)
}

func (s *Server) jobsRoot() string {
	return filepath.Join(filepathDir(s.cfg.StatePath), "jobs")
}

const opsSessionCookie = "basecamp_agent_ops"

// authorizeOps accepts the ops token as a bearer header, a session cookie, or
// a ?token= query. A query token is exchanged for the cookie and page requests
// are redirected to the same URL without it, so the token stays out of browser
// history, bookmarks, and links.
func (s *Server) authorizeOps(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Ops.Token == "" {
		return true
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && secretEqual(bearer, s.cfg.Ops.Token) {
		return true
	}
	session := opsSessionValue(s.cfg.Ops.Token)
	if c, err := r.Cookie(opsSessionCookie); err == nil && secretEqual(c.Value, session) {
		return true
	}
	query := r.URL.Query()
	if !query.Has("token") || !secretEqual(query.Get("token"), s.cfg.Ops.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     opsSessionCookie,
		Value:    session,
		Path:     "/ops",
		MaxAge:   30 * 24 * 60 * 60,
		HttpOnly: true,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
	})
	if r.Method != http.MethodGet || r.URL.Path == "/ops/stream" || r.URL.Path == "/ops/datastar.js" {
		return true
	}
	query.Del("token")
	clean := *r.URL
	clean.RawQuery = query.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
	return false
}

// opsSessionValue derives the cookie value from the token, so the cookie never
// holds the token and rotating the token ends every session.
func opsSessionValue(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("basecamp-agent ops session"))
	return hex.EncodeToString(mac.Sum(nil))
}

func secretEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) opsDatastar(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeOps(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(datastarJS)
}

func (s *Server) opsIndex(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeOps(w, r) {
		return
	}
	if r.URL.Path != "/ops" && r.URL.Path != "/ops/" {
		http.NotFound(w, r)
		return
	}
	s.renderOpsPage(w, 0)
}

// renderOpsPage renders the jobs dashboard, limited to one Basecamp project
// when project is not 0.
func (s *Server) renderOpsPage(w http.ResponseWriter, project int64) {
	title, nav := "Basecamp Agent Jobs", s.opsProjectNav()
	if project != 0 {
		title = "Project " + strconv.FormatInt(project, 10)
		if repo := s.cfg.ProjectRepos[strconv.FormatInt(project, 10)]; repo != "" {
			title += " · " + repo
		}
		nav = `<a class="link link-hover text-sm opacity-70" href="/ops">← all projects</a>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html data-theme="night"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%[2]s</title>
<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css">
<link href="https://cdn.jsdelivr.net/npm/daisyui@5/themes.css" rel="stylesheet" type="text/css">
<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>
<style>%[1]s</style></head><body data-init="@get('/ops/stream%[4]s')" class="min-h-screen bg-base-200 text-base-content">
<main class="ops-shell"><header class="mb-5 flex flex-wrap items-center justify-between gap-3"><div><h1 class="text-2xl font-bold tracking-tight md:text-3xl">%[2]s</h1>%[3]s</div><span class="badge badge-info badge-outline gap-2"><span class="status status-info animate-pulse"></span>live</span></header>
<section id="jobs"><div class="skeleton h-32 w-full rounded-box"></div></section></main>
<script type="module" src="/ops/datastar.js"></script>
</body></html>`, detailCSS(), html.EscapeString(title), nav, projectQuery(project))
}

func (s *Server) opsRoute(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeOps(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/ops/")
	if path == "stream" {
		s.opsStream(w, r)
		return
	}
	if path == "jobs/stream" {
		s.opsStream(w, r)
		return
	}
	if rest, ok := strings.CutPrefix(path, "projects/"); ok && r.Method == http.MethodGet {
		project, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || project <= 0 {
			http.NotFound(w, r)
			return
		}
		s.renderOpsPage(w, project)
		return
	}
	if !strings.HasPrefix(path, "jobs/") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "jobs/"), "/")
	if len(parts) == 1 && r.Method == http.MethodGet {
		s.opsJobDetail(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "stream" {
		s.opsJobStream(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
		s.opsStopJob(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "restart" && r.Method == http.MethodPost {
		s.opsRestartJob(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		s.opsCancelJob(w, r, parts[0])
		return
	}
	http.NotFound(w, r)
}

func jobID(job Job) string {
	if job.Attempt <= 1 {
		return fmt.Sprintf("%d-%s", job.Event.ID, job.Agent)
	}
	return fmt.Sprintf("%d-%s-retry-%d", job.Event.ID, job.Agent, job.Attempt)
}

func restartPrompt(job Job) string {
	if job.PreviousJobID == "" {
		return ""
	}
	return fmt.Sprintf(`Restart context:
- This is a restarted job attempt.
- Previous job id: %s
- Previous status/log summary: %s
Before acting, inspect the previous attempt's status, logs, and worktree if useful. Do not assume previous changes are correct. If you reuse anything, copy it deliberately into this fresh worktree.

`, job.PreviousJobID, job.PreviousSummary)
}

func (s *Server) writeJobRecord(job Job) {
	if job.JobDir == "" {
		return
	}
	b, err := json.MarshalIndent(job, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(job.JobDir, "job.json"), b, 0600)
	}
}

func (s *Server) readJobRecord(id string) (Job, error) {
	var job Job
	b, err := os.ReadFile(filepath.Join(s.jobsRoot(), id, "job.json"))
	if err != nil {
		return job, err
	}
	return job, json.Unmarshal(b, &job)
}

func (s *Server) reconstructJobFromStatus(ctx context.Context, st JobStatus) (Job, error) {
	if st.Target == "" || st.Agent == "" || st.EventID == 0 {
		return Job{}, errors.New("original job record missing and status is incomplete")
	}
	projectID, recordingID := idsFromBasecampURL(st.Target)
	if projectID == 0 && len(s.cfg.AllowedProjectIDs) == 1 {
		projectID = s.cfg.AllowedProjectIDs[0]
	}
	if projectID == 0 {
		return Job{}, errors.New("cannot infer Basecamp project for legacy job")
	}
	profile := s.cfg.BotProfiles[st.Agent]
	if profile == "" {
		return Job{}, fmt.Errorf("agent %q has no profile", st.Agent)
	}
	instruction := st.Message
	if fetched, err := s.basecampJSON(ctx, "show", st.Target, "--json"); err == nil {
		instruction = collectText(fetched)
	}
	if strings.TrimSpace(instruction) == "" {
		instruction = "Restart the previous Basecamp-triggered task. Inspect the triggering item and nearby context before acting."
	}
	job := Job{Agent: st.Agent, Profile: profile, Target: st.Target, Instruction: instruction, Attempt: st.Attempt}
	job.Event.ID = st.EventID
	job.Event.Kind = "ops_restart"
	job.Event.CreatedAt = time.Now().UTC()
	job.Event.Creator = Person{ID: firstInt64(s.cfg.AllowedCreatorIDs), Name: "operator"}
	job.Event.Recording.ID = recordingID
	job.Event.Recording.URL = st.Target
	job.Event.Recording.Bucket.ID = projectID
	job.Title = st.Repo
	return job, nil
}

func idsFromBasecampURL(url string) (projectID, recordingID int64) {
	parts := strings.Split(url, "/")
	for i, part := range parts {
		if part == "buckets" && i+1 < len(parts) {
			projectID, _ = strconv.ParseInt(parts[i+1], 10, 64)
		}
	}
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSuffix(parts[i], ".json")
		if id, err := strconv.ParseInt(part, 10, 64); err == nil {
			recordingID = id
			break
		}
	}
	return projectID, recordingID
}

func basecampAppURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	// The API host and the legacy 3.basecamp.com host both address records a
	// person should open on app.basecamp.com.
	if u.Host != "app.basecamp.com" && (strings.HasSuffix(u.Host, "basecampapi.com") || strings.HasSuffix(u.Host, "basecamp.com")) {
		u.Scheme = "https"
		u.Host = "app.basecamp.com"
		u.Path = strings.TrimSuffix(u.Path, ".json")
		u.RawQuery = ""
		u.Fragment = ""
	}
	return u.String()
}

func firstInt64(items []int64) int64 {
	if len(items) == 0 {
		return 0
	}
	return items[0]
}

func (s *Server) writeJobStatus(job Job, state string, pgid int, prURL, message string) {
	if job.JobDir == "" {
		return
	}
	_ = os.MkdirAll(job.JobDir, 0700)
	st, _ := s.readStatus(job.JobID)
	if st.StartedAt == "" {
		st.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	st.ID, st.State, st.Agent, st.EventID, st.Target = job.JobID, state, job.Agent, job.Event.ID, job.Target
	st.Attempt = job.Attempt
	st.Repo, st.Worktree, st.Branch, st.Base = job.Repo, job.Worktree, job.Branch, job.BaseBranch
	if pgid != 0 {
		st.PGID = pgid
	}
	if prURL != "" {
		st.PRURL = prURL
	}
	if message != "" {
		st.Message = message
	}
	if state == "completed" || state == "failed" || state == "stopped" {
		st.EndedAt = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(job.JobDir, "status.json"), b, 0600)
		s.notifyOps()
	}
}

func (s *Server) readStatus(id string) (JobStatus, error) {
	var st JobStatus
	b, err := os.ReadFile(filepath.Join(s.jobsRoot(), id, "status.json"))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if isActiveJobState(st.State) && st.PGID > 0 && !processGroupAlive(st.PGID) {
		st.State = "stopped"
		if st.EndedAt == "" {
			st.EndedAt = time.Now().UTC().Format(time.RFC3339)
		}
		if st.Message == "" {
			st.Message = "Job process is no longer running. It may have been stopped by an operator or service restart."
		}
	}
	return st, nil
}

func isActiveJobState(state string) bool {
	return state == "preparing" || state == "running" || state == "publishing"
}

func isCancellableJobState(state string) bool {
	return state == "failed" || state == "stopped"
}

func processGroupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

// supersededTarget reports whether some other job for the same Basecamp record
// started later than this one.
func supersededTarget(st JobStatus, all []JobStatus) bool {
	if st.Target == "" {
		return false
	}
	for _, other := range all {
		if other.ID != st.ID && other.Target == st.Target && other.StartedAt > st.StartedAt {
			return true
		}
	}
	return false
}

// landedPR returns the pull request for a job's branch, in any state, or "" if
// the work never reached GitHub. It is how a run that actually succeeded is
// told apart from one that died with nothing to show.
func (s *Server) landedPR(st JobStatus) string {
	if st.Branch == "" || st.Repo == "" {
		return ""
	}
	repo, err := s.allowedRepoByName(st.Repo)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return prForBranch(ctx, repo.Path, st.Branch, "all")
}

// recoverOrphans closes out jobs whose agent died with the dispatcher. Stopping
// the service kills the running agents, and nothing else ever finishes those
// jobs: the status file would claim "running" forever, Basecamp would never
// hear back, and the card would sit in the in-progress column. Their worktrees
// are left in place, as they are for any other stopped job.
func (s *Server) recoverOrphans() {
	entries, err := os.ReadDir(s.jobsRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.jobsRoot(), e.Name(), "status.json"))
		if err != nil {
			continue
		}
		var st JobStatus
		if json.Unmarshal(b, &st) != nil || !isActiveJobState(st.State) {
			continue
		}
		if st.PGID > 0 && processGroupAlive(st.PGID) {
			continue
		}
		job, err := s.readJobRecord(e.Name())
		if err != nil {
			continue
		}
		job.JobID, job.JobDir = e.Name(), filepath.Join(s.jobsRoot(), e.Name())
		// The agent may well have finished and had its branch published before
		// the process went away. A pull request for its branch is proof the
		// work landed, so report that rather than calling the run stopped.
		state := "stopped"
		reason := "the dispatcher restarted while this job was still running, so the agent was killed before it finished"
		prURL := s.landedPR(st)
		if prURL != "" {
			state = "completed"
			reason = "The agent finished and its pull request was opened; the dispatcher restarted before it could report back."
		}
		s.writeJobStatus(job, state, 0, prURL, reason)
		log.Printf("recovered orphaned job=%s agent=%s as=%s", job.JobID, job.Agent, state)
		// Only the newest job for a record may move its card. An older orphan
		// has since been superseded, and its card already reflects whatever
		// that later run decided; dragging it back would undo real progress.
		if !supersededTarget(st, s.readStatuses()) {
			column := s.cfg.Cards.Failed
			if state == "completed" {
				column = s.cfg.Cards.Done
				if prURL != "" {
					column = s.cfg.Cards.PROpen
				}
			}
			go s.moveCard(job.Event, job.Target, column, job.Profile)
		}
		msg := failureMessage(job, state, reason, "")
		if state == "completed" {
			msg = fmt.Sprintf("%s opened a PR for review: %s", job.Agent, prURL)
		}
		go func(job Job, msg string) {
			var err error
			if job.ChatRoom != 0 {
				err = s.chatPost(job, msg)
			} else {
				err = s.comment(job.Event, job.Target, msg, job.Profile)
			}
			if err != nil {
				log.Printf("orphan notice failed job=%s: %v", job.JobID, err)
			}
		}(job, msg)
	}
	s.notifyOps()
}

func (s *Server) readStatuses() []JobStatus {
	entries, _ := os.ReadDir(s.jobsRoot())
	out := []JobStatus{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := s.readStatus(e.Name())
		if err == nil && st.ID != "" {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	if s.cfg.Ops.MaxJobs > 0 && len(out) > s.cfg.Ops.MaxJobs {
		out = out[:s.cfg.Ops.MaxJobs]
	}
	return out
}

func (s *Server) opsStream(w http.ResponseWriter, r *http.Request) {
	sseHeaders(w)
	updates := s.subscribeOps()
	defer s.unsubscribeOps(updates)
	jobID := r.URL.Query().Get("job")
	project := opsProjectParam(r)
	s.sendOpsPatches(w, jobID, project)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-updates:
			s.sendOpsPatches(w, jobID, project)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

func (s *Server) opsJobDetail(w http.ResponseWriter, r *http.Request, id string) {
	back := "/ops"
	if st, err := s.readStatus(id); err == nil {
		if project := jobProjectID(st.Target); project != 0 {
			back = "/ops/projects/" + strconv.FormatInt(project, 10)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><html data-theme="night"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title><link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css"><link href="https://cdn.jsdelivr.net/npm/daisyui@5/themes.css" rel="stylesheet" type="text/css"><script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script><style>%s</style></head><body data-init="@get('/ops/stream?job=%[3]s')" class="min-h-screen bg-base-200"><main class="ops-shell"><a class="btn btn-ghost btn-sm mb-4" href="%[5]s">← jobs</a><section id="job-detail"><div class="skeleton h-64 w-full"></div></section></main><script type="module" src="/ops/datastar.js"></script><script>%[4]s</script></body></html>`, html.EscapeString(id), detailCSS(), html.EscapeString(id), outputPanelJS(), back)
}

func (s *Server) opsJobStream(w http.ResponseWriter, r *http.Request, id string) {
	s.opsStream(w, r)
}

func (s *Server) opsStopJob(w http.ResponseWriter, r *http.Request, id string) {
	st, err := s.readStatus(id)
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	s.activeMu.Lock()
	cancel := s.active[id]
	s.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if st.PGID > 0 {
		_ = syscall.Kill(-st.PGID, syscall.SIGTERM)
	}
	s.notifyOps()
	w.Header().Set("Content-Type", "text/event-stream")
	s.sendOpsPatches(w, id, opsProjectParam(r))
}

// opsCancelJob retires a stopped or failed job for good. Cancelling is the way
// to say a run will not be picked up again, so the restart button goes away.
func (s *Server) opsCancelJob(w http.ResponseWriter, r *http.Request, id string) {
	st, err := s.readStatus(id)
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if !isCancellableJobState(st.State) {
		http.Error(w, "only a stopped or failed job can be cancelled", http.StatusConflict)
		return
	}
	job, err := s.readJobRecord(id)
	if err != nil {
		job = Job{Agent: st.Agent, Target: st.Target}
	}
	job.JobID, job.JobDir = id, filepath.Join(s.jobsRoot(), id)
	s.writeJobStatus(job, "cancelled", 0, st.PRURL, "Cancelled by an operator; this run will not be retried.")
	log.Printf("cancelled job=%s", id)
	s.notifyOps()
	w.Header().Set("Content-Type", "text/event-stream")
	s.sendOpsPatches(w, id, opsProjectParam(r))
}

func (s *Server) opsRestartJob(w http.ResponseWriter, r *http.Request, id string) {
	st, err := s.readStatus(id)
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if isActiveJobState(st.State) {
		http.Error(w, "running jobs cannot be restarted", http.StatusConflict)
		return
	}
	if st.State == "cancelled" {
		http.Error(w, "cancelled jobs cannot be restarted", http.StatusConflict)
		return
	}
	job, err := s.readJobRecord(id)
	if err != nil {
		job, err = s.reconstructJobFromStatus(r.Context(), st)
		if err != nil {
			http.Error(w, "job cannot be restarted: "+err.Error(), http.StatusConflict)
			return
		}
	}
	attempt := job.Attempt
	if attempt < 1 {
		attempt = 1
	}
	job.Attempt = attempt + 1
	job.PreviousJobID = id
	job.PreviousSummary = fmt.Sprintf("state=%s started=%s ended=%s pr=%s message=%s", st.State, st.StartedAt, st.EndedAt, st.PRURL, st.Message)
	job.JobID, job.JobDir = "", ""
	job.Repo, job.Worktree, job.Branch, job.BaseBranch = "", "", "", ""
	job.LocalBranch, job.Upstream, job.ExistingPR = "", "", ""
	select {
	case s.jobs <- job:
		s.notifyOps()
		w.Header().Set("Content-Type", "text/event-stream")
		s.sendOpsPatches(w, id, opsProjectParam(r))
	default:
		http.Error(w, "job queue full", http.StatusServiceUnavailable)
	}
}

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

func (s *Server) subscribeOps() chan struct{} {
	ch := make(chan struct{}, 1)
	s.opsMu.Lock()
	s.opsSubs[ch] = struct{}{}
	s.opsMu.Unlock()
	return ch
}

func (s *Server) unsubscribeOps(ch chan struct{}) {
	s.opsMu.Lock()
	delete(s.opsSubs, ch)
	close(ch)
	s.opsMu.Unlock()
}

func (s *Server) notifyOps() {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	for ch := range s.opsSubs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) sendOpsPatches(w io.Writer, jobID string, project int64) {
	sendDatastarPatch(w, s.renderJobsTable(project))
	if jobID != "" {
		sendDatastarPatch(w, s.renderJobDetail(jobID))
	}
}

func sendDatastarPatch(w io.Writer, elements string) {
	_, _ = io.WriteString(w, "event: datastar-patch-elements\n")
	for _, line := range strings.Split(elements, "\n") {
		_, _ = fmt.Fprintf(w, "data: elements %s\n", line)
	}
	_, _ = io.WriteString(w, "\n")
}

func (s *Server) renderJobsTable(project int64) string {
	jobs := s.readStatuses()
	if project != 0 {
		filtered := jobs[:0:0]
		for _, st := range jobs {
			if jobProjectID(st.Target) == project {
				filtered = append(filtered, st)
			}
		}
		jobs = filtered
	}
	var active, completed, attention int
	for _, st := range jobs {
		switch {
		case isActiveJobState(st.State):
			active++
		case st.State == "completed":
			completed++
		case st.State == "failed" || st.State == "stopped":
			attention++
		}
	}
	var b strings.Builder
	b.WriteString(`<section id="jobs" class="space-y-5">`)
	b.WriteString(`<div class="grid grid-cols-2 gap-3 md:grid-cols-4"><div class="stat rounded-box border border-base-300 bg-base-100 shadow"><div class="stat-title">Total jobs</div><div class="stat-value">` + strconv.Itoa(len(jobs)) + `</div><div class="stat-desc">retained runs</div></div><div class="stat rounded-box border border-base-300 bg-base-100 shadow"><div class="stat-title">Active</div><div class="stat-value text-info">` + strconv.Itoa(active) + `</div><div class="stat-desc">preparing / running</div></div><div class="stat rounded-box border border-base-300 bg-base-100 shadow"><div class="stat-title">Completed</div><div class="stat-value text-success">` + strconv.Itoa(completed) + `</div><div class="stat-desc">PRs and clean exits</div></div><div class="stat rounded-box border border-base-300 bg-base-100 shadow"><div class="stat-title">Needs attention</div><div class="stat-value text-warning">` + strconv.Itoa(attention) + `</div><div class="stat-desc">failed or stopped</div></div></div>`)
	if len(jobs) == 0 {
		b.WriteString(`<div class="card border border-dashed border-base-300 bg-base-100 shadow"><div class="card-body items-center py-16 text-center"><div class="text-5xl">☕</div><h2 class="card-title">No agent jobs yet</h2><p class="max-w-lg opacity-70">When an allowed Basecamp event asks an agent to work, the run will appear here with live logs and actions.</p></div></div></section>`)
		return b.String()
	}
	b.WriteString(`<section class="card border border-base-300 bg-base-100 shadow-xl"><div class="card-body p-0"><div class="flex items-center justify-between gap-3 border-b border-base-300 px-4 py-3"><h2 class="font-semibold">Recent runs</h2><span class="badge badge-info badge-outline"><span class="status status-info"></span> streaming</span></div><div class="overflow-x-auto"><table class="table table-zebra"><thead><tr><th>State</th><th>Job</th><th class="hidden md:table-cell">Repo</th><th class="hidden lg:table-cell">Started</th><th class="hidden lg:table-cell">Finished</th><th>Links</th></tr></thead><tbody>`)
	for _, st := range jobs {
		stateLabel, badgeClass := statusPresentation(st.State)
		b.WriteString(`<tr><td><span class="badge ` + badgeClass + ` badge-outline gap-1"><span class="status ` + statusDotClass(st.State) + `"></span>` + html.EscapeString(stateLabel) + `</span></td><td><a class="link link-primary font-mono font-semibold" href="/ops/jobs/` + html.EscapeString(st.ID) + `">` + html.EscapeString(st.ID) + `</a><br><span class="text-xs opacity-60">` + html.EscapeString(st.Agent) + ` · event ` + strconv.FormatInt(st.EventID, 10) + ` · attempt ` + strconv.Itoa(st.Attempt) + `</span>` +
			// Narrow screens drop the columns below; fold their content in here
			// so nothing is lost when they are hidden.
			`<span class="mt-1 block text-xs opacity-70 md:hidden">` + html.EscapeString(emptyDash(st.Repo)) + ` · <span class="font-mono">` + html.EscapeString(emptyDash(st.Branch)) + `</span></span>` +
			`<span class="mt-0.5 block text-xs opacity-60 lg:hidden">` + html.EscapeString(st.StartedAt) + ` → ` + html.EscapeString(emptyDash(st.EndedAt)) + `</span>` +
			`</td><td class="hidden md:table-cell"><span class="font-medium">` + html.EscapeString(emptyDash(st.Repo)) + `</span><br><span class="text-xs opacity-60 font-mono">` + html.EscapeString(emptyDash(st.Branch)) + `</span></td><td class="hidden lg:table-cell"><span class="text-xs whitespace-nowrap">` + html.EscapeString(st.StartedAt) + `</span></td><td class="hidden lg:table-cell"><span class="text-xs whitespace-nowrap">` + html.EscapeString(emptyDash(st.EndedAt)) + `</span></td><td><div class="join join-vertical sm:join-horizontal">`)
		if st.Target != "" {
			b.WriteString(`<a class="btn join-item btn-xs btn-outline" target="_blank" rel="noopener noreferrer" href="` + html.EscapeString(basecampAppURL(st.Target)) + `">Basecamp</a>`)
		}
		if st.PRURL != "" {
			b.WriteString(`<a class="btn join-item btn-xs btn-primary" target="_blank" rel="noopener noreferrer" href="` + html.EscapeString(st.PRURL) + `">PR</a>`)
		}
		if st.State == "failed" || st.State == "stopped" {
			b.WriteString(`<button class="btn join-item btn-xs btn-warning" data-on:click="@post('/ops/jobs/` + html.EscapeString(st.ID) + `/restart` + projectQuery(project) + `')">Restart</button>`)
		}
		if isCancellableJobState(st.State) {
			b.WriteString(`<button class="btn join-item btn-xs btn-outline btn-error" data-on:click="@post('/ops/jobs/` + html.EscapeString(st.ID) + `/cancel` + projectQuery(project) + `')">Cancel</button>`)
		}
		b.WriteString(`</div></td></tr>`)
	}
	b.WriteString(`</tbody></table></div></div></section></section>`)
	return b.String()
}

func (s *Server) renderJobDetail(id string) string {
	st, err := s.readStatus(id)
	if err != nil {
		return `<p>Job not found.</p>`
	}
	output := html.EscapeString(s.jobOutput(id))
	stateLabel, badgeClass := statusPresentation(st.State)
	var b strings.Builder
	b.WriteString(`<section id="job-detail" class="space-y-5"><div class="card border border-base-300 bg-base-100 shadow-xl"><div class="card-body"><div class="flex flex-wrap items-start justify-between gap-3"><div><div class="mb-2 inline-flex items-center gap-2 rounded-full bg-base-200 px-3 py-1 text-xs uppercase tracking-[0.2em] opacity-80">` + html.EscapeString(st.Agent) + ` agent</div><h1 class="card-title font-mono break-all text-xl md:text-2xl">` + html.EscapeString(st.ID) + `</h1><p class="text-sm opacity-70">event ` + strconv.FormatInt(st.EventID, 10) + ` · attempt ` + strconv.Itoa(st.Attempt) + ` · pgid ` + strconv.Itoa(st.PGID) + `</p></div><span class="badge ` + badgeClass + ` badge-outline gap-1"><span class="status ` + statusDotClass(st.State) + `"></span>` + html.EscapeString(stateLabel) + `</span></div><div class="card-actions mt-4">`)
	if isActiveJobState(st.State) {
		b.WriteString(`<button class="btn btn-error btn-sm" data-on:click="@post('/ops/jobs/` + html.EscapeString(st.ID) + `/stop` + `')">Stop job</button>`)
	} else if st.State == "failed" || st.State == "stopped" {
		b.WriteString(`<button class="btn btn-warning btn-sm" data-on:click="@post('/ops/jobs/` + html.EscapeString(st.ID) + `/restart` + `')">Restart as fresh attempt</button>`)
	}
	if isCancellableJobState(st.State) {
		b.WriteString(`<button class="btn btn-outline btn-error btn-sm" data-on:click="@post('/ops/jobs/` + html.EscapeString(st.ID) + `/cancel` + `')">Cancel</button>`)
	}
	if st.Target != "" {
		b.WriteString(`<a class="btn btn-outline btn-sm" target="_blank" rel="noopener noreferrer" href="` + html.EscapeString(basecampAppURL(st.Target)) + `">Basecamp trigger</a>`)
	}
	if st.PRURL != "" {
		b.WriteString(`<a class="btn btn-primary btn-sm" target="_blank" rel="noopener noreferrer" href="` + html.EscapeString(st.PRURL) + `">PR</a>`)
	}
	b.WriteString(`</div></div></div>`)
	b.WriteString(`<div class="grid grid-cols-2 gap-3 md:grid-cols-4"><div class="stat rounded-box border border-base-300 bg-base-100 shadow"><div class="stat-title">Started</div><div class="stat-value text-sm">` + html.EscapeString(emptyDash(st.StartedAt)) + `</div></div><div class="stat rounded-box border border-base-300 bg-base-100 shadow"><div class="stat-title">Finished</div><div class="stat-value text-sm">` + html.EscapeString(emptyDash(st.EndedAt)) + `</div></div><div class="stat rounded-box border border-base-300 bg-base-100 shadow"><div class="stat-title">Repo</div><div class="stat-value text-sm font-mono">` + html.EscapeString(emptyDash(st.Repo)) + `</div></div><div class="stat rounded-box border border-base-300 bg-base-100 shadow"><div class="stat-title">Base</div><div class="stat-value text-sm font-mono">` + html.EscapeString(emptyDash(st.Base)) + `</div></div></div>`)
	b.WriteString(`<div class="card border border-base-300 bg-base-100 shadow"><div class="card-body"><dl class="grid gap-4 md:grid-cols-2"><div><dt class="text-xs uppercase tracking-wide opacity-60">Branch</dt><dd class="font-mono break-all">` + html.EscapeString(emptyDash(st.Branch)) + `</dd></div><div><dt class="text-xs uppercase tracking-wide opacity-60">Worktree</dt><dd class="font-mono text-xs break-all">` + html.EscapeString(emptyDash(st.Worktree)) + `</dd></div></dl></div></div>`)
	b.WriteString(`<section class="card border border-base-300 bg-base-100 shadow"><div class="card-body"><div class="flex items-center justify-between gap-3"><h2 class="card-title">Output</h2><button type="button" class="btn btn-ghost btn-xs" onclick="copyJobOutput(this)">Copy</button></div><pre id="job-output" class="log-panel p-4 text-sm">` + output + `</pre></div></section>`)
	if st.Message != "" {
		b.WriteString(`<section class="card border border-base-300 bg-base-100 shadow"><div class="card-body"><h2 class="card-title">Final message</h2><pre class="log-panel p-4 text-sm">` + html.EscapeString(st.Message) + `</pre></div></section>`)
	}
	b.WriteString(`</section>`)
	return b.String()
}

// jobOutput returns the tail of a run's log. Jobs recorded before the streams
// were merged kept a file per stream, so fall back to those and show them in
// the one panel rather than losing their history.
func (s *Server) jobOutput(id string) string {
	dir := filepath.Join(s.jobsRoot(), id)
	limit := s.cfg.Ops.LogTailBytes
	if out := tailFile(filepath.Join(dir, "output.log"), limit); out != "" {
		return out
	}
	parts := []string{}
	for _, legacy := range []string{"stdout.log", "stderr.log"} {
		if out := tailFile(filepath.Join(dir, legacy), limit); out != "" {
			parts = append(parts, out)
		}
	}
	return strings.Join(parts, "\n")
}

func tailFile(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if info.Size() > max {
		start = info.Size() - max
	}
	_, _ = f.Seek(start, io.SeekStart)
	b, _ := io.ReadAll(f)
	if start > 0 {
		return "[tail truncated]\n" + string(b)
	}
	return string(b)
}

func outputPanelJS() string {
	return `(()=>{let panel=null;let outputObserver=null;let following=true;const nearBottom=el=>el.scrollHeight-el.scrollTop-el.clientHeight<32;const scrollToBottom=()=>{if(panel&&following)requestAnimationFrame(()=>{if(panel&&following)panel.scrollTop=panel.scrollHeight})};const bind=()=>{const next=document.getElementById('job-output');if(next===panel)return;if(outputObserver)outputObserver.disconnect();panel=next;if(!panel)return;panel.addEventListener('scroll',()=>{following=nearBottom(panel)},{passive:true});outputObserver=new MutationObserver(scrollToBottom);outputObserver.observe(panel,{childList:true,subtree:true,characterData:true});scrollToBottom()};new MutationObserver(bind).observe(document.body,{childList:true,subtree:true});bind();window.copyJobOutput=async button=>{const text=document.getElementById('job-output')?.textContent||'';try{await navigator.clipboard.writeText(text)}catch(_){const area=document.createElement('textarea');area.value=text;area.style.position='fixed';area.style.opacity='0';document.body.appendChild(area);area.select();document.execCommand('copy');area.remove()}const old=button.textContent;button.textContent='Copied';setTimeout(()=>{button.textContent=old},1200)}})();`
}

func detailCSS() string {
	return `.ops-shell{max-width:1400px;margin:0 auto;padding:1.5rem}.log-panel{white-space:pre-wrap;word-break:break-word;max-height:70vh;overflow:auto;border-radius:var(--radius-box);background:var(--color-base-200);font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace}@media(max-width:900px){.ops-shell{padding:.75rem}}`
}

func statusPresentation(state string) (string, string) {
	switch state {
	case "completed":
		return "done", "badge-success"
	case "failed":
		return "failed", "badge-error"
	case "stopped":
		return "stopped", "badge-warning"
	case "cancelled":
		return "cancelled", "badge-neutral"
	case "preparing":
		return state, "badge-secondary"
	case "running":
		return state, "badge-info"
	case "publishing":
		return state, "badge-primary"
	default:
		return state, "badge-neutral"
	}
}

func statusDotClass(state string) string {
	switch state {
	case "completed":
		return "status-success"
	case "failed":
		return "status-error"
	case "stopped":
		return "status-warning"
	case "cancelled":
		return "status-neutral"
	case "preparing":
		return "status-secondary animate-pulse"
	case "running":
		return "status-info animate-pulse"
	case "publishing":
		return "status-primary animate-pulse"
	default:
		return "status-neutral"
	}
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

var bucketIDPattern = regexp.MustCompile(`/buckets/(\d+)/`)

// jobProjectID returns the Basecamp project a job belongs to, read from its
// target recording URL, or 0 when the target has none.
func jobProjectID(target string) int64 {
	m := bucketIDPattern.FindStringSubmatch(target)
	if m == nil {
		return 0
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

// opsProjectParam reads the ?project= filter that project pages pass to their
// stream and actions.
func opsProjectParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.URL.Query().Get("project"), 10, 64)
	return id
}

func projectQuery(project int64) string {
	if project == 0 {
		return ""
	}
	return "?project=" + strconv.FormatInt(project, 10)
}

// opsProjectNav links the all-jobs page to each configured project's page.
func (s *Server) opsProjectNav() string {
	if len(s.cfg.AllowedProjectIDs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<nav class="mt-1 flex flex-wrap gap-3 text-sm">`)
	for _, project := range s.cfg.AllowedProjectIDs {
		id := strconv.FormatInt(project, 10)
		label := "Project " + id
		if repo := s.cfg.ProjectRepos[id]; repo != "" {
			label += " · " + repo
		}
		b.WriteString(`<a class="link link-primary" href="/ops/projects/` + id + `">` + html.EscapeString(label) + `</a>`)
	}
	b.WriteString(`</nav>`)
	return b.String()
}
