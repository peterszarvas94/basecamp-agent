package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const missingEndpointMessage = "public URL is not configured; run `basecamp-agent endpoint set https://agent.example.com`"

func newEndpointCommand(opts *cliOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "endpoint", Short: "Manage the public URL Basecamp and GitHub send webhooks to"}

	var noSync bool
	set := &cobra.Command{
		Use:   "set <https-url>",
		Short: "Store the public URL and move webhooks to it",
		Long: `Store the public HTTPS URL that forwards to this agent's listen address.
Webhooks registered for the previous URL are removed, and Basecamp and GitHub
webhooks are synced to the new one. The URL must already forward to this
machine; see the README for tunnel options if you do not have a domain.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validPublicURL(args[0]); err != nil {
				return err
			}
			cfg, err := readOrDefaultConfig(opts.configPath)
			if err != nil {
				return err
			}
			return switchPublicURL(cmd.Context(), opts, cfg, args[0], !noSync)
		},
	}
	set.Flags().BoolVar(&noSync, "no-sync", false, "store the URL without reconciling webhooks")

	var keepWebhooks bool
	remove := &cobra.Command{Use: "remove", Short: "Remove the public URL's webhooks and clear it", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		if cfg.PublicURL == "" {
			return printValue(opts, map[string]any{"ok": true, "removed": false})
		}
		if !keepWebhooks {
			if err := syncPublicWebhooks(opts, cfg, cfg.PublicURL, true); err != nil {
				return fmt.Errorf("remove webhooks: %w", err)
			}
		}
		previous := cfg.PublicURL
		cfg.PublicURL = ""
		if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "removed": previous})
	}}
	remove.Flags().BoolVar(&keepWebhooks, "keep-webhooks", false, "leave Basecamp and GitHub webhooks in place")

	show := &cobra.Command{Use: "show", Short: "Show the configured public URL", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		if cfg.PublicURL == "" {
			return errors.New(missingEndpointMessage)
		}
		return printValue(opts, map[string]any{"public_url": cfg.PublicURL})
	}}
	check := &cobra.Command{Use: "check", Short: "Check that the public URL reaches the agent", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		if cfg.PublicURL == "" {
			return errors.New(missingEndpointMessage)
		}
		if err := checkEndpointHealth(cfg.PublicURL); err != nil {
			return err
		}
		return printValue(opts, map[string]any{"ok": true, "public_url": cfg.PublicURL, "health": "ok"})
	}}
	cmd.AddCommand(set, remove, show, check)
	return cmd
}

// promptPublicURL is the setup wizard's endpoint step.
func promptPublicURL(ctx context.Context, opts *cliOptions, reader *bufio.Reader) error {
	cfg, err := readOrDefaultConfig(opts.configPath)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(opts.out, "\nBasecamp and GitHub need a public HTTPS URL that forwards to "+cfg.Listen+".")
	_, _ = fmt.Fprintln(opts.out, "Without a domain, use a tunnel such as Tailscale Funnel or ngrok (see the README).")
	publicURL, err := prompt(reader, opts.out, "Public URL (leave empty to set it later)", cfg.PublicURL)
	if err != nil {
		return err
	}
	if publicURL == "" {
		_, _ = fmt.Fprintln(opts.out, "WARNING: without a public URL Basecamp cannot deliver webhooks, so the agent will never receive work.\nRun `basecamp-agent endpoint set <https-url>` once the URL forwards to this machine.")
		return nil
	}
	if err := validPublicURL(publicURL); err != nil {
		return err
	}
	return switchPublicURL(ctx, opts, cfg, publicURL, true)
}

// switchPublicURL stores publicURL, checks that it answers, and moves Basecamp
// and GitHub webhooks from the previous public_url when sync is set.
func switchPublicURL(ctx context.Context, opts *cliOptions, cfg Config, publicURL string, sync bool) error {
	previous := cfg.PublicURL
	cfg.PublicURL = strings.TrimRight(publicURL, "/")
	if sync && cfg.GitHub.WebhookSecret == "" {
		secret, err := randomSecret(32)
		if err != nil {
			return err
		}
		cfg.GitHub.WebhookSecret = secret
	}
	if err := writeConfig(opts.configPath, cfg, opts.dryRun, opts.out); err != nil {
		return err
	}
	if !opts.jsonOutput && !opts.dryRun {
		_, _ = fmt.Fprintf(opts.out, "Public URL: %s\n", cfg.PublicURL)
	}
	health := "ok"
	if !opts.dryRun {
		if err := checkEndpointHealth(cfg.PublicURL); err != nil {
			health = err.Error()
			_, _ = fmt.Fprintf(opts.errOut, "warning: %v (does it forward to %s, and is the agent running?)\n", err, cfg.Listen)
		}
	}
	if sync {
		if previous != "" && previous != cfg.PublicURL {
			if err := syncPublicWebhooks(opts, cfg, previous, true); err != nil {
				_, _ = fmt.Fprintf(opts.errOut, "warning: remove webhooks for previous URL %s: %v\n", previous, err)
			}
		}
		if err := syncPublicWebhooks(opts, cfg, cfg.PublicURL, false); err != nil {
			return fmt.Errorf("sync webhooks: %w", err)
		}
	}
	if opts.jsonOutput {
		return printValue(opts, map[string]any{"public_url": cfg.PublicURL, "health": health, "webhooks_synced": sync})
	}
	if sync && !opts.dryRun {
		_, err := fmt.Fprintf(opts.out, "Webhooks synced for %d Basecamp project(s) and %d repository(ies).\n", len(cfg.AllowedProjectIDs), len(cfg.AllowedRepos))
		return err
	}
	return nil
}

// syncPublicWebhooks reconciles the Basecamp and GitHub webhooks that point at
// publicURL for every configured project and repository.
func syncPublicWebhooks(opts *cliOptions, cfg Config, publicURL string, remove bool) error {
	publicURL = strings.TrimRight(publicURL, "/")
	if len(cfg.AllowedProjectIDs) > 0 {
		profile := defaultWebhookProfile(cfg)
		if profile == "" {
			return errors.New("an enabled agent profile is required to manage Basecamp webhooks")
		}
		for _, id := range cfg.AllowedProjectIDs {
			if err := reconcileBasecampWebhook(opts, cfg.BasecampBin, profile, id, publicURL+"/webhook", remove); err != nil {
				return err
			}
		}
	}
	for _, repo := range cfg.AllowedRepos {
		slug, err := githubSlug(repo.Path)
		if err != nil {
			return err
		}
		if _, err := reconcileGitHubWebhook(opts, slug, publicURL+"/github/webhook", cfg.GitHub.WebhookSecret, remove); err != nil {
			return err
		}
	}
	return nil
}

func defaultWebhookProfile(cfg Config) string {
	if profile := cfg.BotProfiles["codex"]; profile != "" {
		return profile
	}
	names := make([]string, 0, len(cfg.BotProfiles))
	for name, profile := range cfg.BotProfiles {
		if profile != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return cfg.BotProfiles[names[0]]
}

func validPublicURL(raw string) error {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("public URL must be an HTTPS origin, for example https://agent.example.com")
	}
	return nil
}

func checkEndpointHealth(publicURL string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimRight(publicURL, "/") + "/")
	if err != nil {
		return fmt.Errorf("check public URL: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s: %q", publicURL, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
