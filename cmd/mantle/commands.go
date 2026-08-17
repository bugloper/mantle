package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// --- organizations ---

func newOrgCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "org", Short: "Manage organizations"}

	list := &cobra.Command{
		Use:   "list",
		Short: "List organizations",
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var response struct {
				Organizations []struct {
					Slug         string `json:"slug"`
					DisplayName  string `json:"display_name"`
					QuotaBytes   *int64 `json:"quota_bytes"`
					Repositories int    `json:"repositories"`
				} `json:"organizations"`
			}
			if err := c.get("/api/v1/organizations", &response); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, response)
			}
			t := newTable("slug", "display name", "repositories", "quota")
			for _, o := range response.Organizations {
				quota := dim("unlimited")
				if o.QuotaBytes != nil {
					quota = humanBytes(*o.QuotaBytes)
				}
				t.add(cyan(o.Slug), o.DisplayName, fmt.Sprint(o.Repositories), quota)
			}
			t.render(os.Stdout)
			return nil
		},
	}

	var displayName string
	var quota string
	create := &cobra.Command{
		Use:   "create <slug>",
		Short: "Create an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			body := map[string]any{"slug": args[0], "display_name": displayName}
			if quota != "" {
				parsed, err := parseBytes(quota)
				if err != nil {
					return err
				}
				body["quota_bytes"] = parsed
			}
			if err := c.post("/api/v1/organizations", body, nil); err != nil {
				return err
			}
			fmt.Printf("%s created organization %s\n", statusMark(true), cyan(args[0]))
			return nil
		},
	}
	create.Flags().StringVar(&displayName, "display-name", "", "human-readable name")
	create.Flags().StringVar(&quota, "quota", "", "storage quota, e.g. 100GiB")

	cmd.AddCommand(list, create)
	return cmd
}

// --- users ---

func newUserCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "Manage user accounts"}

	list := &cobra.Command{
		Use:   "list",
		Short: "List users",
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var response struct {
				Users []identityJSON `json:"users"`
			}
			if err := c.get("/api/v1/users", &response); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, response)
			}
			t := newTable("name", "email", "admin", "status", "last used")
			for _, u := range response.Users {
				t.add(cyan(u.Name), u.Email, boolMark(u.Admin), statusWord(u.Disabled),
					humanAgePtr(u.LastUsedAt))
			}
			t.render(os.Stdout)
			return nil
		},
	}

	var email, password string
	var admin bool
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if password == "" {
				password = os.Getenv("MANTLE_NEW_PASSWORD")
			}
			if password == "" {
				return fmt.Errorf(
					"no password supplied\n" +
						"  Pass --password, or set MANTLE_NEW_PASSWORD to keep it out of\n" +
						"  your shell history.")
			}
			body := map[string]any{
				"name": args[0], "email": email,
				"password": password, "instance_admin": admin,
			}
			if err := c.post("/api/v1/users", body, nil); err != nil {
				return err
			}
			fmt.Printf("%s created user %s\n", statusMark(true), cyan(args[0]))
			return nil
		},
	}
	create.Flags().StringVar(&email, "email", "", "email address")
	create.Flags().StringVar(&password, "password", "", "initial password (prefer $MANTLE_NEW_PASSWORD)")
	create.Flags().BoolVar(&admin, "admin", false, "grant instance administrator rights")

	cmd.AddCommand(list, create)
	return cmd
}

// --- tokens ---

func newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage deploy tokens, robots, and PATs"}

	list := &cobra.Command{
		Use:   "list",
		Short: "List machine credentials",
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var response struct {
				Tokens []identityJSON `json:"tokens"`
			}
			if err := c.get("/api/v1/tokens", &response); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, response)
			}
			t := newTable("name", "kind", "status", "expires", "last used", "uuid")
			for _, tok := range response.Tokens {
				expires := dim("never")
				if tok.ExpiresAt != nil {
					expires = humanAge(*tok.ExpiresAt)
				}
				t.add(cyan(tok.Name), tok.Kind, statusWord(tok.Disabled), expires,
					humanAgePtr(tok.LastUsedAt), dim(tok.UUID))
			}
			t.render(os.Stdout)
			return nil
		},
	}

	var org, namespace, role, kind string
	var expiresIn int
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a machine credential",
		Long: `Create a deploy token, robot account, or personal access token.

The secret is printed once and cannot be retrieved afterwards. The default role
is 'reader', which is pull-only — pass --role contributor for a builder.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if org == "" {
				return fmt.Errorf("--org is required")
			}
			body := map[string]any{
				"name": args[0], "kind": kind, "organization": org,
				"namespace": namespace, "role": role, "expires_in_days": expiresIn,
			}
			var created struct {
				Name   string `json:"name"`
				Kind   string `json:"kind"`
				UUID   string `json:"uuid"`
				Secret string `json:"secret"`
			}
			if err := c.post("/api/v1/tokens", body, &created); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, created)
			}

			scope := namespace
			if scope == "" {
				scope = org + "/"
			}
			fmt.Printf("%s created %s %s (%s on %s)\n",
				statusMark(true), created.Kind, cyan(created.Name), role, scope)
			fmt.Println()
			fmt.Printf("  %s\n", bold(created.Secret))
			fmt.Println()
			fmt.Println(yellow("  This secret is shown once and cannot be retrieved again."))
			fmt.Printf("  %s\n", dim("docker login "+trimScheme(c.baseURL)+" -u "+created.Name+" -p <secret>"))
			return nil
		},
	}
	create.Flags().StringVar(&org, "org", "", "organization the credential belongs to (required)")
	create.Flags().StringVar(&namespace, "namespace", "", "repository prefix it may access (default: <org>/)")
	create.Flags().StringVar(&role, "role", "reader", "reader, contributor, maintainer, or owner")
	create.Flags().StringVar(&kind, "kind", "deploy_token", "deploy_token, robot, or pat")
	create.Flags().IntVar(&expiresIn, "expires-in-days", 0, "expiry in days (0 = never)")

	revoke := &cobra.Command{
		Use:   "revoke <uuid>",
		Short: "Revoke a machine credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if !confirm(fmt.Sprintf("Revoke credential %s?", args[0])) {
				fmt.Println("cancelled")
				return nil
			}
			var result struct {
				Revoked string `json:"revoked"`
				Note    string `json:"note"`
			}
			if err := c.delete("/api/v1/tokens/"+args[0], &result); err != nil {
				return err
			}
			fmt.Printf("%s revoked %s\n", statusMark(true), cyan(result.Revoked))
			if result.Note != "" {
				fmt.Printf("  %s\n", dim(result.Note))
			}
			return nil
		},
	}

	cmd.AddCommand(list, create, revoke)
	return cmd
}

// --- repositories ---

func newRepoCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "Manage repositories"}

	list := &cobra.Command{
		Use:   "list",
		Short: "List repositories",
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var response struct {
				Repositories []repositoryJSON `json:"repositories"`
			}
			if err := c.get("/api/v1/repositories", &response); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, response)
			}
			t := newTable("repository", "visibility", "tags", "manifests", "size")
			for _, r := range response.Repositories {
				visibility := r.Visibility
				if visibility == "public" {
					visibility = yellow(visibility)
				}
				t.add(cyan(r.Name), visibility, fmt.Sprint(r.Tags),
					fmt.Sprint(r.Manifests), humanBytes(r.UsedBytes))
			}
			t.render(os.Stdout)
			return nil
		},
	}

	var createVisibility string
	var createImmutable bool
	create := &cobra.Command{
		Use:   "create <repository>",
		Short: "Create an empty repository",
		Long: `Create a repository before anything is pushed to it.

Pushing to a name creates it automatically, so this is only needed when you want
its visibility or tag policy settled before the first image arrives.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if createVisibility == "public" && !confirm(
				fmt.Sprintf("Create %s as public, readable by anyone who can reach this registry?", args[0])) {
				fmt.Println("cancelled")
				return nil
			}
			body := map[string]any{
				"name":           args[0],
				"visibility":     createVisibility,
				"immutable_tags": createImmutable,
			}
			var repo repositoryJSON
			if err := c.post("/api/v1/repositories", body, &repo); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, repo)
			}
			fmt.Printf("%s created %s (%s)\n", statusMark(true), cyan(repo.Name), repo.Visibility)
			if repo.ImmutableTags {
				fmt.Printf("  %s\n", dim("tags are immutable: once set, a tag cannot be moved or deleted"))
			}
			return nil
		},
	}
	create.Flags().StringVar(&createVisibility, "visibility", "private", "private or public")
	create.Flags().BoolVar(&createImmutable, "immutable-tags", false, "make tags immutable from the outset")

	show := &cobra.Command{
		Use:   "show <repository>",
		Short: "Show a repository's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var repo repositoryJSON
			if err := c.get("/api/v1/repositories/"+args[0], &repo); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, repo)
			}
			fmt.Printf("%s\n", bold(repo.Name))
			fmt.Printf("  organization   %s\n", repo.Organization)
			fmt.Printf("  visibility     %s\n", repo.Visibility)
			fmt.Printf("  immutable tags %s\n", boolMark(repo.ImmutableTags))
			if repo.SourceURL != "" {
				fmt.Printf("  source         %s\n", repo.SourceURL)
			}
			fmt.Printf("  tags           %d\n", repo.Tags)
			fmt.Printf("  manifests      %d\n", repo.Manifests)
			fmt.Printf("  storage        %s\n", humanBytes(repo.UsedBytes))
			return nil
		},
	}

	visibility := &cobra.Command{
		Use:   "visibility <repository> <public|private>",
		Short: "Change a repository's visibility",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if args[1] == "public" && !confirm(
				fmt.Sprintf("Make %s readable by anyone who can reach this registry?", args[0])) {
				fmt.Println("cancelled")
				return nil
			}
			body := map[string]any{"visibility": args[1]}
			if err := c.patch("/api/v1/repositories/"+args[0], body, nil); err != nil {
				return err
			}
			fmt.Printf("%s %s is now %s\n", statusMark(true), cyan(args[0]), args[1])
			return nil
		},
	}

	immutable := &cobra.Command{
		Use:   "immutable <repository> <on|off>",
		Short: "Turn tag immutability on or off",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			on := args[1] == "on" || args[1] == "true"
			if err := c.patch("/api/v1/repositories/"+args[0],
				map[string]any{"immutable_tags": on}, nil); err != nil {
				return err
			}
			fmt.Printf("%s immutable tags %s for %s\n",
				statusMark(true), map[bool]string{true: "enabled", false: "disabled"}[on], cyan(args[0]))
			return nil
		},
	}

	remove := &cobra.Command{
		Use:   "delete <repository>",
		Short: "Delete a repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}

			// Show the impact before asking. A destructive command that does
			// not say what it is about to destroy is not a safe prompt.
			var repo repositoryJSON
			if err := c.get("/api/v1/repositories/"+args[0], &repo); err != nil {
				return err
			}
			fmt.Printf("%s holds %d tag(s), %d manifest(s), %s of storage.\n",
				bold(repo.Name), repo.Tags, repo.Manifests, humanBytes(repo.UsedBytes))
			if !confirm("Delete it?") {
				fmt.Println("cancelled")
				return nil
			}

			var result struct {
				Deleted string `json:"deleted"`
				Note    string `json:"note"`
			}
			if err := c.delete("/api/v1/repositories/"+args[0], &result); err != nil {
				return err
			}
			fmt.Printf("%s deleted %s\n", statusMark(true), cyan(result.Deleted))
			if result.Note != "" {
				fmt.Printf("  %s\n", dim(result.Note))
			}
			return nil
		},
	}

	cmd.AddCommand(list, create, show, visibility, immutable, remove)
	return cmd
}

// --- shared response shapes ---

type identityJSON struct {
	UUID       string     `json:"uuid"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Email      string     `json:"email"`
	Admin      bool       `json:"instance_admin"`
	Disabled   bool       `json:"disabled"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type repositoryJSON struct {
	Name          string    `json:"name"`
	Organization  string    `json:"organization"`
	Visibility    string    `json:"visibility"`
	ImmutableTags bool      `json:"immutable_tags"`
	SourceURL     string    `json:"source_url"`
	Tags          int       `json:"tags"`
	Manifests     int       `json:"manifests"`
	UsedBytes     int64     `json:"used_bytes"`
	CreatedAt     time.Time `json:"created_at"`
}

func boolMark(b bool) string {
	if b {
		return green("yes")
	}
	return dim("no")
}

func statusWord(disabled bool) string {
	if disabled {
		return red("disabled")
	}
	return green("active")
}

func humanAgePtr(t *time.Time) string {
	if t == nil {
		return dim("never")
	}
	return humanAge(*t)
}

func trimScheme(url string) string {
	url = strings.TrimPrefix(url, "https://")
	return strings.TrimPrefix(url, "http://")
}

// parseBytes accepts the same unit suffixes the daemon's configuration does, so
// an operator does not have to think in raw bytes at the command line.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	multipliers := []struct {
		suffix string
		factor int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
		{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40}, {"B", 1},
	}
	for _, m := range multipliers {
		if strings.HasSuffix(s, m.suffix) {
			var value float64
			if _, err := fmt.Sscanf(strings.TrimSuffix(s, m.suffix), "%g", &value); err != nil {
				return 0, fmt.Errorf("%q is not a valid size", s)
			}
			return int64(value * float64(m.factor)), nil
		}
	}
	var value int64
	if _, err := fmt.Sscanf(s, "%d", &value); err != nil {
		return 0, fmt.Errorf("%q is not a valid size: expected something like 100GiB", s)
	}
	return value, nil
}
