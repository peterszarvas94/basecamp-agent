package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type cliOptions struct {
	configPath     string
	jsonOutput     bool
	nonInteractive bool
	dryRun         bool
	in             io.Reader
	out            io.Writer
	errOut         io.Writer
}

func executeCLI() error {
	opts := &cliOptions{in: os.Stdin, out: os.Stdout, errOut: os.Stderr}
	cmd := newRootCommand(opts)
	cmd.SetArgs(normalizeLegacyArgs(os.Args[1:]))
	return cmd.Execute()
}

func normalizeLegacyArgs(args []string) []string {
	legacy := []string{"config", "replay-chat-event", "replay-todo-assignment"}
	out := append([]string(nil), args...)
	for i, arg := range out {
		for _, name := range legacy {
			if arg == "-"+name || strings.HasPrefix(arg, "-"+name+"=") {
				out[i] = "-" + arg
				break
			}
		}
	}
	return out
}

func newRootCommand(opts *cliOptions) *cobra.Command {
	var replayChat, replayAssignment int64
	root := &cobra.Command{
		Use:           "basecamp-webhook-agent",
		Short:         "Dispatch trusted Basecamp work to local coding agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServer(opts.configPath, replayChat, replayAssignment)
		},
	}
	root.SetIn(opts.in)
	root.SetOut(opts.out)
	root.SetErr(opts.errOut)
	root.PersistentFlags().StringVar(&opts.configPath, "config", defaultConfigPath(), "configuration file")
	root.PersistentFlags().BoolVar(&opts.jsonOutput, "json", false, "print machine-readable JSON")
	root.PersistentFlags().BoolVar(&opts.nonInteractive, "non-interactive", false, "never prompt; fail when input is missing")
	root.PersistentFlags().BoolVar(&opts.dryRun, "dry-run", false, "show changes without applying them")
	root.Flags().Int64Var(&replayChat, "replay-chat-event", 0, "retry a verified chat event by ID")
	root.Flags().Int64Var(&replayAssignment, "replay-todo-assignment", 0, "retry a verified todo assignment event by ID")

	root.AddCommand(newServeCommand(opts), newSetupCommand(opts), newDoctorCommand(opts), newConfigCommand(opts), newAgentCommand(opts), newGitHubCommand(opts), newBasecampCommand(opts), newServiceCommand(opts))
	return root
}

func newServeCommand(opts *cliOptions) *cobra.Command {
	var replayChat, replayAssignment int64
	cmd := &cobra.Command{Use: "serve", Short: "Run the webhook server", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runServer(opts.configPath, replayChat, replayAssignment)
	}}
	cmd.Flags().Int64Var(&replayChat, "replay-chat-event", 0, "retry a verified chat event by ID")
	cmd.Flags().Int64Var(&replayAssignment, "replay-todo-assignment", 0, "retry a verified todo assignment event by ID")
	return cmd
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(dir, "basecamp-webhook-agent", "config.json")
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Listen: "127.0.0.1:8789", BasecampBin: "basecamp", CodexBin: "codex", ClaudeBin: "claude",
		WorkDir: filepath.Join(home, "Projects"), StatePath: filepath.Join(home, ".local/state/basecamp-webhook-agent/state.json"),
		CommandTimeoutMins: 45, MaxOutputBytes: 12000,
		BotIDs: map[string]int64{}, BotProfiles: map[string]string{}, ProjectRepos: map[string]string{},
		Cards: CardsConfig{MoveEnabled: true, InProgress: "In progress", PROpen: "PR open", Done: "Done", Failed: "Figuring it out"},
		Ops:   OpsConfig{Enabled: true, MaxJobs: 200, LogTailBytes: 200000},
	}
}

func readOrDefaultConfig(path string) (Config, error) {
	cfg, err := loadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(), nil
	}
	return cfg, err
}

func writeConfig(path string, cfg Config, dryRun bool, out io.Writer) error {
	displayCfg := cfg
	if dryRun {
		if displayCfg.GitHub.WebhookSecret != "" {
			displayCfg.GitHub.WebhookSecret = "<redacted>"
		}
		if displayCfg.Ops.Token != "" {
			displayCfg.Ops.Token = "<redacted>"
		}
	}
	b, err := json.MarshalIndent(displayCfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if dryRun {
		_, err = out.Write(b)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func validateConfig(cfg Config) error {
	var missing []string
	if cfg.AllowedAccountID == 0 {
		missing = append(missing, "allowed_account_id")
	}
	if len(cfg.AllowedProjectIDs) == 0 {
		missing = append(missing, "allowed_project_ids")
	}
	if len(cfg.AllowedCreatorIDs) == 0 {
		missing = append(missing, "allowed_creator_ids")
	}
	if len(cfg.BotIDs) == 0 {
		missing = append(missing, "bot_ids")
	}
	if len(cfg.AllowedRepos) == 0 {
		missing = append(missing, "allowed_repos")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	for _, repo := range cfg.AllowedRepos {
		if repo.Path == "" {
			return fmt.Errorf("repository %q has no path", repo.Name)
		}
		if _, err := os.Stat(expandPath(repo.Path)); err != nil {
			return fmt.Errorf("repository %q: %w", repo.Name, err)
		}
	}
	return nil
}

func printValue(opts *cliOptions, value any) error {
	if opts.jsonOutput {
		return json.NewEncoder(opts.out).Encode(value)
	}
	_, err := fmt.Fprintln(opts.out, value)
	return err
}

func newConfigCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Create, inspect, and validate configuration"}
	var force bool
	initCmd := &cobra.Command{Use: "init", Short: "Create a configuration file with safe defaults", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !force {
			if _, err := os.Stat(opts.configPath); err == nil {
				return fmt.Errorf("%s already exists (use --force to replace it)", opts.configPath)
			}
		}
		if err := writeConfig(opts.configPath, defaultConfig(), opts.dryRun, opts.out); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "config": opts.configPath, "dry_run": opts.dryRun})
	}}
	initCmd.Flags().BoolVar(&force, "force", false, "replace an existing configuration")
	show := &cobra.Command{Use: "show", Short: "Print configuration with secrets redacted", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		if cfg.GitHub.WebhookSecret != "" {
			cfg.GitHub.WebhookSecret = "<redacted>"
		}
		if cfg.Ops.Token != "" {
			cfg.Ops.Token = "<redacted>"
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		_, err = fmt.Fprintln(opts.out, string(b))
		return err
	}}
	validate := &cobra.Command{Use: "validate", Short: "Validate configuration and repository paths", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		if err := validateConfig(cfg); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "config": opts.configPath})
	}}
	set := &cobra.Command{Use: "set <key> <value>", Short: "Set a scalar configuration value", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		switch args[0] {
		case "public_url":
			cfg.PublicURL = strings.TrimRight(args[1], "/")
		case "listen":
			cfg.Listen = args[1]
		case "work_dir":
			cfg.WorkDir = expandPath(args[1])
		case "state_path":
			cfg.StatePath = expandPath(args[1])
		default:
			return fmt.Errorf("unsupported scalar key %q", args[0])
		}
		return writeConfig(opts.configPath, cfg, opts.dryRun, opts.out)
	}}
	cmd.AddCommand(initCmd, show, validate, set)
	return cmd
}

func newAgentCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Manage enabled coding agents"}
	cmd.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		return printValue(opts, map[string]any{"bot_ids": cfg.BotIDs, "bot_profiles": cfg.BotProfiles})
	}})
	for _, action := range []string{"enable", "disable"} {
		action := action
		var id int64
		var profile string
		sub := &cobra.Command{Use: action + " <codex|claude>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.ToLower(args[0])
			if name != "codex" && name != "claude" {
				return errors.New("agent must be codex or claude")
			}
			cfg, err := readOrDefaultConfig(opts.configPath)
			if err != nil {
				return err
			}
			if cfg.BotIDs == nil {
				cfg.BotIDs = map[string]int64{}
			}
			if cfg.BotProfiles == nil {
				cfg.BotProfiles = map[string]string{}
			}
			if action == "disable" {
				delete(cfg.BotIDs, name)
				delete(cfg.BotProfiles, name)
			} else {
				if id == 0 {
					return errors.New("--person-id is required")
				}
				if profile == "" {
					profile = name + "-bot"
				}
				cfg.BotIDs[name], cfg.BotProfiles[name] = id, profile
			}
			if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
				return err
			}
			return printValue(opts, map[string]any{"ok": true, "agent": name, "enabled": action == "enable"})
		}}
		sub.Flags().Int64Var(&id, "person-id", 0, "Basecamp person ID")
		sub.Flags().StringVar(&profile, "profile", "", "Basecamp CLI profile")
		cmd.AddCommand(sub)
	}
	return cmd
}

func runInteractive(opts *cliOptions, name string, args ...string) error {
	if opts.dryRun {
		_, _ = fmt.Fprintf(opts.out, "+ %s %s\n", name, strings.Join(args, " "))
		return nil
	}
	c := exec.Command(name, args...)
	c.Stdin, c.Stdout, c.Stderr = opts.in, opts.out, opts.errOut
	return c.Run()
}

func commandOutput(name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	out, err := c.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func newGitHubCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "github", Short: "Manage GitHub authentication, repositories, and webhooks"}
	cmd.AddCommand(&cobra.Command{Use: "login", Short: "Authenticate using GitHub CLI", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if opts.nonInteractive {
			return errors.New("github login requires an interactive terminal")
		}
		return runInteractive(opts, "gh", "auth", "login")
	}})
	var name string
	var aliases []string
	add := &cobra.Command{Use: "add <folder>", Short: "Allow a local GitHub repository", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := filepath.Abs(expandPath(args[0]))
		if err != nil {
			return err
		}
		if name == "" {
			name = filepath.Base(path)
		}
		if _, err := commandOutput("git", "-C", path, "rev-parse", "--show-toplevel"); err != nil {
			return err
		}
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		for i, repo := range cfg.AllowedRepos {
			if repo.Name == name || repo.Path == path {
				cfg.AllowedRepos[i] = AllowedRepo{Name: name, Path: path, Aliases: aliases}
				return writeConfig(opts.configPath, cfg, opts.dryRun, opts.out)
			}
		}
		cfg.AllowedRepos = append(cfg.AllowedRepos, AllowedRepo{Name: name, Path: path, Aliases: aliases})
		if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "repo": name, "path": path})
	}}
	add.Flags().StringVar(&name, "name", "", "configured repository name")
	add.Flags().StringSliceVar(&aliases, "alias", nil, "additional name used to select this repository")
	repos := &cobra.Command{Use: "repo", Short: "Manage local repositories"}
	repos.AddCommand(add, &cobra.Command{Use: "list", Short: "List configured repositories", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		return printValue(opts, cfg.AllowedRepos)
	}}, &cobra.Command{Use: "remove <name>", Short: "Remove a repository from the allowlist", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		kept := cfg.AllowedRepos[:0]
		for _, repo := range cfg.AllowedRepos {
			if repo.Name != args[0] {
				kept = append(kept, repo)
			}
		}
		cfg.AllowedRepos = kept
		for project, repo := range cfg.ProjectRepos {
			if repo == args[0] {
				delete(cfg.ProjectRepos, project)
			}
		}
		return writeConfig(opts.configPath, cfg, opts.dryRun, opts.out)
	}})
	cmd.AddCommand(repos)
	webhooks := &cobra.Command{Use: "webhook", Short: "Reconcile GitHub repository webhooks"}
	webhooks.AddCommand(githubWebhookCommand(opts, false), githubWebhookCommand(opts, true))
	cmd.AddCommand(webhooks)
	return cmd
}

func githubWebhookCommand(opts *cliOptions, remove bool) *cobra.Command {
	verb := "sync"
	if remove {
		verb = "remove"
	}
	var callback string
	cmd := &cobra.Command{Use: verb, Short: strings.Title(verb) + " GitHub webhooks for configured repositories", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		if callback == "" && cfg.PublicURL != "" {
			callback = strings.TrimRight(cfg.PublicURL, "/") + "/github/webhook"
		}
		if callback == "" {
			return errors.New("--url is required when public_url is not configured")
		}
		if cfg.GitHub.WebhookSecret == "" && !remove {
			cfg.GitHub.WebhookSecret, err = randomSecret(32)
			if err != nil {
				return err
			}
			if err = writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
				return err
			}
		}
		results := []map[string]any{}
		for _, repo := range cfg.AllowedRepos {
			slug, err := githubSlug(repo.Path)
			if err != nil {
				return err
			}
			changed, err := reconcileGitHubWebhook(opts, slug, callback, cfg.GitHub.WebhookSecret, remove)
			if err != nil {
				return err
			}
			results = append(results, map[string]any{"repo": slug, "changed": changed})
		}
		return printValue(opts, results)
	}}
	cmd.Flags().StringVar(&callback, "url", "", "public callback URL")
	return cmd
}

func githubSlug(path string) (string, error) {
	b, err := commandOutput("git", "-C", path, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	u := strings.TrimSpace(string(b))
	u = strings.TrimSuffix(u, ".git")
	for _, prefix := range []string{"git@github.com:", "https://github.com/", "ssh://git@github.com/"} {
		if strings.HasPrefix(u, prefix) {
			return strings.TrimPrefix(u, prefix), nil
		}
	}
	return "", fmt.Errorf("%s does not have a GitHub origin", path)
}

func reconcileGitHubWebhook(opts *cliOptions, slug, callback, secret string, remove bool) (bool, error) {
	if opts.dryRun {
		_, _ = fmt.Fprintf(opts.out, "%s GitHub webhook %s for %s\n", map[bool]string{true: "remove", false: "sync"}[remove], callback, slug)
		return true, nil
	}
	b, err := commandOutput("gh", "api", "repos/"+slug+"/hooks", "--paginate")
	if err != nil {
		return false, err
	}
	var hooks []struct {
		ID     int64 `json:"id"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	if err := json.Unmarshal(b, &hooks); err != nil {
		return false, err
	}
	for _, hook := range hooks {
		if hook.Config.URL == callback {
			if remove {
				_, err = commandOutput("gh", "api", "--method", "DELETE", fmt.Sprintf("repos/%s/hooks/%d", slug, hook.ID))
				return err == nil, err
			}
			return false, nil
		}
	}
	if remove {
		return false, nil
	}
	payload, _ := json.Marshal(map[string]any{"name": "web", "active": true, "events": []string{"pull_request"}, "config": map[string]string{"url": callback, "content_type": "json", "secret": secret, "insecure_ssl": "0"}})
	c := exec.Command("gh", "api", "--method", "POST", "repos/"+slug+"/hooks", "--input", "-")
	c.Stdin = bytes.NewReader(payload)
	out, err := c.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("create webhook for %s: %w: %s", slug, err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

func randomSecret(bytesCount int) (string, error) {
	b := make([]byte, bytesCount)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newBasecampCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "basecamp", Short: "Manage Basecamp profiles, projects, and webhooks"}
	var profile string
	login := &cobra.Command{Use: "login", Short: "Create or authenticate a Basecamp CLI profile", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if opts.nonInteractive {
			return errors.New("basecamp login requires an interactive terminal")
		}
		args := []string{"profile", "create"}
		if profile != "" {
			args = append(args, profile)
		}
		return runInteractive(opts, "basecamp", args...)
	}}
	login.Flags().StringVar(&profile, "profile", "", "profile name, such as codex-bot")
	cmd.AddCommand(login)
	var account, project, creator int64
	var defaultRepo string
	projectAdd := &cobra.Command{Use: "add", Short: "Allow a Basecamp project", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if account == 0 || project == 0 || creator == 0 {
			return errors.New("--account, --project, and --creator are required")
		}
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		cfg.AllowedAccountID = account
		cfg.AllowedProjectIDs = appendUniqueInt64(cfg.AllowedProjectIDs, project)
		cfg.AllowedCreatorIDs = appendUniqueInt64(cfg.AllowedCreatorIDs, creator)
		if cfg.ProjectRepos == nil {
			cfg.ProjectRepos = map[string]string{}
		}
		if defaultRepo != "" {
			cfg.ProjectRepos[strconv.FormatInt(project, 10)] = defaultRepo
		}
		return writeConfig(opts.configPath, cfg, opts.dryRun, opts.out)
	}}
	projectAdd.Flags().Int64Var(&account, "account", 0, "Basecamp account ID")
	projectAdd.Flags().Int64Var(&project, "project", 0, "Basecamp project ID")
	projectAdd.Flags().Int64Var(&creator, "creator", 0, "trusted requester person ID")
	projectAdd.Flags().StringVar(&defaultRepo, "default-repo", "", "default repository name")
	projects := &cobra.Command{Use: "project", Short: "Manage Basecamp projects"}
	projects.AddCommand(projectAdd, &cobra.Command{Use: "list", Short: "List configured Basecamp projects", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		return printValue(opts, map[string]any{"account": cfg.AllowedAccountID, "projects": cfg.AllowedProjectIDs, "default_repos": cfg.ProjectRepos})
	}}, &cobra.Command{Use: "remove <project-id>", Short: "Remove a Basecamp project", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		kept := cfg.AllowedProjectIDs[:0]
		for _, projectID := range cfg.AllowedProjectIDs {
			if projectID != id {
				kept = append(kept, projectID)
			}
		}
		cfg.AllowedProjectIDs = kept
		delete(cfg.ProjectRepos, args[0])
		return writeConfig(opts.configPath, cfg, opts.dryRun, opts.out)
	}})
	cmd.AddCommand(projects)
	webhooks := &cobra.Command{Use: "webhook", Short: "Reconcile Basecamp project webhooks"}
	webhooks.AddCommand(basecampWebhookCommand(opts, false), basecampWebhookCommand(opts, true))
	cmd.AddCommand(webhooks)
	return cmd
}

func appendUniqueInt64(values []int64, value int64) []int64 {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}

func basecampWebhookCommand(opts *cliOptions, remove bool) *cobra.Command {
	verb := "sync"
	if remove {
		verb = "remove"
	}
	var callback, profile string
	cmd := &cobra.Command{Use: verb, Short: strings.Title(verb) + " Basecamp webhooks for configured projects", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		if callback == "" && cfg.PublicURL != "" {
			callback = strings.TrimRight(cfg.PublicURL, "/") + "/webhook"
		}
		if callback == "" {
			return errors.New("--url is required when public_url is not configured")
		}
		if profile == "" {
			profile = cfg.BotProfiles["codex"]
		}
		if profile == "" {
			return errors.New("--profile is required when no codex bot profile is configured")
		}
		for _, id := range cfg.AllowedProjectIDs {
			if err := reconcileBasecampWebhook(opts, cfg.BasecampBin, profile, id, callback, remove); err != nil {
				return err
			}
		}
		return printValue(opts, map[string]any{"ok": true, "projects": len(cfg.AllowedProjectIDs), "removed": remove})
	}}
	cmd.Flags().StringVar(&callback, "url", "", "public callback URL")
	cmd.Flags().StringVar(&profile, "profile", "", "Basecamp CLI profile")
	return cmd
}

func reconcileBasecampWebhook(opts *cliOptions, bin, profile string, project int64, callback string, remove bool) error {
	if bin == "" {
		bin = "basecamp"
	}
	projectArg := strconv.FormatInt(project, 10)
	if opts.dryRun {
		_, _ = fmt.Fprintf(opts.out, "%s Basecamp webhook %s for project %d\n", map[bool]string{true: "remove", false: "sync"}[remove], callback, project)
		return nil
	}
	b, err := commandOutput(bin, "-P", profile, "webhooks", "list", "--project", projectArg, "--agent")
	if err != nil {
		return err
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	id := findWebhookID(raw, callback)
	if remove {
		if id == 0 {
			return nil
		}
		_, err = commandOutput(bin, "-P", profile, "webhooks", "delete", strconv.FormatInt(id, 10), "--project", projectArg, "--agent")
		return err
	}
	if id != 0 {
		return nil
	}
	_, err = commandOutput(bin, "-P", profile, "webhooks", "create", callback, "--types", "Message,Todo,Kanban::Card,Comment", "--project", projectArg, "--agent")
	return err
}

func findWebhookID(v any, callback string) int64 {
	switch x := v.(type) {
	case map[string]any:
		u, _ := x["payload_url"].(string)
		if u == "" {
			u, _ = x["url"].(string)
		}
		if u == callback {
			if id, ok := x["id"].(float64); ok {
				return int64(id)
			}
		}
		for _, child := range x {
			if id := findWebhookID(child, callback); id != 0 {
				return id
			}
		}
	case []any:
		for _, child := range x {
			if id := findWebhookID(child, callback); id != 0 {
				return id
			}
		}
	}
	return 0
}

func newDoctorCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{Use: "doctor", Aliases: []string{"check"}, Short: "Check configuration and external tools", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		checks := map[string]any{}
		failed := false
		cfg, configErr := loadConfig(opts.configPath)
		bins := []string{"git", "basecamp", "gh"}
		if _, enabled := cfg.BotIDs["codex"]; enabled {
			bins = append(bins, "codex")
		}
		if _, enabled := cfg.BotIDs["claude"]; enabled {
			bins = append(bins, "claude")
		}
		for _, bin := range bins {
			path, err := exec.LookPath(bin)
			checks[bin] = map[string]any{"ok": err == nil, "path": path}
			failed = failed || err != nil
		}
		checks["config"] = map[string]any{"ok": configErr == nil, "path": opts.configPath}
		if configErr == nil {
			configErr = validateConfig(cfg)
			checks["configuration"] = map[string]any{"ok": configErr == nil, "error": errorString(configErr)}
		}
		failed = failed || configErr != nil
		_ = printValue(opts, checks)
		if failed {
			return errors.New("one or more checks failed")
		}
		return nil
	}}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func newServiceCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Manage the systemd user service"}
	serviceAction := func(action string) *cobra.Command {
		return &cobra.Command{Use: action, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return runInteractive(opts, "systemctl", "--user", action, "basecamp-webhook-agent.service")
		}}
	}
	install := &cobra.Command{Use: "install", Short: "Install and enable the systemd user service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return installSystemdService(opts)
	}}
	uninstall := &cobra.Command{Use: "uninstall", Short: "Disable and remove the systemd user service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := runInteractive(opts, "systemctl", "--user", "disable", "--now", "basecamp-webhook-agent.service"); err != nil && !opts.dryRun {
			return err
		}
		home, _ := os.UserHomeDir()
		path := filepath.Join(home, ".config/systemd/user/basecamp-webhook-agent.service")
		if opts.dryRun {
			_, _ = fmt.Fprintf(opts.out, "remove %s\n", path)
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return runInteractive(opts, "systemctl", "--user", "daemon-reload")
	}}
	cmd.AddCommand(install, uninstall, serviceAction("start"), serviceAction("stop"), serviceAction("restart"), serviceAction("status"))
	return cmd
}

func installSystemdService(opts *cliOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".config/systemd/user/basecamp-webhook-agent.service")
	body := fmt.Sprintf("[Unit]\nDescription=Basecamp webhook local coding agent dispatcher\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=%s serve --config %s\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n", strconv.Quote(exe), strconv.Quote(opts.configPath))
	if opts.dryRun {
		_, err = fmt.Fprint(opts.out, body)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		return err
	}
	if err := runInteractive(opts, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return runInteractive(opts, "systemctl", "--user", "enable", "--now", "basecamp-webhook-agent.service")
}

func newSetupCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{Use: "setup", Short: "Interactively configure the agent from reusable setup operations", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if opts.nonInteractive {
			return errors.New("setup is interactive; use the individual commands with --non-interactive")
		}
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		reader := bufio.NewReader(opts.in)
		fmt.Fprintln(opts.out, "Basecamp webhook agent setup (press Enter to keep a displayed value)")
		if cfg.PublicURL, err = prompt(reader, opts.out, "Public base URL (for example https://agent.example.com)", cfg.PublicURL); err != nil {
			return err
		}
		if cfg.AllowedAccountID, err = promptInt64(reader, opts.out, "Basecamp account ID", cfg.AllowedAccountID); err != nil {
			return err
		}
		var project, creator int64
		if len(cfg.AllowedProjectIDs) > 0 {
			project = cfg.AllowedProjectIDs[0]
		}
		if len(cfg.AllowedCreatorIDs) > 0 {
			creator = cfg.AllowedCreatorIDs[0]
		}
		if project, err = promptInt64(reader, opts.out, "Basecamp project ID", project); err != nil {
			return err
		}
		if creator, err = promptInt64(reader, opts.out, "Trusted requester person ID", creator); err != nil {
			return err
		}
		cfg.AllowedProjectIDs = appendUniqueInt64(cfg.AllowedProjectIDs, project)
		cfg.AllowedCreatorIDs = appendUniqueInt64(cfg.AllowedCreatorIDs, creator)
		repoPath, err := prompt(reader, opts.out, "Local repository folder", "")
		if err != nil {
			return err
		}
		if repoPath != "" {
			abs, err := filepath.Abs(expandPath(repoPath))
			if err != nil {
				return err
			}
			if _, err := commandOutput("git", "-C", abs, "rev-parse", "--show-toplevel"); err != nil {
				return err
			}
			repoName := filepath.Base(abs)
			cfg.AllowedRepos = append(cfg.AllowedRepos, AllowedRepo{Name: repoName, Path: abs})
			if cfg.ProjectRepos == nil {
				cfg.ProjectRepos = map[string]string{}
			}
			cfg.ProjectRepos[strconv.FormatInt(project, 10)] = repoName
		}
		agents, err := prompt(reader, opts.out, "Agents to enable (codex, claude, or both)", "codex")
		if err != nil {
			return err
		}
		enabledAgents := strings.FieldsFunc(strings.ToLower(agents), func(r rune) bool { return r == ',' || r == ' ' })
		if len(enabledAgents) == 1 && enabledAgents[0] == "both" {
			enabledAgents = []string{"codex", "claude"}
		}
		for _, agent := range enabledAgents {
			if agent != "codex" && agent != "claude" {
				return fmt.Errorf("unknown agent %q; choose codex, claude, or both", agent)
			}
			id, err := promptInt64(reader, opts.out, agent+" Basecamp bot person ID", cfg.BotIDs[agent])
			if err != nil {
				return err
			}
			if cfg.BotIDs == nil {
				cfg.BotIDs = map[string]int64{}
			}
			if cfg.BotProfiles == nil {
				cfg.BotProfiles = map[string]string{}
			}
			cfg.BotIDs[agent], cfg.BotProfiles[agent] = id, agent+"-bot"
		}
		if cfg.GitHub.WebhookSecret == "" {
			cfg.GitHub.WebhookSecret, err = randomSecret(32)
			if err != nil {
				return err
			}
		}
		if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
			return err
		}
		fmt.Fprintf(opts.out, "Configuration written to %s.\n", opts.configPath)

		authNow, err := promptBool(reader, opts.out, "Authenticate required CLI profiles now", true)
		if err != nil {
			return err
		}
		if authNow {
			if _, err := commandOutput("gh", "auth", "status"); err != nil {
				if err := runInteractive(opts, "gh", "auth", "login"); err != nil {
					return fmt.Errorf("GitHub login: %w", err)
				}
			}
			for agent, profile := range cfg.BotProfiles {
				if err := ensureBasecampProfile(opts, cfg.BasecampBin, profile); err != nil {
					return fmt.Errorf("%s Basecamp profile: %w", agent, err)
				}
			}
		}

		if cfg.PublicURL != "" {
			syncNow, err := promptBool(reader, opts.out, "Create or update Basecamp and GitHub webhooks now", true)
			if err != nil {
				return err
			}
			if syncNow {
				basecampURL := strings.TrimRight(cfg.PublicURL, "/") + "/webhook"
				profile := cfg.BotProfiles["codex"]
				if profile == "" {
					for _, candidate := range cfg.BotProfiles {
						profile = candidate
						break
					}
				}
				for _, id := range cfg.AllowedProjectIDs {
					if err := reconcileBasecampWebhook(opts, cfg.BasecampBin, profile, id, basecampURL, false); err != nil {
						return err
					}
				}
				githubURL := strings.TrimRight(cfg.PublicURL, "/") + "/github/webhook"
				for _, repo := range cfg.AllowedRepos {
					slug, err := githubSlug(repo.Path)
					if err != nil {
						return err
					}
					if _, err := reconcileGitHubWebhook(opts, slug, githubURL, cfg.GitHub.WebhookSecret, false); err != nil {
						return err
					}
				}
			}
		}

		installNow, err := promptBool(reader, opts.out, "Install and start the systemd user service", true)
		if err != nil {
			return err
		}
		if installNow {
			if err := installSystemdService(opts); err != nil {
				return err
			}
		}
		fmt.Fprintln(opts.out, "Setup complete. Run `basecamp-webhook-agent doctor` at any time to verify it.")
		return nil
	}}
}

func ensureBasecampProfile(opts *cliOptions, bin, profile string) error {
	if bin == "" {
		bin = "basecamp"
	}
	if opts.dryRun {
		_, _ = fmt.Fprintf(opts.out, "ensure Basecamp profile %s\n", profile)
		return nil
	}
	b, err := commandOutput(bin, "profile", "list", "--agent")
	if err == nil && strings.Contains(string(b), `"`+profile+`"`) {
		return nil
	}
	return runInteractive(opts, bin, "profile", "create", profile)
}

func prompt(reader *bufio.Reader, out io.Writer, label, current string) (string, error) {
	if current == "" {
		fmt.Fprintf(out, "%s: ", label)
	} else {
		fmt.Fprintf(out, "%s [%s]: ", label, current)
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return current, nil
	}
	return line, nil
}

func promptInt64(reader *bufio.Reader, out io.Writer, label string, current int64) (int64, error) {
	cur := ""
	if current != 0 {
		cur = strconv.FormatInt(current, 10)
	}
	value, err := prompt(reader, out, label, cur)
	if err != nil {
		return 0, err
	}
	if value == "" {
		return 0, fmt.Errorf("%s is required", label)
	}
	return strconv.ParseInt(value, 10, 64)
}

func promptBool(reader *bufio.Reader, out io.Writer, label string, defaultValue bool) (bool, error) {
	suffix := "y/N"
	if defaultValue {
		suffix = "Y/n"
	}
	value, err := prompt(reader, out, label+" ["+suffix+"]", "")
	if err != nil {
		return false, err
	}
	if value == "" {
		return defaultValue, nil
	}
	switch strings.ToLower(value) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("answer %q with yes or no", value)
	}
}
