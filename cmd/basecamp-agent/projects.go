package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newProjectCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Manage the Basecamp projects the agent works for"}

	var account, project, creator int64
	var defaultRepo string
	var noSync, noColumns bool
	add := &cobra.Command{
		Use:   "add",
		Short: "Allow a Basecamp project and register its webhook",
		Long: `Allow a Basecamp project, trust a requester in it, and optionally set the
repository its jobs use. When a public URL is configured, the project's Basecamp
webhook is registered too. Missing card columns the agent moves cards through
are created unless --no-columns is passed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if account == 0 || project == 0 || creator == 0 {
				return errors.New("--account, --project, and --creator are required")
			}
			cfg, err := readOrDefaultConfig(opts.configPath)
			if err != nil {
				return err
			}
			if defaultRepo != "" && !hasRepo(cfg, defaultRepo) {
				return fmt.Errorf("repository %q is not configured; add it with `basecamp-agent github repo add <folder> --name %s`", defaultRepo, defaultRepo)
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
			if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
				return err
			}
			if noSync {
				return nil
			}
			if cfg.PublicURL != "" {
				if err := reconcileProjectWebhook(opts, cfg, project, false); err != nil {
					return fmt.Errorf("project saved, but its webhook could not be registered (are the bots members of the project?): %w", err)
				}
			}
			if noColumns || !cfg.Cards.MoveEnabled {
				return nil
			}
			results, err := ensureCardColumns(opts, cfg, project)
			if err != nil {
				return fmt.Errorf("project saved, but its card columns could not be set up: %w", err)
			}
			return printColumnResults(opts, project, results)
		},
	}
	add.Flags().Int64Var(&account, "account", 0, "Basecamp account ID")
	add.Flags().Int64Var(&project, "project", 0, "Basecamp project ID")
	add.Flags().Int64Var(&creator, "creator", 0, "trusted requester person ID")
	add.Flags().StringVar(&defaultRepo, "default-repo", "", "repository jobs use when the item names none")
	add.Flags().BoolVar(&noSync, "no-sync", false, "only save the config; do not register the webhook or create card columns")
	add.Flags().BoolVar(&noColumns, "no-columns", false, "do not create missing card columns")

	list := &cobra.Command{Use: "list", Short: "List configured projects with their repositories and dashboards", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		return printProjects(opts, cfg)
	}}

	var keepWebhook bool
	remove := &cobra.Command{Use: "remove <project-id>", Short: "Stop working for a project and remove its webhook", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		cfg, err := readOrDefaultConfig(opts.configPath)
		if err != nil {
			return err
		}
		if !keepWebhook && cfg.PublicURL != "" && containsInt64(cfg.AllowedProjectIDs, id) {
			if err := reconcileProjectWebhook(opts, cfg, id, true); err != nil {
				return fmt.Errorf("remove webhook (pass --keep-webhook to skip): %w", err)
			}
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
	}}
	remove.Flags().BoolVar(&keepWebhook, "keep-webhook", false, "leave the project's Basecamp webhook in place")

	columns := &cobra.Command{Use: "columns <project-id>", Short: "Create any card columns the agent needs that the project lacks", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		results, err := ensureCardColumns(opts, cfg, id)
		if err != nil {
			return err
		}
		return printColumnResults(opts, id, results)
	}}

	cmd.AddCommand(add, list, remove, columns)
	return cmd
}

func reconcileProjectWebhook(opts *cliOptions, cfg Config, project int64, remove bool) error {
	profile := defaultWebhookProfile(cfg)
	if profile == "" {
		return errors.New("an enabled agent profile is required to manage Basecamp webhooks")
	}
	return reconcileBasecampWebhook(opts, cfg.BasecampBin, profile, project, cfg.PublicURL+"/webhook", remove)
}

func printProjects(opts *cliOptions, cfg Config) error {
	rows := make([]map[string]any, 0, len(cfg.AllowedProjectIDs))
	for _, id := range cfg.AllowedProjectIDs {
		key := strconv.FormatInt(id, 10)
		row := map[string]any{"project": id, "account": cfg.AllowedAccountID, "repo": cfg.ProjectRepos[key], "basecamp": fmt.Sprintf("https://app.basecamp.com/%d/projects/%d", cfg.AllowedAccountID, id)}
		if cfg.Ops.Enabled && cfg.PublicURL != "" {
			row["dashboard"] = opsDashboardURL(cfg.PublicURL, id, "")
		}
		rows = append(rows, row)
	}
	if opts.jsonOutput {
		return printValue(opts, rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(opts.out, "No projects configured. Add one with `basecamp-agent project add`.")
		return err
	}
	for _, row := range rows {
		repo, _ := row["repo"].(string)
		if repo == "" {
			repo = "(none; items must name a repository)"
		}
		if _, err := fmt.Fprintf(opts.out, "Project %d\n  Repository: %s\n  Basecamp:   %s\n", row["project"], repo, row["basecamp"]); err != nil {
			return err
		}
		if dashboard, ok := row["dashboard"].(string); ok {
			if _, err := fmt.Fprintf(opts.out, "  Dashboard:  %s\n", dashboard); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasRepo(cfg Config, name string) bool {
	for _, repo := range cfg.AllowedRepos {
		if repo.Name == name {
			return true
		}
	}
	return false
}

type basecampColumn struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

func listCardColumns(cfg Config, profile string, project int64) ([]basecampColumn, error) {
	b, err := commandOutput(basecampBin(cfg), "-P", profile, "cards", "columns", "--in", strconv.FormatInt(project, 10), "--agent")
	if err != nil {
		return nil, err
	}
	var columns []basecampColumn
	if err := json.Unmarshal(b, &columns); err != nil {
		return nil, fmt.Errorf("read card columns: %w", err)
	}
	return columns, nil
}

// ensureCardColumns creates the card columns the agent moves cards through
// when the project's card table lacks them. New columns go just before the
// Done column, in lifecycle order.
func ensureCardColumns(opts *cliOptions, cfg Config, project int64) ([]map[string]any, error) {
	profile := defaultWebhookProfile(cfg)
	if profile == "" {
		return nil, errors.New("an enabled agent profile is required to manage card columns")
	}
	columns, err := listCardColumns(cfg, profile, project)
	if err != nil {
		return nil, err
	}
	var results []map[string]any
	seen := map[string]bool{}
	for _, want := range []string{cfg.Cards.Failed, cfg.Cards.InProgress, cfg.Cards.PROpen, cfg.Cards.Done} {
		key := strings.ToLower(strings.TrimSpace(want))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if columnIndex(columns, want) >= 0 {
			results = append(results, map[string]any{"column": want, "status": "exists"})
			continue
		}
		if opts.dryRun {
			results = append(results, map[string]any{"column": want, "status": "would create"})
			continue
		}
		projectArg := strconv.FormatInt(project, 10)
		b, err := commandOutput(basecampBin(cfg), "-P", profile, "cards", "column", "create", want, "--in", projectArg, "--agent")
		if err != nil {
			return results, err
		}
		var created basecampColumn
		if err := json.Unmarshal(b, &created); err != nil {
			return results, fmt.Errorf("read created column: %w", err)
		}
		if done := doneColumnIndex(columns, cfg.Cards.Done); done >= 0 && !sameColumn(want, cfg.Cards.Done) {
			if _, err := commandOutput(basecampBin(cfg), "-P", profile, "cards", "column", "move", strconv.FormatInt(created.ID, 10), "--position", strconv.Itoa(done), "--in", projectArg, "--agent"); err != nil {
				return results, fmt.Errorf("position column %q: %w", want, err)
			}
		}
		results = append(results, map[string]any{"column": want, "status": "created"})
		if columns, err = listCardColumns(cfg, profile, project); err != nil {
			return results, err
		}
	}
	return results, nil
}

func columnIndex(columns []basecampColumn, title string) int {
	for i, c := range columns {
		if sameColumn(c.Title, title) {
			return i
		}
	}
	return -1
}

// doneColumnIndex finds Basecamp's Done column. Its index in the listing is
// the position that places a column immediately before it.
func doneColumnIndex(columns []basecampColumn, doneTitle string) int {
	for i, c := range columns {
		if c.Type == "Kanban::DoneColumn" {
			return i
		}
	}
	return columnIndex(columns, doneTitle)
}

func basecampBin(cfg Config) string {
	if cfg.BasecampBin == "" {
		return "basecamp"
	}
	return cfg.BasecampBin
}

func printColumnResults(opts *cliOptions, project int64, results []map[string]any) error {
	if opts.jsonOutput {
		return printValue(opts, map[string]any{"project": project, "columns": results})
	}
	if _, err := fmt.Fprintf(opts.out, "Card columns for project %d:\n", project); err != nil {
		return err
	}
	for _, r := range results {
		if _, err := fmt.Fprintf(opts.out, "  %s: %s\n", r["column"], r["status"]); err != nil {
			return err
		}
	}
	return nil
}

// setupCardColumns is the setup wizard's column step. A failure only warns,
// because the usual cause is bots that are not project members yet.
func setupCardColumns(opts *cliOptions) {
	cfg, err := readOrDefaultConfig(opts.configPath)
	if err != nil || !cfg.Cards.MoveEnabled {
		return
	}
	for _, project := range cfg.AllowedProjectIDs {
		results, err := ensureCardColumns(opts, cfg, project)
		if err != nil {
			_, _ = fmt.Fprintf(opts.errOut, "warning: card columns for project %d: %v\nAdd the bots to the project, then run `basecamp-agent project columns %d`.\n", project, err, project)
			continue
		}
		_ = printColumnResults(opts, project, results)
	}
}
