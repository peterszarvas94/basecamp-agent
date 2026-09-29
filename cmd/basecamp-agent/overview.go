package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func newListCommand(opts *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show everything configured: public URL, projects, repositories, agents, and service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts.configPath)
			if err != nil {
				return err
			}
			service := userServiceState("basecamp-agent.service")
			ops := "disabled"
			if cfg.Ops.Enabled && cfg.Ops.Token != "" {
				ops = "enabled, token set (`basecamp-agent ops url` prints links)"
			} else if cfg.Ops.Enabled {
				ops = "enabled, no token (run `basecamp-agent config secret generate ops-token`)"
			}
			if opts.jsonOutput {
				projects := make([]map[string]any, 0, len(cfg.AllowedProjectIDs))
				for _, id := range cfg.AllowedProjectIDs {
					projects = append(projects, map[string]any{"project": id, "repo": cfg.ProjectRepos[fmt.Sprint(id)]})
				}
				return printValue(opts, map[string]any{
					"config":     opts.configPath,
					"public_url": cfg.PublicURL,
					"ops":        map[string]any{"enabled": cfg.Ops.Enabled, "token_set": cfg.Ops.Token != ""},
					"service":    service,
					"account":    cfg.AllowedAccountID,
					"projects":   projects,
					"requesters": cfg.AllowedCreatorIDs,
					"repos":      cfg.AllowedRepos,
					"agents":     cfg.BotIDs,
				})
			}
			publicURL := cfg.PublicURL
			if publicURL == "" {
				publicURL = "not set; Basecamp cannot deliver webhooks until `basecamp-agent endpoint set <https-url>`"
			}
			requesters := make([]string, 0, len(cfg.AllowedCreatorIDs))
			for _, id := range cfg.AllowedCreatorIDs {
				requesters = append(requesters, fmt.Sprint(id))
			}
			if _, err := fmt.Fprintf(opts.out, "Config:     %s\nPublic URL: %s\nDashboard:  %s\nService:    %s\nRequesters: %s\n\nProjects (account %d):\n", opts.configPath, publicURL, ops, service, emptyDash(strings.Join(requesters, ", ")), cfg.AllowedAccountID); err != nil {
				return err
			}
			if err := printProjects(opts, cfg); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(opts.out); err != nil {
				return err
			}
			if err := printRepos(opts, cfg.AllowedRepos); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(opts.out); err != nil {
				return err
			}
			return printAgents(opts, cfg)
		},
	}
}

// userServiceState reports a systemd user unit's state without failing when
// systemd is unavailable.
func userServiceState(unit string) string {
	// is-active exits non-zero for inactive units but still prints the state.
	out, _ := exec.Command("systemctl", "--user", "is-active", unit).Output()
	if state := strings.TrimSpace(string(out)); state != "" {
		return state
	}
	return "unknown"
}
