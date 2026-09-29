package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func opsAuthRequest(s *Server, method, target string, prepare func(*http.Request)) (*httptest.ResponseRecorder, bool) {
	r := httptest.NewRequest(method, target, nil)
	if prepare != nil {
		prepare(r)
	}
	w := httptest.NewRecorder()
	return w, s.authorizeOps(w, r)
}

func TestOpsQueryTokenBecomesCookieAndCleanRedirect(t *testing.T) {
	s := &Server{cfg: Config{Ops: OpsConfig{Token: "secret-token"}}}
	w, ok := opsAuthRequest(s, http.MethodGet, "/ops/jobs/42?token=secret-token&view=log", func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	if ok || w.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect, got ok=%v code=%d", ok, w.Code)
	}
	if got := w.Header().Get("Location"); got != "/ops/jobs/42?view=log" {
		t.Fatalf("redirect kept the token or lost the query: %q", got)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != opsSessionCookie || !c.HttpOnly || !c.Secure || c.Path != "/ops" || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected cookie attributes: %+v", c)
	}
	if strings.Contains(c.Value, "secret-token") || c.Value != opsSessionValue("secret-token") {
		t.Fatalf("cookie must hold the derived session value, not the token: %q", c.Value)
	}

	if _, ok := opsAuthRequest(s, http.MethodGet, "/ops/jobs/42?view=log", func(r *http.Request) { r.AddCookie(c) }); !ok {
		t.Fatal("session cookie was not accepted")
	}
}

func TestOpsAuthRejectsAndAcceptsOtherCredentials(t *testing.T) {
	s := &Server{cfg: Config{Ops: OpsConfig{Token: "secret-token"}}}
	for name, prepare := range map[string]func(*http.Request){
		"nothing":      nil,
		"wrong query":  func(r *http.Request) { r.URL.RawQuery = "token=nope" },
		"wrong cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: opsSessionCookie, Value: "nope"}) },
		"token cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: opsSessionCookie, Value: "secret-token"}) },
		"wrong bearer": func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
	} {
		if w, ok := opsAuthRequest(s, http.MethodGet, "/ops", prepare); ok || w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got ok=%v code=%d", name, ok, w.Code)
		}
	}
	if _, ok := opsAuthRequest(s, http.MethodGet, "/ops", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret-token") }); !ok {
		t.Fatal("bearer token was not accepted")
	}
	// Non-page requests with a query token proceed without a redirect.
	for _, tc := range []struct{ method, target string }{{http.MethodPost, "/ops/jobs/42/stop?token=secret-token"}, {http.MethodGet, "/ops/stream?token=secret-token"}} {
		w, ok := opsAuthRequest(s, tc.method, tc.target, nil)
		if !ok || len(w.Result().Cookies()) != 1 {
			t.Fatalf("%s %s: expected access with a session cookie, got ok=%v", tc.method, tc.target, ok)
		}
	}
	if w, _ := opsAuthRequest(s, http.MethodGet, "/ops?token=secret-token", nil); w.Result().Cookies()[0].Secure {
		t.Fatal("cookie must not be Secure over plain HTTP, or local browsers drop it")
	}
}

func TestOpsWithoutTokenIsOpen(t *testing.T) {
	if _, ok := opsAuthRequest(&Server{}, http.MethodGet, "/ops", nil); !ok {
		t.Fatal("ops without a configured token should be open")
	}
}

func TestJobProjectIDFromTarget(t *testing.T) {
	for target, want := range map[string]int64{
		"https://3.basecampapi.com/6292322/buckets/49042606/card_tables/cards/10345844978.json": 49042606,
		"https://3.basecampapi.com/6292322/buckets/7/todos/1.json":                              7,
		"": 0,
		"https://3.basecampapi.com/6292322/projects.json": 0,
	} {
		if got := jobProjectID(target); got != want {
			t.Fatalf("jobProjectID(%q) = %d, want %d", target, got, want)
		}
	}
}

func TestOpsProjectPageShowsOnlyThatProject(t *testing.T) {
	cfg := Config{
		StatePath:         t.TempDir() + "/state.json",
		AllowedProjectIDs: []int64{11, 22},
		ProjectRepos:      map[string]string{"11": "app"},
		Ops:               OpsConfig{Enabled: true},
	}
	s := &Server{cfg: cfg}
	writeTestStatus(t, s, JobStatus{ID: "job-a", State: "completed", Target: "https://3.basecampapi.com/1/buckets/11/card_tables/cards/1.json", StartedAt: "2026-09-29T10:00:00Z"})
	writeTestStatus(t, s, JobStatus{ID: "job-b", State: "completed", Target: "https://3.basecampapi.com/1/buckets/22/card_tables/cards/2.json", StartedAt: "2026-09-29T11:00:00Z"})

	all := s.renderJobsTable(0)
	if !strings.Contains(all, "job-a") || !strings.Contains(all, "job-b") {
		t.Fatalf("all-jobs table is missing a job:\n%s", all)
	}
	only := s.renderJobsTable(11)
	if !strings.Contains(only, "job-a") || strings.Contains(only, "job-b") {
		t.Fatalf("project table is not filtered:\n%s", only)
	}

	h := s.routes()
	for path, want := range map[string]int{"/ops/projects/11": http.StatusOK, "/ops/projects/nope": http.StatusNotFound, "/ops": http.StatusOK} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if path == "/ops/projects/11" && (!strings.Contains(rec.Body.String(), "/ops/stream?project=11") || !strings.Contains(rec.Body.String(), "<h1 class=\"text-2xl font-bold tracking-tight md:text-3xl\">app</h1>")) {
			t.Fatalf("project page does not stream its project or show its repo:\n%s", rec.Body.String())
		}
		if path == "/ops/projects/11" && (!strings.Contains(rec.Body.String(), `class="menu-active" href="/ops/projects/11"`) || !strings.Contains(rec.Body.String(), "1 job<")) {
			t.Fatalf("project page does not highlight its tab with its job count:\n%s", rec.Body.String())
		}
		if path == "/ops" && !strings.Contains(rec.Body.String(), `href="/ops/projects/22"`) {
			t.Fatalf("all-jobs page does not link to projects:\n%s", rec.Body.String())
		}
	}
}

func TestOpsURLCommandPrintsTokenURLs(t *testing.T) {
	config := t.TempDir() + "/config.json"
	cfg := defaultConfig()
	cfg.PublicURL = "https://agent.example.test"
	cfg.AllowedProjectIDs = []int64{11, 22}
	cfg.ProjectRepos = map[string]string{"22": "app"}
	cfg.Ops.Token = "tok"
	if err := writeConfig(config, cfg, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIForTest(t, config, "ops", "url")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"all projects: https://agent.example.test/ops?token=tok",
		"project 11: https://agent.example.test/ops/projects/11?token=tok",
		"project 22 (app): https://agent.example.test/ops/projects/22?token=tok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
	out, err = runCLIForTest(t, config, "ops", "url", "--project", "22", "--no-token")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "project 22 (app): https://agent.example.test/ops/projects/22" {
		t.Fatalf("unexpected single-project output:\n%s", out)
	}
}

func writeTestStatus(t *testing.T, s *Server, st JobStatus) {
	t.Helper()
	dir := filepath.Join(s.jobsRoot(), st.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
