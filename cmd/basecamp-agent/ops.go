package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/a-h/templ"
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
	// RetriedAs is the attempt a restart of this job started.
	RetriedAs string `json:"retried_as,omitempty"`
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
	s.renderOpsPage(w, r, 0)
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
		s.renderOpsPage(w, r, project)
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

// retriedAs names the attempt that replaced a job, or "" while the job is
// still the newest attempt of its event. Jobs restarted before RetriedAs was
// recorded are matched by their later attempts.
func retriedAs(st JobStatus, all []JobStatus) string {
	if st.RetriedAs != "" {
		return st.RetriedAs
	}
	newest, attempt := "", max(st.Attempt, 1)
	for _, other := range all {
		if other.EventID == st.EventID && other.Agent == st.Agent && max(other.Attempt, 1) > attempt {
			newest, attempt = other.ID, max(other.Attempt, 1)
		}
	}
	return newest
}

// needsAttention reports a failed or stopped job that nobody has restarted or
// dismissed yet. Only such a job offers Restart and Dismiss.
func needsAttention(st JobStatus, all []JobStatus) bool {
	return isCancellableJobState(st.State) && retriedAs(st, all) == ""
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
	s.sendActionPatches(w, r, id)
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
		http.Error(w, "only a stopped or failed job can be dismissed", http.StatusConflict)
		return
	}
	if next := retriedAs(st, s.readStatuses()); next != "" {
		http.Error(w, "job was already restarted as "+next, http.StatusConflict)
		return
	}
	job, err := s.readJobRecord(id)
	if err != nil {
		job = Job{Agent: st.Agent, Target: st.Target}
	}
	job.JobID, job.JobDir = id, filepath.Join(s.jobsRoot(), id)
	s.writeJobStatus(job, "cancelled", 0, st.PRURL, "Dismissed by an operator; this run will not be retried.")
	log.Printf("dismissed job=%s", id)
	s.notifyOps()
	w.Header().Set("Content-Type", "text/event-stream")
	s.sendActionPatches(w, r, id)
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
		http.Error(w, "dismissed jobs cannot be restarted", http.StatusConflict)
		return
	}
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if next := retriedAs(st, s.readStatuses()); next != "" {
		http.Error(w, "job was already restarted as "+next, http.StatusConflict)
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
		// Record the retry on the old job at once: the new attempt writes its
		// own status only when a worker picks it up.
		st.RetriedAs = jobID(job)
		if b, err := json.MarshalIndent(st, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(s.jobsRoot(), id, "status.json"), b, 0600)
		}
		s.notifyOps()
		w.Header().Set("Content-Type", "text/event-stream")
		s.sendActionPatches(w, r, id)
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

// sendOpsPatches patches what the requesting page shows: a job page shows one
// job's detail, every other page the jobs section.
func (s *Server) sendOpsPatches(w io.Writer, jobID string, project int64) {
	if jobID != "" {
		sendDatastarPatch(w, s.renderJobDetail(jobID))
		sendDatastarSignals(w, map[string]any{"output": s.jobOutput(jobID)})
		return
	}
	sendDatastarPatch(w, s.renderJobsTable(project))
}

// sendActionPatches answers an action posted from a job page or a jobs list.
func (s *Server) sendActionPatches(w io.Writer, r *http.Request, id string) {
	if r.URL.Query().Get("view") == "detail" {
		s.sendOpsPatches(w, id, 0)
		return
	}
	s.sendOpsPatches(w, "", opsProjectParam(r))
}

func sendDatastarPatch(w io.Writer, elements string) {
	_, _ = io.WriteString(w, "event: datastar-patch-elements\n")
	for _, line := range strings.Split(elements, "\n") {
		_, _ = fmt.Fprintf(w, "data: elements %s\n", line)
	}
	_, _ = io.WriteString(w, "\n")
}

// sendDatastarSignals patches Datastar signals, such as a job's output.
func sendDatastarSignals(w io.Writer, signals map[string]any) {
	b, err := json.Marshal(signals)
	if err != nil {
		log.Printf("encode ops signals: %v", err)
		return
	}
	_, _ = fmt.Fprintf(w, "event: datastar-patch-signals\ndata: signals %s\n\n", b)
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
		return "bg-neutral-content"
	case "preparing":
		return "status-secondary animate-pulse"
	case "running":
		return "status-info animate-pulse"
	case "publishing":
		return "status-primary animate-pulse"
	default:
		return "bg-neutral-content"
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

// projectLabel names a project by its repository, falling back to its ID.
func (s *Server) projectLabel(project int64) string {
	if repo := s.cfg.ProjectRepos[strconv.FormatInt(project, 10)]; repo != "" {
		return repo
	}
	return "Project " + strconv.FormatInt(project, 10)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func (s *Server) renderOpsPage(w http.ResponseWriter, r *http.Request, project int64) {
	v := opsPageView{Title: "Basecamp Agent Jobs", Project: project, StreamURL: "/ops/stream" + projectQuery(project), Menu: s.projectMenuItems(project)}
	if project != 0 {
		v.Title = s.projectLabel(project)
		v.BasecampURL = fmt.Sprintf("https://app.basecamp.com/%d/projects/%d", s.cfg.AllowedAccountID, project)
	}
	renderPage(w, r, opsIndexPage(v))
}

func (s *Server) opsJobDetail(w http.ResponseWriter, r *http.Request, id string) {
	back := "/ops"
	if st, err := s.readStatus(id); err == nil {
		if project := jobProjectID(st.Target); project != 0 {
			back = "/ops/projects/" + strconv.FormatInt(project, 10)
		}
	}
	renderPage(w, r, jobPage(id, back, "/ops/stream?job="+url.QueryEscape(id)))
}

func renderPage(w http.ResponseWriter, r *http.Request, page templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Render(r.Context(), w); err != nil {
		log.Printf("render ops page: %v", err)
	}
}

// renderFragment renders a component for a Datastar patch.
func renderFragment(c templ.Component) string {
	var b bytes.Buffer
	if err := c.Render(context.Background(), &b); err != nil {
		log.Printf("render ops fragment: %v", err)
	}
	return b.String()
}

func (s *Server) renderJobsTable(project int64) string {
	v := jobsView{Project: project, RetriedAs: map[string]string{}}
	all := s.readStatuses()
	for _, st := range all {
		if project != 0 && jobProjectID(st.Target) != project {
			continue
		}
		v.Jobs = append(v.Jobs, st)
		if next := retriedAs(st, all); next != "" {
			v.RetriedAs[st.ID] = next
		}
		switch {
		case isActiveJobState(st.State):
			v.Active++
		case st.State == "completed":
			v.Completed++
		case needsAttention(st, all):
			v.Attention++
		}
	}
	return renderFragment(jobsSection(v))
}

func (s *Server) renderJobDetail(id string) string {
	st, err := s.readStatus(id)
	if err != nil {
		return renderFragment(jobDetail(jobDetailView{}))
	}
	return renderFragment(jobDetail(jobDetailView{Found: true, Status: st, RetriedAs: retriedAs(st, s.readStatuses())}))
}

// projectMenuItems lists all jobs and each configured project with job
// counts, marking the current page.
func (s *Server) projectMenuItems(current int64) []projectMenuItem {
	if len(s.cfg.AllowedProjectIDs) == 0 {
		return nil
	}
	total, running := map[int64]int{}, map[int64]int{}
	for _, st := range s.readStatuses() {
		project := jobProjectID(st.Target)
		total[project]++
		total[0]++
		if isActiveJobState(st.State) {
			running[project]++
			running[0]++
		}
	}
	items := []projectMenuItem{{Href: "/ops", Label: "All projects", Active: current == 0, Jobs: total[0], Running: running[0]}}
	for _, project := range s.cfg.AllowedProjectIDs {
		id := strconv.FormatInt(project, 10)
		items = append(items, projectMenuItem{Href: "/ops/projects/" + id, Label: s.projectLabel(project), ID: id, Active: current == project, Jobs: total[project], Running: running[project]})
	}
	return items
}

func datastarGet(url string) string  { return "@get('" + url + "')" }
func datastarPost(url string) string { return "@post('" + url + "')" }

// jobActionURL is where a job page (detail) or a jobs list posts an action.
func jobActionURL(id, action string, project int64, detail bool) string {
	u := "/ops/jobs/" + url.PathEscape(id) + "/" + action
	if detail {
		return u + "?view=detail"
	}
	return u + projectQuery(project)
}

func stateLabel(state string) string {
	label, _ := statusPresentation(state)
	return label
}

// stateBadgeClass outlines state badges, except neutral ones: an outlined
// neutral badge is nearly invisible on the dark theme, so it is drawn solid.
func stateBadgeClass(state string) string {
	_, class := statusPresentation(state)
	if class == "badge-neutral" {
		return class
	}
	return class + " badge-outline"
}

// recoverLandedFailures re-checks failed jobs whose work landed anyway:
// reporting can fail after the branch was pushed and the pull request opened.
// A job counts as landed only when its own commit is the pushed branch head
// and that branch has a pull request, so an earlier attempt's PR on the same
// branch never vouches for a run that failed before pushing.
func (s *Server) recoverLandedFailures() {
	statuses := s.readStatuses()
	recovered := false
	for _, st := range statuses {
		if st.State != "failed" || st.PRURL != "" || st.Worktree == "" || st.Branch == "" || supersededTarget(st, statuses) {
			continue
		}
		if !s.branchHeadIsWorktreeHead(st) {
			continue
		}
		prURL := s.landedPR(st)
		if prURL == "" {
			continue
		}
		job, err := s.readJobRecord(st.ID)
		if err != nil {
			continue
		}
		job.JobID, job.JobDir = st.ID, filepath.Join(s.jobsRoot(), st.ID)
		job.Repo, job.Worktree, job.Branch, job.BaseBranch = st.Repo, st.Worktree, st.Branch, st.Base
		s.writeJobStatus(job, "completed", 0, prURL, "The agent finished and its pull request was opened; reporting the pull request failed at the time.")
		log.Printf("recovered failed job=%s agent=%s pr=%s", job.JobID, job.Agent, prURL)
		recovered = true
		go s.moveCard(job.Event, job.Target, s.cfg.Cards.PROpen, job.Profile)
		go func(job Job, msg string) {
			var err error
			if job.ChatRoom != 0 {
				err = s.chatPost(job, msg)
			} else {
				err = s.comment(job.Event, job.Target, msg, job.Profile)
			}
			if err != nil {
				log.Printf("recovery notice failed job=%s: %v", job.JobID, err)
			}
		}(job, fmt.Sprintf("%s opened a PR for review: %s", job.Agent, prURL))
	}
	if recovered {
		s.notifyOps()
	}
}

// branchHeadIsWorktreeHead reports whether the job's worktree commit is what
// the remote branch points at, which proves this run pushed its work.
func (s *Server) branchHeadIsWorktreeHead(st JobStatus) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	head, err := gitCommand(ctx, st.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return false
	}
	remote, err := gitCommand(ctx, st.Worktree, "ls-remote", "origin", "refs/heads/"+st.Branch)
	if err != nil {
		return false
	}
	fields := strings.Fields(remote)
	return len(fields) > 0 && fields[0] == strings.TrimSpace(head)
}
