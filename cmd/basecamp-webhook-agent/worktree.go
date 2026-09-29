package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func recordingContextTitle(data map[string]any, title string) string {
	parent, _ := data["parent"].(map[string]any)
	if name, _ := parent["title"].(string); name != "" {
		return title + " " + name
	}
	return title
}

func expandPath(path string) string {
	if path == "" {
		return path
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return os.ExpandEnv(path)
}

func (s *Server) normalizedAllowedRepos() ([]AllowedRepo, error) {
	repos := make([]AllowedRepo, 0, len(s.cfg.AllowedRepos))
	for _, repo := range s.cfg.AllowedRepos {
		repo.Name = strings.TrimSpace(repo.Name)
		repo.Path = strings.TrimSpace(repo.Path)
		if repo.Path == "" {
			return nil, fmt.Errorf("allowed repo %q needs a path", repo.Name)
		}
		if repo.Name == "" {
			repo.Name = filepath.Base(repo.Path)
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`).MatchString(repo.Name) {
			return nil, fmt.Errorf("invalid allowed repo name %q", repo.Name)
		}
		path := expandPath(repo.Path)
		if !filepath.IsAbs(path) {
			path = filepath.Join(expandPath(s.cfg.WorkDir), path)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("allowed repo %q path: %w", repo.Name, err)
		}
		repo.Path = abs
		repos = append(repos, repo)
	}
	return repos, nil
}

func repoMatchTerms(repo AllowedRepo) []string {
	terms := []string{repo.Name, filepath.Base(repo.Path)}
	terms = append(terms, repo.Aliases...)
	seen := map[string]bool{}
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		key := strings.ToLower(term)
		if term != "" && !seen[key] {
			seen[key] = true
			out = append(out, term)
		}
	}
	return out
}

func matchesRepoTerm(clues, term string) bool {
	pattern := `(?i)(^|[^a-z0-9])` + regexp.QuoteMeta(term) + `([^a-z0-9]|$)`
	return regexp.MustCompile(pattern).MatchString(clues)
}

// Only a single explicitly allowlisted repo may be selected. Unresolved jobs can
// still answer Basecamp questions, but their prompt forbids source changes.
func (s *Server) selectRepo(job Job) (AllowedRepo, error) {
	clues := job.Instruction + "\n" + job.Title
	repos, err := s.normalizedAllowedRepos()
	if err != nil {
		return AllowedRepo{}, err
	}
	var matches []AllowedRepo
	for _, repo := range repos {
		for _, term := range repoMatchTerms(repo) {
			if matchesRepoTerm(clues, term) {
				matches = append(matches, repo)
				break
			}
		}
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, repo := range matches {
			names = append(names, repo.Name)
		}
		return AllowedRepo{}, fmt.Errorf("multiple repositories mentioned (%s); specify one repo per job", strings.Join(names, ", "))
	}
	if len(matches) == 0 {
		if s.cfg.ProjectRepos != nil && job.Event.Recording.Bucket.ID != 0 {
			mapped := s.cfg.ProjectRepos[strconv.FormatInt(job.Event.Recording.Bucket.ID, 10)]
			for _, repo := range repos {
				if mapped == repo.Name {
					return repo, nil
				}
			}
			if mapped != "" {
				return AllowedRepo{}, fmt.Errorf("project %d maps to unknown repo %q", job.Event.Recording.Bucket.ID, mapped)
			}
		}
		return AllowedRepo{}, nil
	}
	return matches[0], nil
}

func (s *Server) allowedRepoByName(name string) (AllowedRepo, error) {
	repos, err := s.normalizedAllowedRepos()
	if err != nil {
		return AllowedRepo{}, err
	}
	for _, repo := range repos {
		if repo.Name == name {
			return repo, nil
		}
	}
	return AllowedRepo{}, fmt.Errorf("repo %q is not allowlisted", name)
}

func gitCommand(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out := &cappedOutput{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if out.truncated {
		return "", fmt.Errorf("git %s: output exceeded 64 KiB", strings.Join(args, " "))
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// previewPaths lists the routes the agent flagged with Preview-Path trailers on
// its commits. The dispatcher cannot derive an app's routes from a diff, and
// the agent already knows which pages it touched, so it declares them. Paths
// stay relative: the preview's base URL does not exist yet when the pull
// request is opened.
func previewPaths(ctx context.Context, job Job) []string {
	out, err := gitCommand(ctx, job.Worktree, "log", "--format=%(trailers:key=Preview-Path,valueonly)", "origin/"+job.BaseBranch+"..HEAD")
	if err != nil {
		return nil
	}
	return parsePreviewPaths(out)
}

func parsePreviewPaths(out string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || seen[p] || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t") {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
		if len(paths) == 20 {
			break
		}
	}
	return paths
}

// bulletList renders at most limit entries, saying how many were left out.
func bulletList(items []string, limit int) string {
	var b strings.Builder
	for i, item := range items {
		if i == limit {
			fmt.Fprintf(&b, "- …and %d more\n", len(items)-limit)
			break
		}
		fmt.Fprintf(&b, "- `%s`\n", item)
	}
	return b.String()
}

// prBody describes the change for whoever reviews it. Everything is read back
// off the branch, so it stays true however the agent worked.
func prBody(ctx context.Context, job Job) string {
	var b strings.Builder
	if asked := strings.TrimSpace(job.Instruction); asked != "" {
		lines := strings.Split(asked, "\n")
		if len(lines) > 8 {
			lines = append(lines[:8], "…")
		}
		b.WriteString("### Asked for\n\n")
		for _, line := range lines {
			b.WriteString("> " + line + "\n")
		}
		b.WriteString("\n")
	}
	span := "origin/" + job.BaseBranch + "..HEAD"
	if commits, err := gitCommand(ctx, job.Worktree, "log", "--reverse", "--format=- %s", span); err == nil && commits != "" {
		b.WriteString("### Commits\n\n" + commits + "\n\n")
	}
	if files, err := gitCommand(ctx, job.Worktree, "diff", "--name-only", span); err == nil && files != "" {
		b.WriteString("### Files\n\n" + bulletList(strings.Split(files, "\n"), 20) + "\n")
	}
	if paths := previewPaths(ctx, job); len(paths) > 0 {
		b.WriteString("### Preview\n\nThis pull request gets its own deployment; once the build finishes its URL appears below. Pages worth opening there:\n\n")
		b.WriteString(bulletList(paths, 20) + "\n")
	}
	fmt.Fprintf(&b, "---\n\nRequested in [Basecamp](%s) · agent `%s` · base `%s`\n", basecampAppURL(job.Target), job.Agent, job.BaseBranch)
	return b.String()
}

// cardBranch names the shared branch for a Basecamp record. Follow-up comments
// arrive as separate events but carry the same record, so keying on the record
// keeps one card's work on one branch, and therefore in one pull request.
// Anything without a record id (a chat line, say) keeps its per-event branch.
func cardBranch(target string, eventID int64) string {
	if _, record := idsFromBasecampURL(target); record != 0 {
		return "bc-card-" + strconv.FormatInt(record, 10)
	}
	return "bc-" + strconv.FormatInt(eventID, 10)
}

// openPRForBranch returns the URL of the open pull request whose head is
// branch, or "" when there is none. A merged or closed pull request does not
// count: that work has landed, so the next comment starts from the base again.
func openPRForBranch(ctx context.Context, repoPath, branch string) string {
	return prForBranch(ctx, repoPath, branch, "open")
}

// prForBranch returns the URL of a pull request whose head is branch, in the
// given state ("open", "merged", "closed" or "all").
func prForBranch(ctx context.Context, repoPath, branch, state string) string {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--head", branch, "--state", state, "--json", "url", "--jq", ".[0].url // empty")
	cmd.Dir = repoPath
	out := &cappedOutput{limit: 16 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return ""
	}
	url := strings.TrimSpace(out.String())
	if strings.HasPrefix(url, "https://github.com/") && strings.Contains(url, "/pull/") {
		return url
	}
	return ""
}

// freeBranch finds an unused name for a fresh start, so a card whose earlier
// pull request already merged does not try to reopen that branch.
func freeBranch(ctx context.Context, repoPath, branch string) string {
	for i := 0; i < 50; i++ {
		name := branch
		if i > 0 {
			name = fmt.Sprintf("%s-%d", branch, i+1)
		}
		_, localErr := gitCommand(ctx, repoPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
		_, remoteErr := gitCommand(ctx, repoPath, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+name)
		if localErr != nil && remoteErr != nil {
			return name
		}
	}
	return fmt.Sprintf("%s-%d", branch, time.Now().Unix())
}

func (s *Server) prepareWorktree(job Job) (Job, error) {
	repo, err := s.selectRepo(job)
	if err != nil || repo.Name == "" {
		return job, err
	}
	repoPath := repo.Path
	info, err := os.Lstat(repoPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return job, fmt.Errorf("repo %q is not a real directory: %s", repo.Name, repoPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	top, err := gitCommand(ctx, repoPath, "rev-parse", "--show-toplevel")
	if err != nil || top != repoPath {
		return job, fmt.Errorf("%q is not a repository root: %v", repo, err)
	}
	origin, err := gitCommand(ctx, repoPath, "remote", "get-url", "origin")
	if err != nil || !(strings.HasPrefix(origin, "git@github.com:") || strings.HasPrefix(origin, "https://github.com/") || strings.HasPrefix(origin, "ssh://git@github.com/")) {
		return job, fmt.Errorf("repo %q must have a GitHub origin", repo.Name)
	}
	base, err := gitCommand(ctx, repoPath, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err != nil || !strings.HasPrefix(base, "origin/") {
		return job, fmt.Errorf("repo %q needs origin/HEAD pointing to its default branch: %v", repo.Name, err)
	}
	base = strings.TrimPrefix(base, "origin/")
	if base == "" || strings.ContainsAny(base, " \t\n") {
		return job, fmt.Errorf("invalid default branch %q", base)
	}
	if _, err := gitCommand(ctx, repoPath, "fetch", "--no-tags", "origin", base); err != nil {
		return job, err
	}
	branch := cardBranch(job.Target, job.Event.ID)
	startPoint := "origin/" + base
	upstream := "origin/" + base
	existingPR := ""
	// A still-open pull request for this card means the work continues there.
	if _, err := gitCommand(ctx, repoPath, "fetch", "--no-tags", "origin", branch); err == nil {
		if pr := openPRForBranch(ctx, repoPath, branch); pr != "" {
			startPoint = "origin/" + branch
			upstream = "origin/" + branch
			existingPR = pr
		}
	}
	if existingPR == "" {
		branch = freeBranch(ctx, repoPath, branch)
	}
	suffix := "bc-" + strconv.FormatInt(job.Event.ID, 10)
	if job.Attempt > 1 {
		suffix += "-retry-" + strconv.Itoa(job.Attempt)
	}
	local := branch + "-run-" + suffix
	path := filepath.Join(s.cfg.WorktreeRoot, repo.Name, job.Agent, suffix)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return job, err
	}
	if _, err := os.Lstat(path); err == nil {
		return job, fmt.Errorf("worktree %s already exists; inspect it before retrying", path)
	} else if !os.IsNotExist(err) {
		return job, err
	}
	if _, err := gitCommand(ctx, repoPath, "worktree", "add", "-b", local, path, startPoint); err != nil {
		return job, err
	}
	job.Repo, job.Worktree, job.Branch, job.BaseBranch = repo.Name, path, branch, base
	job.LocalBranch, job.Upstream, job.ExistingPR = local, upstream, existingPR
	s.writeJobRecord(job)
	return job, nil
}

// Only a clean worktree with committed changes is pushed. Never push the base
// branch or merge. On no changes, remove the unused worktree and branch.
func (s *Server) publishWorktree(job Job) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	current, err := gitCommand(ctx, job.Worktree, "branch", "--show-current")
	if err != nil || current != job.LocalBranch {
		return "", fmt.Errorf("worktree is not on expected branch %q: %v", job.LocalBranch, err)
	}
	dirty, err := gitCommand(ctx, job.Worktree, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", fmt.Errorf("uncommitted changes remain in %s; no branch was pushed", job.Worktree)
	}
	count, err := gitCommand(ctx, job.Worktree, "rev-list", "--count", job.Upstream+"..HEAD")
	if err != nil {
		return "", err
	}
	if count == "0" {
		repo, err := s.allowedRepoByName(job.Repo)
		if err != nil {
			return "", err
		}
		if _, err := gitCommand(ctx, repo.Path, "worktree", "remove", "--", job.Worktree); err != nil {
			return "", err
		}
		if _, err := gitCommand(ctx, repo.Path, "branch", "-D", job.LocalBranch); err != nil {
			return "", err
		}
		return "", nil
	}
	if _, err := strconv.Atoi(count); err != nil {
		return "", fmt.Errorf("bad commit count %q", count)
	}
	// The shared branch is what the pull request tracks, so push this run's
	// local branch onto it. A non-fast-forward here means someone else moved
	// the branch; that surfaces as an error rather than clobbering their work.
	if _, err := gitCommand(ctx, job.Worktree, "push", "origin", "HEAD:refs/heads/"+job.Branch); err != nil {
		return "", err
	}
	if job.ExistingPR != "" {
		return job.ExistingPR, nil
	}
	title, err := gitCommand(ctx, job.Worktree, "log", "-1", "--format=%s")
	if err != nil {
		return "", err
	}
	if len(title) > 160 {
		title = title[:160]
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "create", "--base", job.BaseBranch, "--head", job.Branch, "--title", title, "--body", prBody(ctx, job))
	cmd.Dir = job.Worktree
	out := &cappedOutput{limit: 16 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("branch pushed but PR creation failed for %s: %w: %s", job.Branch, err, out.String())
	}
	url := strings.TrimSpace(out.String())
	if !strings.HasPrefix(url, "https://github.com/") || !strings.Contains(url, "/pull/") {
		return "", fmt.Errorf("unexpected PR URL: %q", url)
	}
	return url, nil
}
