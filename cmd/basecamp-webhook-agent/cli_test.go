package main

import (
	"bytes"
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
	if _, err := runCLIForTest(t, config, "basecamp", "project", "add", "--account", "1", "--project", "2", "--creator", "3"); err != nil {
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
	if _, err := runCLIForTest(t, config, "basecamp", "project", "add", "--account", "1", "--project", "2", "--creator", "3"); err != nil {
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
	for _, args := range [][]string{{"github", "repo", "add", "--help"}, {"basecamp", "project", "add", "--help"}} {
		if _, err := runCLIForTest(t, config, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
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
