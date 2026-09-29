package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectRepo(t *testing.T) {
	s := &Server{cfg: Config{WorkDir: "/home/tester/Projects", AllowedRepos: AllowedRepoList{{Name: "restaurant", Path: "/home/tester/Projects/restaurant", Aliases: []string{"foodapp"}}, {Name: "bandcash", Path: "/srv/bandcash"}}}}
	cases := []struct {
		text, title, want string
		wantErr           bool
	}{
		{"Fix autocomplete", "Re: Restaurant", "restaurant", false},
		{"Look at ~/Projects/restaurant", "", "restaurant", false},
		{"Fix checkout", "foodapp", "restaurant", false},
		{"Create a todo", "My Project", "", false},
		{"Move Bandcash into Restaurant", "", "", true},
		{"Restaurante is not Restaurant", "", "restaurant", false},
	}
	for _, tc := range cases {
		got, err := s.selectRepo(Job{Instruction: tc.text, Title: tc.title})
		if got.Name != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("selectRepo(%q,%q)=%q,%v; want %q error=%v", tc.text, tc.title, got.Name, err, tc.want, tc.wantErr)
		}
	}
}

func TestProjectRepoMappingSelectsDefaultWhenNoRepoMentioned(t *testing.T) {
	s := &Server{cfg: Config{AllowedRepos: AllowedRepoList{{Name: "restaurant", Path: "/home/tester/Projects/restaurant"}}, ProjectRepos: map[string]string{"23456789": "restaurant"}}}
	job := Job{Instruction: "deploy to railway"}
	job.Event.Recording.Bucket.ID = 23456789
	got, err := s.selectRepo(job)
	if err != nil || got.Name != "restaurant" {
		t.Fatalf("project mapping selection: %+v, %v", got, err)
	}
}

func TestAllowedReposConfigRequiresObjectsWithPaths(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"work_dir":"/srv","allowed_repos":[{"name":"api","path":"/opt/repos/api","aliases":["backend"]}]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg}
	repos, err := s.normalizedAllowedRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "api" || repos[0].Path != "/opt/repos/api" {
		t.Fatalf("unexpected repos: %+v", repos)
	}
	got, err := s.selectRepo(Job{Instruction: "please inspect backend"})
	if err != nil || got.Name != "api" {
		t.Fatalf("alias selection: %+v, %v", got, err)
	}

	var legacy Config
	if err := json.Unmarshal([]byte(`{"allowed_repos":["legacy"]}`), &legacy); err == nil {
		t.Fatal("legacy string allowed_repos entry should fail")
	}
}

func TestRecordingContextTitle(t *testing.T) {
	data := map[string]any{"parent": map[string]any{"title": "Restaurant"}}
	if got := recordingContextTitle(data, "Re: task"); got != "Re: task Restaurant" {
		t.Fatal(got)
	}
}

// Optional non-destructive test: create a worktree from origin's default branch
// and verify that an unchanged worktree is removed. Does not push or open a PR.
func TestPrepareWorktreeLive(t *testing.T) {
	if os.Getenv("BASECAMP_WORKTREE_TEST") != "1" {
		t.Skip("set BASECAMP_WORKTREE_TEST=1 for local Restaurant integration test")
	}
	root := t.TempDir()
	s := &Server{cfg: Config{WorkDir: "/home/tester/Projects", WorktreeRoot: root, AllowedRepos: AllowedRepoList{{Name: "restaurant", Path: "/home/tester/Projects/restaurant"}}}}
	job, err := s.prepareWorktree(Job{Event: WebhookEvent{ID: 56789006}, Agent: "codex", Instruction: "Restaurant test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(job.Worktree); err == nil {
			t.Logf("worktree left for manual inspection: %s", job.Worktree)
		}
	})
	if !strings.HasPrefix(job.Worktree, filepath.Join(root, "restaurant", "codex")) || job.BaseBranch != "master" {
		t.Fatalf("unexpected worktree: %+v", job)
	}
	probe := filepath.Join(job.Worktree, "agent-test.txt")
	if err := os.WriteFile(probe, []byte("uncommitted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.publishWorktree(job); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty worktree should not publish: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	if url, err := s.publishWorktree(job); err != nil || url != "" {
		t.Fatalf("no-change cleanup: %q, %v", url, err)
	}
	if _, err := os.Stat(job.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed: %v", err)
	}
}

func TestCardBranchGroupsFollowUpsOnOneBranch(t *testing.T) {
	card := "https://3.basecampapi.com/1234567/buckets/23456789/card_tables/cards/45678003.json"
	first := cardBranch(card, 56789004)
	second := cardBranch(card, 56789005)
	if first != "bc-card-45678003" {
		t.Fatalf("cardBranch = %q", first)
	}
	if first != second {
		t.Errorf("two events on one card gave different branches: %q vs %q", first, second)
	}
	other := cardBranch("https://3.basecampapi.com/1234567/buckets/23456789/card_tables/cards/999.json", 1)
	if other == first {
		t.Error("different cards must not share a branch")
	}
	chat := cardBranch("", 56789005)
	if chat != "bc-56789005" {
		t.Errorf("a target without a record id should fall back to the event: %q", chat)
	}
}

func TestPreviewPathsFromCommitTrailers(t *testing.T) {
	got := parsePreviewPaths("/menu\n/news\n\n/menu\nnot-a-route\n/news items\n/\n")
	want := []string{"/menu", "/news", "/"}
	if len(got) != len(want) {
		t.Fatalf("parsePreviewPaths = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path %d = %q, want %q", i, got[i], want[i])
		}
	}
	if parsePreviewPaths("") != nil {
		t.Error("no trailers should yield no paths")
	}
}

func TestBulletListCapsLongLists(t *testing.T) {
	items := []string{"a", "b", "c", "d"}
	if got := bulletList(items, 10); got != "- `a`\n- `b`\n- `c`\n- `d`\n" {
		t.Errorf("short list = %q", got)
	}
	got := bulletList(items, 2)
	if got != "- `a`\n- `b`\n- …and 2 more\n" {
		t.Errorf("capped list = %q", got)
	}
}

func TestFindPRURLIgnoresWrapperNoise(t *testing.T) {
	noisy := "mise by @jdx – installing 1 tool\nmise ⇢ gh@2.101.0  124ms · already installed\nmise ~/.config/mise/config.toml tools: gh@2.101.0\nhttps://github.com/peterszarvas94/basecamp-agent-dummy/pull/1\n"
	if got := findPRURL(noisy); got != "https://github.com/peterszarvas94/basecamp-agent-dummy/pull/1" {
		t.Fatalf("findPRURL = %q", got)
	}
	if got := findPRURL("no pull request here\nhttps://github.com/o/r/issues/3"); got != "" {
		t.Fatalf("findPRURL = %q, want none", got)
	}
}

func TestBranchHeadIsWorktreeHeadRequiresThisRunsPush(t *testing.T) {
	dir := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	origin, work := filepath.Join(dir, "origin.git"), filepath.Join(dir, "work")
	run(dir, "init", "-q", "--bare", origin)
	run(dir, "clone", "-q", origin, work)
	run(work, "commit", "-q", "--allow-empty", "-m", "first")
	run(work, "push", "-q", "origin", "HEAD:refs/heads/bc-card-1")

	s := &Server{}
	st := JobStatus{Worktree: work, Branch: "bc-card-1"}
	if !s.branchHeadIsWorktreeHead(st) {
		t.Fatal("a pushed commit should count as landed")
	}
	run(work, "commit", "-q", "--allow-empty", "-m", "unpushed")
	if s.branchHeadIsWorktreeHead(st) {
		t.Fatal("an unpushed commit must not count as landed")
	}
	if s.branchHeadIsWorktreeHead(JobStatus{Worktree: work, Branch: "missing"}) {
		t.Fatal("a branch that was never pushed must not count as landed")
	}
}
