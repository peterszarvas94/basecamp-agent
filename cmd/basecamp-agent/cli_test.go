package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLIForTest(t *testing.T, config string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	opts := &cliOptions{configPath: config, in: strings.NewReader(""), out: &out, errOut: &out}
	cmd := newRootCommand(opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--config", config}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestConfigAndComposableSetupCommands(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	if _, err := runCLIForTest(t, config, "config", "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLIForTest(t, config, "agent", "enable", "codex", "--person-id", "42", "--profile", "codex-bot"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLIForTest(t, config, "project", "add", "--account", "1", "--project", "2", "--creator", "3", "--no-sync"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BotIDs["codex"] != 42 || cfg.BotProfiles["codex"] != "codex-bot" {
		t.Fatalf("unexpected bot config: %+v %+v", cfg.BotIDs, cfg.BotProfiles)
	}
	if cfg.AllowedAccountID != 1 || len(cfg.AllowedProjectIDs) != 1 || cfg.AllowedProjectIDs[0] != 2 {
		t.Fatalf("unexpected project config: %+v", cfg)
	}
	if _, err := runCLIForTest(t, config, "project", "add", "--account", "1", "--project", "2", "--creator", "3", "--no-sync"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = loadConfig(config)
	if len(cfg.AllowedProjectIDs) != 1 || len(cfg.AllowedCreatorIDs) != 1 {
		t.Fatalf("project add is not idempotent: %+v", cfg)
	}
}

func TestConfigShowRedactsSecrets(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	cfg := defaultConfig()
	cfg.GitHub.WebhookSecret = "github-secret"
	cfg.Ops.Token = "ops-secret"
	if err := writeConfig(config, cfg, false, os.Stdout); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIForTest(t, config, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "github-secret") || strings.Contains(out, "ops-secret") || strings.Count(out, "redacted") != 2 {
		t.Fatalf("secrets were not redacted: %s", out)
	}
}

func TestNestedCommandHelp(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	for _, args := range [][]string{{"install", "--help"}, {"dependencies", "check", "--help"}, {"github", "repo", "add", "--help"}, {"github", "repo", "create", "--help"}, {"railway", "deploy", "--help"}, {"project", "add", "--help"}} {
		if _, err := runCLIForTest(t, config, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestInstallCommandsAreComposableAndDryRunnable(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	out, err := runCLIForTest(t, config, "--dry-run", "install", "--force", "basecamp", "codex", "railway", "tailscale")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"go install github.com/basecamp/basecamp-cli/cmd/basecamp@latest",
		"npm install --global @openai/codex@latest",
		"curl -fsSL https://railway.com/install.sh | sh",
		"curl -fsSL https://tailscale.com/install.sh | sh",
		"basecamp: would install",
		"railway: would install",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("install output does not contain %q:\n%s", want, out)
		}
	}
}

func TestNonInteractiveSetupConfiguresRepository(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runCLIForTest(t, config,
		"--non-interactive", "setup",
		"--repo", repo,
		"--repo-name", "agent",
		"--repo-alias", "dispatcher",
		"--public-url", "https://agent.example.test/",
		"--account", "1",
		"--project", "2",
		"--creator", "3",
		"--codex-person-id", "4",
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://agent.example.test" || cfg.AllowedAccountID != 1 || cfg.ProjectRepos["2"] != "agent" {
		t.Fatalf("unexpected setup config: %+v", cfg)
	}
	if cfg.BotIDs["codex"] != 4 || cfg.BotProfiles["codex"] != "codex-bot" {
		t.Fatalf("unexpected agent config: %+v %+v", cfg.BotIDs, cfg.BotProfiles)
	}
	if len(cfg.AllowedRepos) != 1 || cfg.AllowedRepos[0].Path != repo || len(cfg.AllowedRepos[0].Aliases) != 1 {
		t.Fatalf("unexpected repository config: %+v", cfg.AllowedRepos)
	}
	// Reconciliation is idempotent.
	if _, err := runCLIForTest(t, config, "--non-interactive", "setup", "--repo", repo, "--repo-name", "agent", "--project", "2"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = loadConfig(config)
	if len(cfg.AllowedRepos) != 1 || len(cfg.AllowedProjectIDs) != 1 {
		t.Fatalf("setup is not idempotent: %+v", cfg)
	}
	if aliases := cfg.AllowedRepos[0].Aliases; len(aliases) != 1 || aliases[0] != "dispatcher" {
		t.Fatalf("rerunning setup without --repo-alias dropped aliases: %v", aliases)
	}
	if _, err := runCLIForTest(t, config, "github", "repo", "add", repo, "--name", "agent"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = loadConfig(config)
	if aliases := cfg.AllowedRepos[0].Aliases; len(aliases) != 1 {
		t.Fatalf("repo add without --alias dropped aliases: %v", aliases)
	}
}

func TestRailwayDeployUsesRailwayCLI(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	out, err := runCLIForTest(t, config, "--dry-run", "--non-interactive", "railway", "deploy", ".", "--new", "--name", "agent", "--project", "project-id")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"railway up", "--new", "--yes", "--name agent", "--project project-id"} {
		if !strings.Contains(out, want) {
			t.Fatalf("railway output does not contain %q:\n%s", want, out)
		}
	}
}

func TestRailwayDeployUsesGlobalRepositoryContextAndFlagOverrides(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	repo := t.TempDir()
	cfg := defaultConfig()
	cfg.AllowedRepos = AllowedRepoList{{
		Name: "app", Path: repo,
		Railway: RailwayRepoConfig{Project: "stored-project", Service: "stored-service", Environment: "production"},
	}}
	if err := writeConfig(config, cfg, false, os.Stdout); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIForTest(t, config, "--dry-run", "railway", "deploy", repo, "--service", "override-service")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(in " + repo + ") railway up", "--project stored-project", "--service override-service", "--environment production"} {
		if !strings.Contains(out, want) {
			t.Fatalf("railway output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stored-service") {
		t.Fatalf("explicit flag did not override global config:\n%s", out)
	}
}

func TestRailwayConfigureStoresContextInGlobalConfig(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	repo := t.TempDir()
	cfg := defaultConfig()
	cfg.AllowedRepos = AllowedRepoList{{Name: "app", Path: repo}}
	if err := writeConfig(config, cfg, false, os.Stdout); err != nil {
		t.Fatal(err)
	}
	_, err := runCLIForTest(t, config, "railway", "configure", repo, "--project", "project-id", "--service", "app", "--environment", "production", "--domain", "https://app.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.AllowedRepos[0].Railway
	if got.Project != "project-id" || got.Service != "app" || got.Environment != "production" || got.Domain != "https://app.example.test" {
		t.Fatalf("unexpected Railway config: %+v", got)
	}
}

func TestRailwayDomainUsesRailwayCLI(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	out, err := runCLIForTest(t, config, "--dry-run", "railway", "domain", "--project", "project-id", "--service", "service-id", "--port", "8080")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"railway domain", "--project project-id", "--service service-id", "--port 8080"} {
		if !strings.Contains(out, want) {
			t.Fatalf("railway domain output does not contain %q:\n%s", want, out)
		}
	}
}

func TestNormalizeLegacyFlags(t *testing.T) {
	got := normalizeLegacyArgs([]string{"-config", "/tmp/config.json", "-replay-chat-event=12"})
	want := []string{"--config", "/tmp/config.json", "--replay-chat-event=12"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argument %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFindWebhookIDUsesBasecampPayloadURL(t *testing.T) {
	v := map[string]any{"webhooks": []any{map[string]any{"id": float64(123), "payload_url": "https://example.test/webhook"}}}
	if got := findWebhookID(v, "https://example.test/webhook"); got != 123 {
		t.Fatalf("findWebhookID = %d, want 123", got)
	}
}

func TestAgentListIsHumanReadable(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	cfg := defaultConfig()
	cfg.BotIDs = map[string]int64{"codex": 42, "claude": 41}
	cfg.BotProfiles = map[string]string{"codex": "codex-bot", "claude": "claude-bot"}
	if err := writeConfig(config, cfg, false, os.Stdout); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIForTest(t, config, "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Enabled agents:", "Claude", "Person ID: 41", "Basecamp profile: claude-bot", "Codex", "Person ID: 42"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "map[") {
		t.Fatalf("output contains Go map formatting:\n%s", out)
	}
}

func TestRepoAndProjectListsAreHumanReadable(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	cfg := defaultConfig()
	cfg.AllowedAccountID = 10
	cfg.AllowedProjectIDs = []int64{20}
	cfg.ProjectRepos = map[string]string{"20": "app"}
	cfg.AllowedRepos = AllowedRepoList{{Name: "app", Path: "/srv/app", Aliases: []string{"api", "backend"}}}
	if err := writeConfig(config, cfg, false, os.Stdout); err != nil {
		t.Fatal(err)
	}
	repos, err := runCLIForTest(t, config, "github", "repo", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Configured repositories:", "app", "Path: /srv/app", "Aliases: api, backend"} {
		if !strings.Contains(repos, want) {
			t.Fatalf("repository output does not contain %q:\n%s", want, repos)
		}
	}
	projects, err := runCLIForTest(t, config, "project", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Project 20", "Repository: app", "Basecamp:   https://app.basecamp.com/10/projects/20"} {
		if !strings.Contains(projects, want) {
			t.Fatalf("project output does not contain %q:\n%s", want, projects)
		}
	}
}

func TestHumanValueOutputNeverUsesGoMapFormatting(t *testing.T) {
	var out bytes.Buffer
	opts := &cliOptions{out: &out}
	value := map[string]any{"ok": true, "checks": map[string]any{"git": map[string]any{"ok": true, "path": "/usr/bin/git"}}, "items": []any{map[string]any{"name": "app"}}}
	if err := printValue(opts, value); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "map[") {
		t.Fatalf("output contains Go map formatting:\n%s", out.String())
	}
	for _, want := range []string{"Checks:", "Git:", "Path: /usr/bin/git", "Ok: yes", "Name: app"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestGitHubWebhookResultsAreHumanReadable(t *testing.T) {
	var out bytes.Buffer
	opts := &cliOptions{out: &out, dryRun: true}
	results := []map[string]any{{"repo": "owner/app", "changed": true}}
	if err := printGitHubWebhookResults(opts, results, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "GitHub webhooks:\n  owner/app: would sync\n" {
		t.Fatalf("unexpected output:\n%s", got)
	}
}

func TestDoctorChecksAreHumanReadable(t *testing.T) {
	var out bytes.Buffer
	opts := &cliOptions{out: &out}
	checks := map[string]any{
		"git":           map[string]any{"ok": true, "path": "/usr/bin/git"},
		"configuration": map[string]any{"ok": true, "error": ""},
		"gh":            map[string]any{"ok": false, "path": ""},
	}
	if err := printChecks(opts, checks); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Checks:", "✓ Git: /usr/bin/git", "✓ Configuration", "✗ GitHub CLI: not available"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestDryRunCommandsAreReadableAndDoNotMutateConfig(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.PublicURL = "https://agent.example.test"
	cfg.AllowedAccountID = 1
	cfg.AllowedProjectIDs = []int64{2}
	cfg.AllowedCreatorIDs = []int64{3}
	cfg.BotIDs = map[string]int64{"codex": 4}
	cfg.BotProfiles = map[string]string{"codex": "codex-bot"}
	cfg.AllowedRepos = AllowedRepoList{{Name: "app", Path: repo}}
	if err := writeConfig(config, cfg, false, os.Stdout); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"--dry-run", "agent", "enable", "claude", "--person-id", "5"},
		{"--dry-run", "basecamp", "webhook", "sync"},
		{"--dry-run", "github", "webhook", "sync"},
		{"--dry-run", "service", "install"},
	}
	for _, args := range commands {
		out, err := runCLIForTest(t, config, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if strings.Contains(out, "map[") {
			t.Fatalf("%v produced Go map formatting:\n%s", args, out)
		}
	}
	after, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dry-run command changed the configuration file")
	}
}

func TestListShowsEverythingConfigured(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	cfg := defaultConfig()
	cfg.AllowedAccountID = 10
	cfg.AllowedProjectIDs = []int64{20}
	cfg.AllowedCreatorIDs = []int64{30}
	cfg.ProjectRepos = map[string]string{"20": "app"}
	cfg.AllowedRepos = AllowedRepoList{{Name: "app", Path: "/srv/app", Railway: RailwayRepoConfig{Project: "p-1", Service: "app", Environment: "production", Domain: "https://app.up.railway.app"}}}
	cfg.BotIDs = map[string]int64{"codex": 40}
	cfg.BotProfiles = map[string]string{"codex": "codex-bot"}
	if err := writeConfig(config, cfg, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIForTest(t, config, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Public URL: not set", "Requesters: 30", "Projects (account 10):", "Project 20", "Repository: app", "Configured repositories:", "Path: /srv/app", "Railway: app (production) https://app.up.railway.app", "Enabled agents:", "Basecamp profile: codex-bot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output does not contain %q:\n%s", want, out)
		}
	}
}

func TestParseRailwayStatusReadsNewProject(t *testing.T) {
	status := `{"id":"p-1","name":"app","services":{"edges":[{"node":{"id":"s-1","name":"app"}}]},"environments":{"edges":[{"node":{"id":"e-2","name":"staging"}},{"node":{"id":"e-1","name":"production"}}]}}`
	got, err := parseRailwayStatus([]byte(status))
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != "p-1" || got.Service != "app" || got.Environment != "production" {
		t.Fatalf("unexpected context: %+v", got)
	}
	if _, err := parseRailwayStatus([]byte(`{"id":"p-1","services":{"edges":[{"node":{"name":"a"}},{"node":{"name":"b"}}]},"environments":{"edges":[{"node":{"name":"production"}}]}}`)); err == nil {
		t.Fatal("two services are ambiguous and must not be guessed")
	}
}

func TestRailwayConnectUsesGitHubSourceAndPREnvironments(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.AllowedRepos = AllowedRepoList{{Name: "agent", Path: repo, Railway: RailwayRepoConfig{Project: "p-1", Service: "web", Environment: "production"}}}
	if err := writeConfig(config, cfg, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIForTest(t, config, "--dry-run", "railway", "connect", repo)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"railway service source connect --repo peterszarvas94/basecamp-agent --branch master --service web --project p-1 --environment production", "prDeploys: true", "--raw-var id=p-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
	cfg.AllowedRepos[0].Railway = RailwayRepoConfig{}
	if err := writeConfig(config, cfg, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLIForTest(t, config, "--dry-run", "railway", "connect", repo); err == nil {
		t.Fatal("connect without Railway context should fail")
	}
}
