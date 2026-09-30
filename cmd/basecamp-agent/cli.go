package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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
		Use:           "basecamp-agent",
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

	root.AddCommand(newServeCommand(opts), newSetupCommand(opts), newInstallCommand(opts), newDependenciesCommand(opts), newEndpointCommand(opts), newOpsCommand(opts), newDoctorCommand(opts), newConfigCommand(opts), newAgentCommand(opts), newGitHubCommand(opts), newProjectCommand(opts), newBasecampCommand(opts), newListCommand(opts), newRailwayCommand(opts), newServiceCommand(opts))
	return root
}

type dependencySpec struct {
	Name       string
	Executable string
	Purpose    string
}

var dependencySpecs = []dependencySpec{
	{Name: "git", Executable: "git", Purpose: "repository and worktree management"},
	{Name: "gh", Executable: "gh", Purpose: "GitHub repositories, pull requests, and webhooks"},
	{Name: "basecamp", Executable: "basecamp", Purpose: "Basecamp authentication and API access"},
	{Name: "codex", Executable: "codex", Purpose: "Codex coding worker"},
	{Name: "claude", Executable: "claude", Purpose: "Claude coding worker"},
	{Name: "railway", Executable: "railway", Purpose: "Railway deployment and configuration"},
	{Name: "tailscale", Executable: "tailscale", Purpose: "private networking and optional public ingress"},
}

func dependencyByName(name string) (dependencySpec, bool) {
	for _, spec := range dependencySpecs {
		if spec.Name == name {
			return spec, true
		}
	}
	return dependencySpec{}, false
}

func newInstallCommand(opts *cliOptions) *cobra.Command {
	var force bool
	cmd := &cobra.Command{Use: "install <dependency>...", Short: "Install dependency CLIs", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		names := args
		if len(args) == 1 && args[0] == "all" {
			names = make([]string, 0, len(dependencySpecs))
			for _, spec := range dependencySpecs {
				names = append(names, spec.Name)
			}
		}
		results, err := installDependencies(opts, names, force)
		if err != nil {
			return err
		}
		return printDependencyResults(opts, results)
	}}
	cmd.Flags().BoolVar(&force, "force", false, "reinstall even when the executable is already available")
	return cmd
}

func installDependencies(opts *cliOptions, names []string, force bool) ([]map[string]any, error) {
	results := make([]map[string]any, 0, len(names))
	for _, name := range names {
		spec, ok := dependencyByName(strings.ToLower(name))
		if !ok {
			return nil, fmt.Errorf("unknown dependency %q (choose %s, or all)", name, strings.Join(dependencyNames(), ", "))
		}
		path, found := findExecutable(spec.Executable)
		if found == nil && !force {
			results = append(results, map[string]any{"dependency": spec.Name, "status": "already installed", "path": path})
			continue
		}
		installer, installerArgs, err := dependencyInstallCommand(spec.Name)
		if err != nil {
			return nil, err
		}
		if err := runInteractive(opts, installer, installerArgs...); err != nil {
			return nil, fmt.Errorf("install %s: %w", spec.Name, err)
		}
		status := "installed"
		if opts.dryRun {
			status = "would install"
		}
		results = append(results, map[string]any{"dependency": spec.Name, "status": status})
	}
	return results, nil
}

func dependencyNames() []string {
	names := make([]string, 0, len(dependencySpecs))
	for _, spec := range dependencySpecs {
		names = append(names, spec.Name)
	}
	return names
}

// missingDependencies returns the named dependencies whose executables are not
// on PATH.
func missingDependencies(names []string) []string {
	var missing []string
	for _, name := range names {
		spec, ok := dependencyByName(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		if _, err := findExecutable(spec.Executable); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

func dependencyInstallCommand(name string) (string, []string, error) {
	switch name {
	case "basecamp":
		return "go", []string{"install", "github.com/basecamp/basecamp-cli/cmd/basecamp@latest"}, nil
	case "codex":
		return "npm", []string{"install", "--global", "@openai/codex@latest"}, nil
	case "claude":
		return "sh", []string{"-c", "curl -fsSL https://claude.ai/install.sh | sh"}, nil
	case "railway":
		return "sh", []string{"-c", "curl -fsSL https://railway.com/install.sh | sh"}, nil
	case "tailscale":
		return "sh", []string{"-c", "curl -fsSL https://tailscale.com/install.sh | sh"}, nil
	case "git", "gh":
		if _, err := exec.LookPath("brew"); err == nil {
			return "brew", []string{"install", name}, nil
		}
		if runtime.GOOS == "linux" {
			if _, err := exec.LookPath("apt-get"); err == nil {
				return "sudo", []string{"apt-get", "install", "-y", name}, nil
			}
			if _, err := exec.LookPath("dnf"); err == nil {
				return "sudo", []string{"dnf", "install", "-y", name}, nil
			}
			if _, err := exec.LookPath("pacman"); err == nil {
				return "sudo", []string{"pacman", "-S", "--needed", name}, nil
			}
		}
	}
	return "", nil, fmt.Errorf("no supported installer found for %s on %s", name, runtime.GOOS)
}

func newDependenciesCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "dependencies", Aliases: []string{"deps"}, Short: "Inspect dependency CLIs"}
	cmd.AddCommand(&cobra.Command{Use: "check", Short: "Check dependency CLI availability", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		results := make([]map[string]any, 0, len(dependencySpecs))
		for _, spec := range dependencySpecs {
			path, err := findExecutable(spec.Executable)
			results = append(results, map[string]any{"dependency": spec.Name, "installed": err == nil, "path": path, "purpose": spec.Purpose})
		}
		return printDependencyResults(opts, results)
	}})
	return cmd
}

func printDependencyResults(opts *cliOptions, results []map[string]any) error {
	if opts.jsonOutput {
		return printValue(opts, results)
	}
	if _, err := fmt.Fprintln(opts.out, "Dependencies:"); err != nil {
		return err
	}
	for _, result := range results {
		name, _ := result["dependency"].(string)
		status, _ := result["status"].(string)
		if status == "" {
			if installed, _ := result["installed"].(bool); installed {
				status = "installed at " + fmt.Sprint(result["path"])
			} else {
				status = "missing"
			}
		}
		if _, err := fmt.Fprintf(opts.out, "  %s: %s\n", name, status); err != nil {
			return err
		}
	}
	return nil
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
	return filepath.Join(dir, "basecamp-agent", "config.json")
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Listen: "127.0.0.1:8789", BasecampBin: "basecamp", CodexBin: "codex", ClaudeBin: "claude",
		WorkDir: filepath.Join(home, "Projects"), StatePath: filepath.Join(home, ".local/state/basecamp-agent/state.json"),
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
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var normalized any
	if err := json.Unmarshal(b, &normalized); err != nil {
		return err
	}
	var output strings.Builder
	writeHumanValue(&output, normalized, 0)
	_, err = fmt.Fprint(opts.out, output.String())
	return err
}

func writeHumanValue(out *strings.Builder, value any, indent int) {
	padding := strings.Repeat("  ", indent)
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			fmt.Fprintf(out, "%s(none)\n", padding)
			return
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := v[key]
			if isHumanScalar(child) {
				fmt.Fprintf(out, "%s%s: %s\n", padding, humanLabel(key), humanScalar(child))
				continue
			}
			fmt.Fprintf(out, "%s%s:\n", padding, humanLabel(key))
			writeHumanValue(out, child, indent+1)
		}
	case []any:
		if len(v) == 0 {
			fmt.Fprintf(out, "%s(none)\n", padding)
			return
		}
		for _, child := range v {
			if isHumanScalar(child) {
				fmt.Fprintf(out, "%s- %s\n", padding, humanScalar(child))
				continue
			}
			fmt.Fprintf(out, "%s-\n", padding)
			writeHumanValue(out, child, indent+1)
		}
	default:
		fmt.Fprintf(out, "%s%s\n", padding, humanScalar(v))
	}
}

func isHumanScalar(value any) bool {
	switch value.(type) {
	case nil, string, bool, float64:
		return true
	default:
		return false
	}
}

func humanLabel(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func humanScalar(value any) string {
	switch v := value.(type) {
	case nil:
		return "(none)"
	case bool:
		if v {
			return "yes"
		}
		return "no"
	case string:
		if v == "" {
			return "(none)"
		}
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func printAgents(opts *cliOptions, cfg Config) error {
	if opts.jsonOutput {
		return printValue(opts, map[string]any{"bot_ids": cfg.BotIDs, "bot_profiles": cfg.BotProfiles})
	}
	names := make([]string, 0, len(cfg.BotIDs))
	for name := range cfg.BotIDs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		_, err := fmt.Fprintln(opts.out, "No agents enabled.")
		return err
	}
	if _, err := fmt.Fprintln(opts.out, "Enabled agents:"); err != nil {
		return err
	}
	for _, name := range names {
		profile := cfg.BotProfiles[name]
		if profile == "" {
			profile = "(not configured)"
		}
		if _, err := fmt.Fprintf(opts.out, "  %s\n    Person ID: %d\n    Basecamp profile: %s\n", humanLabel(name), cfg.BotIDs[name], profile); err != nil {
			return err
		}
	}
	return nil
}

func printRepos(opts *cliOptions, repos AllowedRepoList) error {
	if opts.jsonOutput {
		return printValue(opts, repos)
	}
	if len(repos) == 0 {
		_, err := fmt.Fprintln(opts.out, "No repositories configured.")
		return err
	}
	if _, err := fmt.Fprintln(opts.out, "Configured repositories:"); err != nil {
		return err
	}
	for _, repo := range repos {
		if _, err := fmt.Fprintf(opts.out, "  %s\n    Path: %s\n", repo.Name, repo.Path); err != nil {
			return err
		}
		if len(repo.Aliases) > 0 {
			if _, err := fmt.Fprintf(opts.out, "    Aliases: %s\n", strings.Join(repo.Aliases, ", ")); err != nil {
				return err
			}
		}
		if r := repo.Railway; r.Project != "" {
			base := r.PreviewBaseEnvironment
			if base == "" {
				base = "staging"
			}
			line := "    Railway previews: " + r.Service + " (base: " + base + ")"
			if _, err := fmt.Fprintln(opts.out, line); err != nil {
				return err
			}
		}
	}
	return nil
}

func printGitHubWebhookResults(opts *cliOptions, results []map[string]any, remove bool) error {
	if opts.jsonOutput {
		return printValue(opts, results)
	}
	if len(results) == 0 {
		_, err := fmt.Fprintln(opts.out, "No repositories configured.")
		return err
	}
	if _, err := fmt.Fprintln(opts.out, "GitHub webhooks:"); err != nil {
		return err
	}
	for _, result := range results {
		repo, _ := result["repo"].(string)
		changed, _ := result["changed"].(bool)
		status := "already configured"
		if remove {
			status = "not found"
		}
		if changed {
			if opts.dryRun && remove {
				status = "would remove"
			} else if opts.dryRun {
				status = "would sync"
			} else if remove {
				status = "removed"
			} else {
				status = "synced"
			}
		}
		if _, err := fmt.Fprintf(opts.out, "  %s: %s\n", repo, status); err != nil {
			return err
		}
	}
	return nil
}

func printChecks(opts *cliOptions, checks map[string]any) error {
	if opts.jsonOutput {
		return printValue(opts, checks)
	}
	labels := map[string]string{"basecamp": "Basecamp CLI", "claude": "Claude CLI", "codex": "Codex CLI", "config": "Config file", "configuration": "Configuration", "gh": "GitHub CLI", "git": "Git"}
	keys := make([]string, 0, len(checks))
	for key := range checks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if _, err := fmt.Fprintln(opts.out, "Checks:"); err != nil {
		return err
	}
	for _, key := range keys {
		label := labels[key]
		if label == "" {
			label = humanLabel(key)
		}
		values, _ := checks[key].(map[string]any)
		ok, _ := values["ok"].(bool)
		detail, _ := values["path"].(string)
		if message, _ := values["error"].(string); message != "" {
			detail = message
		}
		mark := "✓"
		if !ok {
			mark = "✗"
			if detail == "" {
				detail = "not available"
			}
		}
		if detail == "" {
			if _, err := fmt.Fprintf(opts.out, "  %s %s\n", mark, label); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(opts.out, "  %s %s: %s\n", mark, label, detail); err != nil {
			return err
		}
	}
	return nil
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
		if opts.jsonOutput {
			return printValue(opts, map[string]any{"ok": true, "config": opts.configPath})
		}
		_, err = fmt.Fprintf(opts.out, "Configuration is valid: %s\n", opts.configPath)
		return err
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
	var forceSecret bool
	secretGenerate := &cobra.Command{Use: "generate <ops-token>", Short: "Generate and store an operator dashboard token", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] != "ops-token" {
			return errors.New("supported generated secret: ops-token")
		}
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		if cfg.Ops.Token != "" && !forceSecret {
			return errors.New("ops token already exists (use --force to rotate it)")
		}
		cfg.Ops.Token, err = randomSecret(32)
		if err != nil {
			return err
		}
		if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "secret": "ops-token", "stored": true, "config": opts.configPath})
	}}
	secretGenerate.Flags().BoolVar(&forceSecret, "force", false, "rotate an existing token")
	secret := &cobra.Command{Use: "secret", Short: "Manage locally stored secrets"}
	secret.AddCommand(secretGenerate)
	cmd.AddCommand(initCmd, show, validate, set, secret)
	return cmd
}

func newAgentCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Manage enabled coding agents"}
	cmd.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		return printAgents(opts, cfg)
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
	return runInteractiveInDir(opts, "", name, args...)
}

func runInteractiveInDir(opts *cliOptions, dir, name string, args ...string) error {
	if opts.dryRun {
		if dir != "" {
			_, _ = fmt.Fprintf(opts.out, "+ (in %s) %s %s\n", dir, name, strings.Join(args, " "))
		} else {
			_, _ = fmt.Fprintf(opts.out, "+ %s %s\n", name, strings.Join(args, " "))
		}
		return nil
	}
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Stdin, c.Stdout, c.Stderr = opts.in, opts.out, opts.errOut
	return c.Run()
}

// commandOutput runs a command and returns its stdout. Stderr is kept out of
// the result, because shims and wrappers print progress there and callers
// parse the output, and it is reported only when the command fails.
func commandOutput(name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()+"\n"+string(out)))
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
		return addRepoConfig(opts, path, name, aliases)
	}}
	add.Flags().StringVar(&name, "name", "", "configured repository name")
	add.Flags().StringSliceVar(&aliases, "alias", nil, "additional name used to select this repository")
	var createName, owner, visibility string
	var createAliases []string
	var push bool
	create := &cobra.Command{Use: "create <folder>", Short: "Create a GitHub repository and add it to the allowlist", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := filepath.Abs(expandPath(args[0]))
		if err != nil {
			return err
		}
		if createName == "" {
			createName = filepath.Base(path)
		}
		slug := createName
		if owner != "" {
			slug = owner + "/" + createName
		}
		ghArgs := []string{"repo", "create", slug, "--source", path, "--remote", "origin", "--" + visibility}
		if push {
			ghArgs = append(ghArgs, "--push")
		}
		if err := runInteractive(opts, "gh", ghArgs...); err != nil {
			return fmt.Errorf("create GitHub repository: %w", err)
		}
		if push {
			// Worktrees branch from origin/HEAD, which only a clone sets up.
			if err := runInteractive(opts, "git", "-C", path, "remote", "set-head", "origin", "--auto"); err != nil {
				return fmt.Errorf("set origin/HEAD: %w", err)
			}
		}
		return addRepoConfig(opts, path, createName, createAliases)
	}}
	create.Flags().StringVar(&createName, "name", "", "GitHub and configured repository name")
	create.Flags().StringVar(&owner, "owner", "", "GitHub owner or organization")
	create.Flags().StringVar(&visibility, "visibility", "private", "repository visibility: private, public, or internal")
	create.Flags().StringSliceVar(&createAliases, "alias", nil, "additional name used to select this repository")
	create.Flags().BoolVar(&push, "push", true, "push local commits after creating the repository")
	create.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if visibility != "private" && visibility != "public" && visibility != "internal" {
			return errors.New("--visibility must be private, public, or internal")
		}
		return nil
	}
	repos := &cobra.Command{Use: "repo", Short: "Manage local repositories"}
	repos.AddCommand(add, create, &cobra.Command{Use: "list", Short: "List configured repositories", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		return printRepos(opts, cfg.AllowedRepos)
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

func addRepoConfig(opts *cliOptions, path, name string, aliases []string) error {
	cfg, err := readOrDefaultConfig(opts.configPath)
	if err != nil {
		return err
	}
	for i, repo := range cfg.AllowedRepos {
		if repo.Name == name || repo.Path == path {
			cfg.AllowedRepos[i].Name = name
			cfg.AllowedRepos[i].Path = path
			if len(aliases) > 0 {
				cfg.AllowedRepos[i].Aliases = aliases
			}
			if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
				return err
			}
			return printValue(opts, map[string]any{"ok": true, "repo": name, "path": path, "updated": true})
		}
	}
	cfg.AllowedRepos = append(cfg.AllowedRepos, AllowedRepo{Name: name, Path: path, Aliases: aliases})
	if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
		return err
	}
	return printValue(opts, map[string]any{"ok": true, "repo": name, "path": path})
}

func newLegacyRailwayCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "railway", Short: "Manage Railway through the Railway CLI"}
	cmd.AddCommand(&cobra.Command{Use: "login", Short: "Authenticate or create a Railway account", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if opts.nonInteractive {
			return errors.New("railway login requires an interactive terminal")
		}
		return runInteractive(opts, "railway", "login")
	}})
	var configureProject, configureService, configureEnvironment, configureDomain string
	configure := &cobra.Command{Use: "configure [folder]", Short: "Store Railway context for a configured repository", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		folder := ""
		if len(args) == 1 {
			folder = args[0]
		}
		repoIndex, _, err := resolveConfiguredRepo(cfg, folder)
		if err != nil {
			return err
		}
		repo := &cfg.AllowedRepos[repoIndex]
		if configureProject != "" {
			repo.Railway.Project = configureProject
		}
		if configureService != "" {
			repo.Railway.Service = configureService
		}
		if configureEnvironment != "" {
			repo.Railway.Environment = configureEnvironment
		}
		if configureDomain != "" {
			repo.Railway.Domain = strings.TrimRight(configureDomain, "/")
		}
		if repo.Railway.Project == "" || repo.Railway.Service == "" || repo.Railway.Environment == "" {
			return errors.New("Railway project, service, and environment are required (from flags or existing global config)")
		}
		if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "repo": repo.Name, "railway": repo.Railway})
	}}
	configure.Flags().StringVar(&configureProject, "project", "", "Railway project ID")
	configure.Flags().StringVar(&configureService, "service", "", "Railway service name or ID")
	configure.Flags().StringVar(&configureEnvironment, "environment", "", "Railway environment name or ID")
	configure.Flags().StringVar(&configureDomain, "domain", "", "public Railway domain")
	cmd.AddCommand(configure)
	var newProject, yes, detach bool
	var name, project, service, environment string
	deploy := &cobra.Command{Use: "deploy [folder]", Short: "Deploy a repository with Railway CLI", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		folder := ""
		if len(args) == 1 {
			folder = args[0]
		}
		repoIndex := -1
		cfg, configErr := loadConfig(opts.configPath)
		if configErr == nil {
			index, repo, err := resolveConfiguredRepo(cfg, folder)
			if err != nil && !newProject {
				return err
			}
			if err == nil {
				repoIndex = index
				folder = repo.Path
				if project == "" {
					project = repo.Railway.Project
				}
				if service == "" {
					service = repo.Railway.Service
				}
				if environment == "" {
					environment = repo.Railway.Environment
				}
			}
		} else if !errors.Is(configErr, os.ErrNotExist) {
			return configErr
		}
		deployDir, err := filepath.Abs(expandPath(folder))
		if err != nil {
			return err
		}
		deployArgs := []string{"up"}
		if newProject {
			deployArgs = append(deployArgs, "--new")
		}
		if yes || opts.nonInteractive {
			deployArgs = append(deployArgs, "--yes")
		}
		if detach {
			deployArgs = append(deployArgs, "--detach")
		}
		if name != "" {
			deployArgs = append(deployArgs, "--name", name)
		}
		if service != "" {
			deployArgs = append(deployArgs, "--service", service)
		}
		if project != "" {
			deployArgs = append(deployArgs, "--project", project)
		}
		if environment != "" {
			deployArgs = append(deployArgs, "--environment", environment)
		}
		if err := runInteractiveInDir(opts, deployDir, "railway", deployArgs...); err != nil {
			return err
		}
		if !newProject || opts.dryRun || repoIndex < 0 {
			return nil
		}
		// Keep the global config the source of truth: store what --new created,
		// so later deploys of this repository need no flags.
		created, err := linkedRailwayContext(deployDir)
		if err != nil {
			return fmt.Errorf("deployed, but could not read the new Railway project (store it with `basecamp-agent railway configure`): %w", err)
		}
		created.Domain = cfg.AllowedRepos[repoIndex].Railway.Domain
		cfg.AllowedRepos[repoIndex].Railway = created
		if err := writeConfig(opts.configPath, cfg, false, opts.out); err != nil {
			return err
		}
		if err := connectRailwayGitHub(opts, cfg.AllowedRepos[repoIndex], true); err != nil {
			_, _ = fmt.Fprintf(opts.errOut, "warning: %v\nRun `basecamp-agent railway connect %s` to retry.\n", err, cfg.AllowedRepos[repoIndex].Path)
		}
		return printValue(opts, map[string]any{"repo": cfg.AllowedRepos[repoIndex].Name, "railway": created})
	}}
	deploy.Flags().BoolVar(&newProject, "new", false, "create a new Railway project and service")
	deploy.Flags().BoolVarP(&yes, "yes", "y", false, "accept defaults and skip surrounding prompts")
	deploy.Flags().BoolVar(&detach, "detach", false, "return after starting the deployment")
	deploy.Flags().StringVar(&name, "name", "", "name for a newly created Railway project")
	deploy.Flags().StringVar(&project, "project", "", "Railway project ID")
	deploy.Flags().StringVar(&service, "service", "", "Railway service")
	deploy.Flags().StringVar(&environment, "environment", "", "Railway environment")
	cmd.AddCommand(deploy)
	var domainProject, domainService, domainEnvironment string
	var domainPort int
	domain := &cobra.Command{Use: "domain [domain]", Short: "Create a Railway-provided or custom public domain", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		domainArgs := []string{"domain"}
		if len(args) == 1 {
			domainArgs = append(domainArgs, args[0])
		}
		if domainPort != 0 {
			domainArgs = append(domainArgs, "--port", strconv.Itoa(domainPort))
		}
		if domainService != "" {
			domainArgs = append(domainArgs, "--service", domainService)
		}
		if domainEnvironment != "" {
			domainArgs = append(domainArgs, "--environment", domainEnvironment)
		}
		if domainProject != "" {
			domainArgs = append(domainArgs, "--project", domainProject)
		}
		if opts.jsonOutput {
			domainArgs = append(domainArgs, "--json")
		}
		return runInteractive(opts, "railway", domainArgs...)
	}}
	domain.Flags().IntVar(&domainPort, "port", 0, "service port to expose")
	domain.Flags().StringVar(&domainService, "service", "", "Railway service name or ID")
	domain.Flags().StringVar(&domainEnvironment, "environment", "", "Railway environment name or ID")
	domain.Flags().StringVar(&domainProject, "project", "", "Railway project ID")
	cmd.AddCommand(domain)
	var noPREnvironments bool
	connect := &cobra.Command{Use: "connect [folder]", Short: "Deploy a repository's Railway service from GitHub, with PR environments", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		folder := ""
		if len(args) == 1 {
			folder = args[0]
		}
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		_, repo, err := resolveConfiguredRepo(cfg, folder)
		if err != nil {
			return err
		}
		if err := connectRailwayGitHub(opts, repo, !noPREnvironments); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "repo": repo.Name, "pr_environments": !noPREnvironments})
	}}
	connect.Flags().BoolVar(&noPREnvironments, "no-pr-environments", false, "do not give pull requests their own Railway environment")
	cmd.AddCommand(connect)
	cmd.AddCommand(&cobra.Command{Use: "status", Short: "Show the linked Railway context", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		args := []string{"status"}
		if opts.jsonOutput {
			args = append(args, "--json")
		}
		return runInteractive(opts, "railway", args...)
	}})
	return cmd
}

func resolveConfiguredRepo(cfg Config, folder string) (int, AllowedRepo, error) {
	if folder == "" {
		var err error
		folder, err = os.Getwd()
		if err != nil {
			return -1, AllowedRepo{}, err
		}
	}
	abs, err := filepath.Abs(expandPath(folder))
	if err != nil {
		return -1, AllowedRepo{}, err
	}
	best := -1
	bestLength := -1
	for i, repo := range cfg.AllowedRepos {
		root, err := filepath.Abs(expandPath(repo.Path))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(root) > bestLength {
			best, bestLength = i, len(root)
		}
	}
	if best < 0 {
		return -1, AllowedRepo{}, fmt.Errorf("no globally configured repository matches %s", abs)
	}
	return best, cfg.AllowedRepos[best], nil
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
		return printGitHubWebhookResults(opts, results, remove)
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
	cmd := &cobra.Command{Use: "basecamp", Short: "Manage Basecamp profiles and webhooks"}
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
		if opts.jsonOutput {
			return printValue(opts, map[string]any{"ok": true, "projects": len(cfg.AllowedProjectIDs), "removed": remove})
		}
		action := "synced"
		if opts.dryRun && remove {
			action = "would be removed from"
		} else if opts.dryRun {
			action = "would be synced for"
		} else if remove {
			action = "removed from"
		}
		_, err = fmt.Fprintf(opts.out, "Basecamp webhook %s %d project(s).\n", action, len(cfg.AllowedProjectIDs))
		return err
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
		if configErr == nil {
			var urlErr error
			if cfg.PublicURL == "" {
				urlErr = errors.New("not configured; Basecamp cannot deliver webhooks until `basecamp-agent endpoint set <https-url>` is run")
			} else {
				urlErr = checkEndpointHealth(cfg.PublicURL)
			}
			checks["public_url"] = map[string]any{"ok": urlErr == nil, "url": cfg.PublicURL, "error": errorString(urlErr)}
			failed = failed || urlErr != nil
		}
		failed = failed || configErr != nil
		_ = printChecks(opts, checks)
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
			return runInteractive(opts, "systemctl", "--user", action, "basecamp-agent.service")
		}}
	}
	install := &cobra.Command{Use: "install", Short: "Install and enable the systemd user service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return installSystemdService(opts)
	}}
	uninstall := &cobra.Command{Use: "uninstall", Short: "Disable and remove the systemd user service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return removeUserUnit(opts, "basecamp-agent.service")
	}}
	cmd.AddCommand(install, uninstall, serviceAction("start"), serviceAction("stop"), serviceAction("restart"), serviceAction("status"))
	return cmd
}

func installSystemdService(opts *cliOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	body := fmt.Sprintf("[Unit]\nDescription=Basecamp webhook local coding agent dispatcher\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=%s serve --config %s\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n", strconv.Quote(exe), strconv.Quote(opts.configPath))
	return installUserUnit(opts, "basecamp-agent.service", body)
}

// installUserUnit writes a systemd user unit, then enables and (re)starts it.
func installUserUnit(opts *cliOptions, name, body string) error {
	if opts.dryRun {
		_, err := fmt.Fprint(opts.out, body)
		return err
	}
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".config/systemd/user", name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		return err
	}
	if err := runInteractive(opts, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := runInteractive(opts, "systemctl", "--user", "enable", name); err != nil {
		return err
	}
	return runInteractive(opts, "systemctl", "--user", "restart", name)
}

// removeUserUnit disables, stops, and deletes a systemd user unit.
func removeUserUnit(opts *cliOptions, name string) error {
	if err := runInteractive(opts, "systemctl", "--user", "disable", "--now", name); err != nil && !opts.dryRun {
		return err
	}
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".config/systemd/user", name)
	if opts.dryRun {
		_, _ = fmt.Fprintf(opts.out, "remove %s\n", path)
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return runInteractive(opts, "systemctl", "--user", "daemon-reload")
}

func newSetupCommand(opts *cliOptions) *cobra.Command {
	var setup setupOptions
	cmd := &cobra.Command{Use: "setup", Short: "Configure the agent interactively or from composable options", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if opts.nonInteractive || setup.repo != "" || setup.project != 0 || setup.account != 0 {
			return runConfiguredSetup(opts, setup)
		}
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		reader := bufio.NewReader(opts.in)
		fmt.Fprintln(opts.out, "Basecamp agent setup (press Enter to keep a displayed value)")
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

		installNow, err := promptBool(reader, opts.out, "Install and start the systemd user service", true)
		if err != nil {
			return err
		}
		if installNow {
			if err := installSystemdService(opts); err != nil {
				return err
			}
		}
		if err := promptPublicURL(cmd.Context(), opts, reader); err != nil {
			return err
		}
		setupCardColumns(opts)
		fmt.Fprintln(opts.out, "Setup complete. Run `basecamp-agent doctor` at any time to verify it.")
		return nil
	}}
	flags := cmd.Flags()
	flags.StringVar(&setup.repo, "repo", "", "local repository folder")
	flags.StringVar(&setup.repoName, "repo-name", "", "configured repository name")
	flags.StringSliceVar(&setup.repoAliases, "repo-alias", nil, "additional repository selection name")
	flags.StringVar(&setup.publicURL, "public-url", "", "public HTTPS URL that forwards to this agent")
	flags.Int64Var(&setup.account, "account", 0, "Basecamp account ID")
	flags.Int64Var(&setup.project, "project", 0, "Basecamp project ID")
	flags.Int64Var(&setup.creator, "creator", 0, "trusted requester person ID")
	flags.Int64Var(&setup.codexPersonID, "codex-person-id", 0, "Codex bot Basecamp person ID")
	flags.StringVar(&setup.codexProfile, "codex-profile", "codex-bot", "Codex bot Basecamp CLI profile")
	flags.Int64Var(&setup.claudePersonID, "claude-person-id", 0, "Claude bot Basecamp person ID")
	flags.StringVar(&setup.claudeProfile, "claude-profile", "claude-bot", "Claude bot Basecamp CLI profile")
	flags.BoolVar(&setup.syncWebhooks, "sync-webhooks", false, "create or update webhooks for the current public_url")
	flags.BoolVar(&setup.installService, "install-service", false, "install and start the systemd user service")
	return cmd
}

type setupOptions struct {
	repo, repoName, publicURL, codexProfile, claudeProfile string
	repoAliases                                            []string
	account, project, creator                              int64
	codexPersonID, claudePersonID                          int64
	syncWebhooks, installService                           bool
}

func runConfiguredSetup(opts *cliOptions, setup setupOptions) error {
	cfg, err := readOrDefaultConfig(opts.configPath)
	if err != nil {
		return err
	}
	if setup.publicURL != "" {
		if err := validPublicURL(setup.publicURL); err != nil {
			return err
		}
	}
	if cfg.BotIDs == nil {
		cfg.BotIDs = map[string]int64{}
	}
	if cfg.BotProfiles == nil {
		cfg.BotProfiles = map[string]string{}
	}
	if cfg.ProjectRepos == nil {
		cfg.ProjectRepos = map[string]string{}
	}
	if setup.account != 0 {
		cfg.AllowedAccountID = setup.account
	}
	if setup.project != 0 {
		cfg.AllowedProjectIDs = appendUniqueInt64(cfg.AllowedProjectIDs, setup.project)
	}
	if setup.creator != 0 {
		cfg.AllowedCreatorIDs = appendUniqueInt64(cfg.AllowedCreatorIDs, setup.creator)
	}
	if setup.codexPersonID != 0 {
		cfg.BotIDs["codex"] = setup.codexPersonID
		cfg.BotProfiles["codex"] = setup.codexProfile
	}
	if setup.claudePersonID != 0 {
		cfg.BotIDs["claude"] = setup.claudePersonID
		cfg.BotProfiles["claude"] = setup.claudeProfile
	}
	if setup.repo != "" {
		path, err := filepath.Abs(expandPath(setup.repo))
		if err != nil {
			return err
		}
		if _, err := commandOutput("git", "-C", path, "rev-parse", "--show-toplevel"); err != nil {
			return err
		}
		if setup.repoName == "" {
			setup.repoName = filepath.Base(path)
		}
		updated := false
		for i, repo := range cfg.AllowedRepos {
			if repo.Name == setup.repoName || repo.Path == path {
				cfg.AllowedRepos[i].Name = setup.repoName
				cfg.AllowedRepos[i].Path = path
				// Keep existing aliases unless new ones are given.
				if len(setup.repoAliases) > 0 {
					cfg.AllowedRepos[i].Aliases = setup.repoAliases
				}
				updated = true
				break
			}
		}
		if !updated {
			cfg.AllowedRepos = append(cfg.AllowedRepos, AllowedRepo{Name: setup.repoName, Path: path, Aliases: setup.repoAliases})
		}
		if setup.project != 0 {
			cfg.ProjectRepos[strconv.FormatInt(setup.project, 10)] = setup.repoName
		}
	}
	if setup.syncWebhooks && setup.publicURL == "" && cfg.PublicURL == "" {
		return errors.New("--public-url or an existing public_url is required with --sync-webhooks")
	}
	if setup.syncWebhooks && cfg.GitHub.WebhookSecret == "" {
		cfg.GitHub.WebhookSecret, err = randomSecret(32)
		if err != nil {
			return err
		}
	}
	if err := validateConfig(cfg); err != nil {
		return fmt.Errorf("setup configuration: %w", err)
	}
	if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
		return err
	}
	if setup.installService {
		if err := installSystemdService(opts); err != nil {
			return err
		}
	}
	if setup.publicURL != "" {
		if err := switchPublicURL(context.Background(), opts, cfg, setup.publicURL, setup.syncWebhooks); err != nil {
			return err
		}
	} else if setup.syncWebhooks {
		if err := syncPublicWebhooks(opts, cfg, cfg.PublicURL, false); err != nil {
			return err
		}
	}
	if setup.syncWebhooks && setup.project != 0 && cfg.Cards.MoveEnabled {
		results, err := ensureCardColumns(opts, cfg, setup.project)
		if err != nil {
			return fmt.Errorf("card columns: %w", err)
		}
		if err := printColumnResults(opts, setup.project, results); err != nil {
			return err
		}
	}
	return printValue(opts, map[string]any{"ok": true, "config": opts.configPath, "repo": setup.repoName})
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

// findExecutable looks on PATH and then in ~/.local/bin, where several
// dependency installers write and which is often missing from PATH right after
// an install.
func findExecutable(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".local/bin", name)
	if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
		return path, nil
	}
	return "", fmt.Errorf("%s not found; run `basecamp-agent install %s`", name, name)
}

// linkedRailwayContext reads the project, service, and environment that the
// Railway CLI linked to dir, which `railway up --new` does for the project it
// creates. It needs exactly one service to be unambiguous.
func linkedRailwayContext(dir string) (RailwayRepoConfig, error) {
	c := exec.Command("railway", "status", "--json")
	c.Dir = dir
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return RailwayRepoConfig{}, fmt.Errorf("railway status: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseRailwayStatus(out)
}

type railwayNodes struct {
	Edges []struct {
		Node struct {
			Name string `json:"name"`
		} `json:"node"`
	} `json:"edges"`
}

func parseRailwayStatus(b []byte) (RailwayRepoConfig, error) {
	var status struct {
		ID           string       `json:"id"`
		Services     railwayNodes `json:"services"`
		Environments railwayNodes `json:"environments"`
	}
	if err := json.Unmarshal(b, &status); err != nil {
		return RailwayRepoConfig{}, err
	}
	if status.ID == "" || len(status.Services.Edges) != 1 || len(status.Environments.Edges) == 0 {
		return RailwayRepoConfig{}, fmt.Errorf("expected one linked project with one service, found %d service(s)", len(status.Services.Edges))
	}
	ctx := RailwayRepoConfig{Project: status.ID, Service: status.Services.Edges[0].Node.Name, Environment: status.Environments.Edges[0].Node.Name}
	for _, env := range status.Environments.Edges {
		if env.Node.Name == "production" {
			ctx.Environment = env.Node.Name
		}
	}
	return ctx, nil
}

// connectRailwayGitHub makes a repository's Railway service deploy from its
// GitHub repository's default branch and, with prEnvironments, gives every
// pull request its own preview environment.
func connectRailwayGitHub(opts *cliOptions, repo AllowedRepo, prEnvironments bool) error {
	r := repo.Railway
	if r.Project == "" || r.Service == "" || r.Environment == "" {
		return fmt.Errorf("repository %s has no Railway project, service, and environment; run `basecamp-agent railway deploy %s --new` or `basecamp-agent railway configure`", repo.Name, repo.Path)
	}
	slug, err := githubSlug(repo.Path)
	if err != nil {
		return err
	}
	head, err := commandOutput("git", "-C", repo.Path, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return fmt.Errorf("find the default branch: %w", err)
	}
	branch := strings.TrimPrefix(strings.TrimSpace(string(head)), "origin/")
	if err := runInteractiveInDir(opts, repo.Path, "railway", "service", "source", "connect", "--repo", slug, "--branch", branch, "--service", r.Service, "--project", r.Project, "--environment", r.Environment); err != nil {
		return fmt.Errorf("connect %s to Railway: %w", slug, err)
	}
	if !prEnvironments {
		return nil
	}
	mutation := `mutation($id: String!) { projectUpdate(id: $id, input: {prDeploys: true}) { id prDeploys } }`
	if err := runInteractiveInDir(opts, repo.Path, "railway", "api", mutation, "--raw-var", "id="+r.Project); err != nil {
		return fmt.Errorf("enable Railway PR environments: %w", err)
	}
	return nil
}
