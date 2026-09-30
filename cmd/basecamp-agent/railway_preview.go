package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

const railwayPreviewQuery = `query($id: String!) { project(id: $id) { id prDeploys botPrEnvironments baseEnvironmentId baseEnvironment { id name isEphemeral } environments(isEphemeral: false) { edges { node { id name isEphemeral } } } } }`

type railwayEnvironment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IsEphemeral bool   `json:"isEphemeral"`
}

type railwayPreviewState struct {
	ID                     string               `json:"id"`
	PRDeploys              bool                 `json:"prDeploys"`
	BotPREnvironments      bool                 `json:"botPrEnvironments"`
	BaseEnvironmentID      string               `json:"baseEnvironmentId"`
	BaseEnvironment        railwayEnvironment   `json:"baseEnvironment"`
	PersistentEnvironments []railwayEnvironment `json:"persistent_environments"`
}

func newRailwayCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "railway", Short: "Configure safe Railway previews for agent pull requests"}
	cmd.AddCommand(&cobra.Command{Use: "login", Short: "Authenticate or create a Railway account", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		if opts.nonInteractive {
			return errors.New("railway login requires an interactive terminal")
		}
		return runInteractive(opts, "railway", "login")
	}})
	cmd.AddCommand(newRailwayPreviewCommand(opts))
	return cmd
}

func newRailwayPreviewCommand(opts *cliOptions) *cobra.Command {
	preview := &cobra.Command{Use: "preview", Short: "Set up and inspect pull-request preview environments"}
	var project, service, base string
	var confirmSafe bool
	setup := &cobra.Command{Use: "setup [folder]", Short: "Use a persistent non-production environment as the PR preview base", Args: cobra.MaximumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		cfg, index, repo, err := railwayPreviewRepo(opts, args, project, service, base)
		if err != nil {
			return err
		}
		if !confirmSafe {
			return errors.New("refusing to enable PR previews without --confirm-safe-secrets; replace inherited production keys and integrations in the preview base first")
		}
		if err := setupRailwayPreview(opts, repo); err != nil {
			return err
		}
		cfg.AllowedRepos[index].Railway = repo.Railway
		if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "repo": repo.Name, "preview_base_environment": repo.Railway.PreviewBaseEnvironment})
	}}
	setup.Flags().StringVar(&project, "project", "", "Railway project ID")
	setup.Flags().StringVar(&service, "service", "", "Railway service name or ID")
	setup.Flags().StringVar(&base, "base", "", "persistent preview base environment (default staging)")
	setup.Flags().BoolVar(&confirmSafe, "confirm-safe-secrets", false, "confirm the preview base no longer contains production credentials")
	preview.AddCommand(setup)

	var prepareProject, prepareService, prepareBase, prepareSource string
	prepare := &cobra.Command{Use: "prepare [folder]", Short: "Disable PR previews and create their persistent staging base", Args: cobra.MaximumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		cfg, index, repo, err := railwayPreviewRepo(opts, args, prepareProject, prepareService, prepareBase)
		if err != nil {
			return err
		}
		created, err := prepareRailwayPreview(opts, repo, prepareSource)
		if err != nil {
			return err
		}
		cfg.AllowedRepos[index].Railway = repo.Railway
		if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "repo": repo.Name, "preview_base_environment": repo.Railway.PreviewBaseEnvironment, "created": created, "pr_environments": false, "next": "replace inherited production secrets, then run preview setup --confirm-safe-secrets"})
	}}
	prepare.Flags().StringVar(&prepareProject, "project", "", "Railway project ID")
	prepare.Flags().StringVar(&prepareService, "service", "", "Railway service name or ID")
	prepare.Flags().StringVar(&prepareBase, "base", "", "persistent preview base environment (default staging)")
	prepare.Flags().StringVar(&prepareSource, "source", "production", "persistent environment whose service topology is copied")
	preview.AddCommand(prepare)

	preview.AddCommand(railwayPreviewReadCommand(opts, "status", false))
	preview.AddCommand(railwayPreviewReadCommand(opts, "audit", true))
	preview.AddCommand(&cobra.Command{Use: "disable [folder]", Short: "Disable automatic Railway PR environments", Args: cobra.MaximumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		_, _, repo, err := railwayPreviewRepo(opts, args, "", "", "")
		if err != nil {
			return err
		}
		mutation := `mutation($id: String!) { projectUpdate(id: $id, input: { prDeploys: false, botPrEnvironments: false }) { id prDeploys botPrEnvironments } }`
		if err := runInteractiveInDir(opts, repo.Path, "railway", "api", mutation, "--raw-var", "id="+repo.Railway.Project); err != nil {
			return fmt.Errorf("disable Railway PR previews: %w", err)
		}
		return printValue(opts, map[string]any{"ok": true, "repo": repo.Name, "pr_environments": false})
	}})
	return preview
}

func prepareRailwayPreview(opts *cliOptions, repo AllowedRepo, source string) (bool, error) {
	disable := `mutation($id: String!) { projectUpdate(id: $id, input: { prDeploys: false, botPrEnvironments: false }) { id prDeploys botPrEnvironments } }`
	if err := runInteractiveInDir(opts, repo.Path, "railway", "api", disable, "--raw-var", "id="+repo.Railway.Project); err != nil {
		return false, fmt.Errorf("disable unsafe PR previews: %w", err)
	}
	base := previewBase(repo.Railway)
	if opts.dryRun {
		mutation := `mutation($project: String!, $name: String!, $source: String!) { environmentCreate(input: { projectId: $project, name: $name, sourceEnvironmentId: $source, skipInitialDeploys: true }) { id name } }`
		return true, runInteractiveInDir(opts, repo.Path, "railway", "api", mutation, "--raw-var", "project="+repo.Railway.Project, "--raw-var", "name="+base, "--raw-var", "source=<resolved-"+source+"-environment-id>")
	}
	state, err := readRailwayPreviewState(repo.Path, repo.Railway.Project)
	if err != nil {
		return false, err
	}
	for _, env := range state.PersistentEnvironments {
		if env.ID == base || strings.EqualFold(env.Name, base) {
			return false, nil
		}
	}
	sourceID := ""
	for _, env := range state.PersistentEnvironments {
		if env.ID == source || strings.EqualFold(env.Name, source) {
			sourceID = env.ID
			break
		}
	}
	if sourceID == "" {
		return false, fmt.Errorf("persistent Railway source environment %q not found", source)
	}
	mutation := `mutation($project: String!, $name: String!, $source: String!) { environmentCreate(input: { projectId: $project, name: $name, sourceEnvironmentId: $source, skipInitialDeploys: true }) { id name } }`
	if err := runInteractiveInDir(opts, repo.Path, "railway", "api", mutation, "--raw-var", "project="+repo.Railway.Project, "--raw-var", "name="+base, "--raw-var", "source="+sourceID); err != nil {
		return false, fmt.Errorf("create Railway preview base: %w", err)
	}
	return true, nil
}

func railwayPreviewReadCommand(opts *cliOptions, name string, audit bool) *cobra.Command {
	return &cobra.Command{Use: name + " [folder]", Short: map[bool]string{false: "Show Railway PR preview configuration", true: "Verify Railway PR previews use the configured safe base"}[audit], Args: cobra.MaximumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		_, _, repo, err := railwayPreviewRepo(opts, args, "", "", "")
		if err != nil {
			return err
		}
		if opts.dryRun {
			return runInteractiveInDir(opts, repo.Path, "railway", "api", railwayPreviewQuery, "--raw-var", "id="+repo.Railway.Project)
		}
		state, err := readRailwayPreviewState(repo.Path, repo.Railway.Project)
		if err != nil {
			return err
		}
		want := previewBase(repo.Railway)
		result := map[string]any{"repo": repo.Name, "project": repo.Railway.Project, "service": repo.Railway.Service, "configured_base": want, "railway": state}
		if audit {
			var problems []string
			if !state.PRDeploys {
				problems = append(problems, "PR environments are disabled")
			}
			if !state.BotPREnvironments {
				problems = append(problems, "bot PR environments are disabled")
			}
			if state.BaseEnvironment.IsEphemeral {
				problems = append(problems, "the base environment is ephemeral")
			}
			if strings.EqualFold(state.BaseEnvironment.Name, "production") {
				problems = append(problems, "the base environment is production")
			}
			if !strings.EqualFold(state.BaseEnvironment.Name, want) && state.BaseEnvironment.ID != want {
				problems = append(problems, fmt.Sprintf("Railway base %q does not match configured base %q", state.BaseEnvironment.Name, want))
			}
			result["ok"] = len(problems) == 0
			result["problems"] = problems
			if err := printValue(opts, result); err != nil {
				return err
			}
			if len(problems) > 0 {
				return errors.New("Railway preview audit failed")
			}
			return nil
		}
		return printValue(opts, result)
	}}
}

func railwayPreviewRepo(opts *cliOptions, args []string, project, service, base string) (Config, int, AllowedRepo, error) {
	cfg, err := loadConfig(opts.configPath)
	if err != nil {
		return cfg, -1, AllowedRepo{}, err
	}
	folder := ""
	if len(args) == 1 {
		folder = args[0]
	}
	index, repo, err := resolveConfiguredRepo(cfg, folder)
	if err != nil {
		return cfg, -1, AllowedRepo{}, err
	}
	if project != "" {
		repo.Railway.Project = project
	}
	if service != "" {
		repo.Railway.Service = service
	}
	if base != "" {
		repo.Railway.PreviewBaseEnvironment = base
	}
	if repo.Railway.PreviewBaseEnvironment == "" {
		repo.Railway.PreviewBaseEnvironment = "staging"
	}
	repo.Railway.Environment, repo.Railway.Domain = "", ""
	if strings.EqualFold(repo.Railway.PreviewBaseEnvironment, "production") {
		return cfg, -1, AllowedRepo{}, errors.New("production cannot be used as the Railway PR preview base")
	}
	if repo.Railway.Project == "" || repo.Railway.Service == "" {
		return cfg, -1, AllowedRepo{}, errors.New("Railway project and service are required; pass --project and --service to `railway preview setup`")
	}
	return cfg, index, repo, nil
}

func previewBase(r RailwayRepoConfig) string {
	if r.PreviewBaseEnvironment == "" {
		return "staging"
	}
	return r.PreviewBaseEnvironment
}

func setupRailwayPreview(opts *cliOptions, repo AllowedRepo) error {
	base := previewBase(repo.Railway)
	baseID := "<resolved-" + base + "-environment-id>"
	if !opts.dryRun {
		state, err := readRailwayPreviewState(repo.Path, repo.Railway.Project)
		if err != nil {
			return err
		}
		baseID = ""
		for _, env := range state.PersistentEnvironments {
			if env.ID == base || strings.EqualFold(env.Name, base) {
				baseID = env.ID
				base = env.Name
				break
			}
		}
		if baseID == "" {
			return fmt.Errorf("persistent Railway environment %q not found; create and safely configure it before enabling PR previews", base)
		}
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
	if err := runInteractiveInDir(opts, repo.Path, "railway", "service", "source", "connect", "--repo", slug, "--branch", branch, "--service", repo.Railway.Service, "--project", repo.Railway.Project, "--environment", base); err != nil {
		return err
	}
	mutation := `mutation($id: String!, $base: String!) { projectUpdate(id: $id, input: { baseEnvironmentId: $base, prDeploys: true, botPrEnvironments: true }) { id prDeploys botPrEnvironments baseEnvironmentId } }`
	return runInteractiveInDir(opts, repo.Path, "railway", "api", mutation, "--raw-var", "id="+repo.Railway.Project, "--raw-var", "base="+baseID)
}

func readRailwayPreviewState(dir, project string) (railwayPreviewState, error) {
	out, err := commandOutputInDir(dir, "railway", "api", railwayPreviewQuery, "--raw-var", "id="+project)
	if err != nil {
		return railwayPreviewState{}, err
	}
	var response struct {
		Data struct {
			Project struct {
				ID                string             `json:"id"`
				PRDeploys         bool               `json:"prDeploys"`
				BotPREnvironments bool               `json:"botPrEnvironments"`
				BaseEnvironmentID string             `json:"baseEnvironmentId"`
				BaseEnvironment   railwayEnvironment `json:"baseEnvironment"`
				Environments      struct {
					Edges []struct {
						Node railwayEnvironment `json:"node"`
					} `json:"edges"`
				} `json:"environments"`
			} `json:"project"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return railwayPreviewState{}, fmt.Errorf("decode Railway preview status: %w", err)
	}
	p := response.Data.Project
	state := railwayPreviewState{ID: p.ID, PRDeploys: p.PRDeploys, BotPREnvironments: p.BotPREnvironments, BaseEnvironmentID: p.BaseEnvironmentID, BaseEnvironment: p.BaseEnvironment}
	for _, edge := range p.Environments.Edges {
		state.PersistentEnvironments = append(state.PersistentEnvironments, edge.Node)
	}
	if state.ID == "" {
		return state, errors.New("Railway project not found")
	}
	return state, nil
}

func commandOutputInDir(dir, name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()+"\n"+string(out)))
	}
	return out, nil
}
