package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultWebhookProfilePrefersCodex(t *testing.T) {
	cfg := Config{BotProfiles: map[string]string{"claude": "claude-bot", "codex": "codex-bot"}}
	if got := defaultWebhookProfile(cfg); got != "codex-bot" {
		t.Fatalf("got %q", got)
	}
	cfg.BotProfiles = map[string]string{"zeta": "z-bot", "claude": "claude-bot"}
	if got := defaultWebhookProfile(cfg); got != "claude-bot" {
		t.Fatalf("got %q", got)
	}
}

func TestValidPublicURL(t *testing.T) {
	if err := validPublicURL("https://agent.example.com"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://agent.example.com", "https://user@agent.example.com", "https://agent.example.com/?x=1", "agent.example.com", ""} {
		if validPublicURL(raw) == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
}

func TestEndpointSetDryRunMovesWebhooksWithoutWriting(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.PublicURL = "https://old.example.test"
	cfg.AllowedProjectIDs = []int64{2}
	cfg.BotProfiles = map[string]string{"codex": "codex-bot"}
	cfg.AllowedRepos = AllowedRepoList{{Name: "app", Path: repo}}
	if err := writeConfig(config, cfg, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIForTest(t, config, "--dry-run", "endpoint", "set", "https://new.example.test/")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"remove Basecamp webhook https://old.example.test/webhook for project 2",
		"sync Basecamp webhook https://new.example.test/webhook for project 2",
		"sync GitHub webhook https://new.example.test/github/webhook",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
	after, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if after.PublicURL != cfg.PublicURL {
		t.Fatalf("dry run changed public_url to %q", after.PublicURL)
	}
	if _, err := runCLIForTest(t, config, "--dry-run", "endpoint", "set", "http://insecure.test"); err == nil {
		t.Fatal("endpoint set accepted a non-HTTPS URL")
	}
}
