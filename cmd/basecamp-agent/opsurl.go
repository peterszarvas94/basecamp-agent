package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
)

func newOpsCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "ops", Short: "Open the ops dashboard"}
	var project int64
	var noToken bool
	urlCmd := &cobra.Command{
		Use:   "url",
		Short: "Print dashboard URLs, with the ops token, for all jobs and each project",
		Long: `Print the ops dashboard URL for all jobs and for each configured Basecamp
project. The URLs include the ops token so they open directly; the dashboard
swaps the token for a session cookie on first visit. Treat the output as a
secret, or pass --no-token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts.configPath)
			if err != nil {
				return err
			}
			if !cfg.Ops.Enabled {
				return errors.New("the ops dashboard is disabled (ops.enabled is false in the config)")
			}
			if cfg.PublicURL == "" {
				return errors.New(missingEndpointMessage)
			}
			token := cfg.Ops.Token
			if noToken {
				token = ""
			} else if token == "" {
				_, _ = fmt.Fprintln(opts.errOut, "warning: no ops token is set, so anyone with the URL can open the dashboard; run `basecamp-agent config secret generate ops-token`")
			}
			projects := cfg.AllowedProjectIDs
			if project != 0 {
				projects = []int64{project}
			}
			rows := make([]map[string]any, 0, len(projects)+1)
			if project == 0 {
				rows = append(rows, map[string]any{"scope": "all projects", "url": opsDashboardURL(cfg.PublicURL, 0, token)})
			}
			for _, id := range projects {
				row := map[string]any{"scope": "project " + strconv.FormatInt(id, 10), "project": id, "url": opsDashboardURL(cfg.PublicURL, id, token)}
				if repo := cfg.ProjectRepos[strconv.FormatInt(id, 10)]; repo != "" {
					row["repo"] = repo
				}
				rows = append(rows, row)
			}
			if opts.jsonOutput {
				return printValue(opts, rows)
			}
			for _, row := range rows {
				label := row["scope"].(string)
				if repo, ok := row["repo"].(string); ok {
					label += " (" + repo + ")"
				}
				if _, err := fmt.Fprintf(opts.out, "%s: %s\n", label, row["url"]); err != nil {
					return err
				}
			}
			return nil
		},
	}
	urlCmd.Flags().Int64Var(&project, "project", 0, "print only this Basecamp project's URL")
	urlCmd.Flags().BoolVar(&noToken, "no-token", false, "leave the token out of the URLs")
	cmd.AddCommand(urlCmd)
	return cmd
}

func opsDashboardURL(publicURL string, project int64, token string) string {
	u := publicURL + "/ops"
	if project != 0 {
		u += "/projects/" + strconv.FormatInt(project, 10)
	}
	if token != "" {
		u += "?token=" + url.QueryEscape(token)
	}
	return u
}
