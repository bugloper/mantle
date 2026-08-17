package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// --- ledger ---

type ledgerJSON struct {
	Repository string          `json:"repository"`
	SourceURL  string          `json:"source_url"`
	Running    *deploymentJSON `json:"running"`
	Rollback   []struct {
		Digest     string    `json:"digest"`
		Tag        string    `json:"tag"`
		CommitSHA  string    `json:"commit_sha"`
		DeployedAt time.Time `json:"deployed_at"`
		Pinned     bool      `json:"pinned"`
	} `json:"rollback_candidates"`
	Tags []struct {
		Name      string    `json:"name"`
		Digest    string    `json:"digest"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"tags"`
	Storage struct {
		TotalBytes  int64 `json:"total_bytes"`
		Manifests   int   `json:"manifests"`
		Reclaimable int64 `json:"reclaimable_bytes"`
		Quarantined int64 `json:"quarantined_bytes"`
	} `json:"storage"`
	Environments []string `json:"environments"`
}

type deploymentJSON struct {
	UUID        string     `json:"uuid"`
	Digest      string     `json:"digest"`
	Tag         string     `json:"tag"`
	Environment string     `json:"environment"`
	Status      string     `json:"status"`
	Confidence  string     `json:"confidence"`
	CommitSHA   string     `json:"commit_sha"`
	Performer   string     `json:"performer"`
	DeployTool  string     `json:"deploy_tool"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Hosts       []struct {
		Hostname string `json:"hostname"`
		Address  string `json:"address"`
		Status   string `json:"status"`
	} `json:"hosts"`
}

func newLedgerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ledger",
		Short: "Inspect the deployment ledger",
		Long: `The deployment ledger links each image to the commit that produced it and the
servers running it — the question "what is running, and where did it come
from?" answered from the registry's own vantage point.`,
	}

	var repo, environment string
	status := &cobra.Command{
		Use:   "status",
		Short: "Show what is deployed for a repository",
		RunE: func(*cobra.Command, []string) error {
			if repo == "" {
				return fmt.Errorf("--repo is required")
			}
			c, err := client()
			if err != nil {
				return err
			}
			path := "/api/v1/repositories/" + repo + "/ledger"
			if environment != "" {
				path += "?environment=" + url.QueryEscape(environment)
			}
			var view ledgerJSON
			if err := c.get(path, &view); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, view)
			}
			renderLedger(&view, environment)
			return nil
		},
	}
	status.Flags().StringVar(&repo, "repo", "", "repository to inspect (required)")
	status.Flags().StringVar(&environment, "env", "", "environment (default: production)")

	cmd.AddCommand(status)
	return cmd
}

// renderLedger prints the view from §13.5.
func renderLedger(view *ledgerJSON, environment string) {
	if environment == "" {
		environment = "production"
	}

	hostCount := 0
	if view.Running != nil {
		hostCount = len(view.Running.Hosts)
	}
	header := fmt.Sprintf("%s", bold(view.Repository))
	right := fmt.Sprintf("%s · %d host(s)", environment, hostCount)
	fmt.Printf("%s%s%s\n", header, strings.Repeat(" ", maxInt(2, 52-len(view.Repository))), dim(right))
	fmt.Println(dim(strings.Repeat("─", 70)))

	// --- now running ---
	if view.Running == nil {
		fmt.Printf("%-15s %s\n", bold("NOW RUNNING"), dim("no deployment recorded for "+environment))
		fmt.Printf("%-15s %s\n", "",
			dim("report one with 'mantle deploy record', or let Mantle infer it from pulls"))
	} else {
		r := view.Running
		label := r.Tag
		if label == "" {
			label = dim("(untagged)")
		}
		fmt.Printf("%-15s %-10s %s", bold("NOW RUNNING"), cyan(label), shortDigest(r.Digest))
		if r.CommitSHA != "" {
			fmt.Printf("   commit %s", green(shortCommit(r.CommitSHA)))
		}
		fmt.Printf("   deployed %s\n", humanAge(r.StartedAt))

		var attribution []string
		if r.Performer != "" {
			attribution = append(attribution, "by "+r.Performer)
		}
		confirmed := 0
		for _, h := range r.Hosts {
			if h.Status == "confirmed" {
				confirmed++
			}
		}
		if len(r.Hosts) > 0 {
			attribution = append(attribution,
				fmt.Sprintf("%d/%d hosts confirmed", confirmed, len(r.Hosts)))
		}
		// The confidence tier is shown, not hidden. An inferred deployment is a
		// good guess from pull traffic and a reported one is a fact; presenting
		// them identically would make the ledger untrustworthy the first time a
		// guess was wrong (§13.2).
		switch r.Confidence {
		case "reported":
			source := "reported"
			if r.DeployTool != "" {
				source += " via " + r.DeployTool
			}
			attribution = append(attribution, source)
		case "verified":
			attribution = append(attribution, green("verified by agent"))
		default:
			attribution = append(attribution, yellow("inferred from pull traffic"))
		}
		fmt.Printf("%-15s %s\n", "", dim(strings.Join(attribution, " · ")))

		if len(r.Hosts) > 0 {
			names := make([]string, 0, len(r.Hosts))
			for _, h := range r.Hosts {
				name := h.Hostname
				if name == "" {
					name = h.Address
				}
				names = append(names, name)
			}
			fmt.Printf("%-15s %s\n", "", dim(strings.Join(names, ", ")))
		}
	}

	// --- rollback targets ---
	fmt.Println()
	if len(view.Rollback) == 0 {
		fmt.Printf("%-15s %s\n", bold("ROLLBACK TO"), dim("no earlier deployment recorded"))
	} else {
		for i, target := range view.Rollback {
			label := bold("ROLLBACK TO")
			if i > 0 {
				label = strings.Repeat(" ", 11)
			}
			tag := target.Tag
			if tag == "" {
				tag = dim("(untagged)")
			}
			pin := yellow("unpinned")
			if target.Pinned {
				// This is the product's central promise, made visible: a pinned
				// image cannot be collected or expired by any policy (§13.4).
				pin = green("pinned ✓")
			}
			commit := ""
			if target.CommitSHA != "" {
				commit = "commit " + shortCommit(target.CommitSHA)
			}
			fmt.Printf("%-15s %-10s %s   %-17s %s\n",
				label, cyan(tag), shortDigest(target.Digest), commit, pin)
		}
	}

	// --- tags ---
	fmt.Println()
	if len(view.Tags) > 0 {
		names := make([]string, 0, len(view.Tags))
		for i, t := range view.Tags {
			if i >= 6 {
				names = append(names, dim(fmt.Sprintf("+%d more", len(view.Tags)-i)))
				break
			}
			names = append(names, t.Name)
		}
		fmt.Printf("%-15s %s\n", bold("TAGS"), strings.Join(names, "  "))
	}

	// --- storage ---
	storage := fmt.Sprintf("%s across %d manifests",
		humanBytes(view.Storage.TotalBytes), view.Storage.Manifests)
	if view.Storage.Reclaimable > 0 {
		storage += fmt.Sprintf(" · GC would free ~%s", humanBytes(view.Storage.Reclaimable))
	}
	if view.Storage.Quarantined > 0 {
		storage += fmt.Sprintf(" · %s quarantined", humanBytes(view.Storage.Quarantined))
	}
	fmt.Printf("%-15s %s\n", bold("STORAGE"), storage)

	if view.SourceURL != "" {
		fmt.Printf("%-15s %s\n", bold("SOURCE"), view.SourceURL)
	}
	if len(view.Environments) > 1 {
		fmt.Printf("%-15s %s\n", bold("ENVIRONMENTS"), strings.Join(view.Environments, ", "))
	}

	// Mantle names the rollback command but never runs it. Owning deployment
	// would put the registry in the incident path, which §3.5 rules out.
	if len(view.Rollback) > 0 {
		fmt.Println()
		target := view.Rollback[0]
		reference := target.Tag
		if reference == "" {
			reference = target.Digest
		}
		fmt.Printf("%s %s\n", dim("To roll back:"),
			dim("docker pull <registry>/"+view.Repository+":"+reference+" && redeploy"))
	}
	fmt.Println()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- deploy record (§13.2, Tier 1) ---

func newDeployCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "deploy", Short: "Report deployments to the ledger"}

	var (
		repo, digest, tag, environment, status string
		commit, performer, tool, externalID    string
		hosts                                  []string
	)

	record := &cobra.Command{
		Use:   "record",
		Short: "Record that an image was deployed",
		Long: `Record a deployment in the ledger.

Only --repo and one of --digest or --tag are required; everything else improves
fidelity. This call is safe to make from any deploy process — it returns
quickly and must never be able to fail a deploy, so the documented invocation
ends in "|| true".`,
		Example: `  mantle deploy record --repo acme/web --digest sha256:9f3a… \
    --env production --host web-1 --host web-2 \
    --performer "$USER" --status active || true`,
		RunE: func(*cobra.Command, []string) error {
			if repo == "" {
				return fmt.Errorf("--repo is required")
			}
			if digest == "" && tag == "" {
				return fmt.Errorf("one of --digest or --tag is required")
			}
			c, err := client()
			if err != nil {
				return err
			}

			body := map[string]any{
				"repository": repo, "digest": digest, "tag": tag,
				"environment": environment, "status": status,
				"commit_sha": commit, "performer": performer,
				"deploy_tool": tool, "external_id": externalID,
				"hosts": hosts,
			}
			var recorded deploymentJSON
			if err := c.post("/api/v1/deployments", body, &recorded); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, recorded)
			}

			fmt.Printf("%s recorded %s deployment of %s %s\n",
				statusMark(true), recorded.Environment, cyan(repo), shortDigest(recorded.Digest))
			if recorded.CommitSHA != "" {
				fmt.Printf("  commit %s\n", green(shortCommit(recorded.CommitSHA)))
			}
			if len(recorded.Hosts) > 0 {
				fmt.Printf("  %d host(s) recorded\n", len(recorded.Hosts))
			}
			// The pin is the reason to bother reporting at all, so it is stated.
			fmt.Printf("  %s\n", dim("this image is now pinned and cannot be removed by retention or GC"))
			return nil
		},
	}
	record.Flags().StringVar(&repo, "repo", "", "repository (required)")
	record.Flags().StringVar(&digest, "digest", "", "image digest")
	record.Flags().StringVar(&tag, "tag", "", "image tag, if the digest is not known")
	record.Flags().StringVar(&environment, "env", "production", "environment")
	record.Flags().StringVar(&status, "status", "active", "started, active, failed, or rolled_back")
	record.Flags().StringVar(&commit, "commit", "", "git commit (default: read from the image)")
	record.Flags().StringVar(&performer, "performer", "", "who or what performed the deploy")
	record.Flags().StringVar(&tool, "tool", "", "deploy tool, e.g. compose, ansible, systemd")
	record.Flags().StringVar(&externalID, "deploy-id", "",
		"caller-supplied id; repeats with the same id collapse to one record")
	record.Flags().StringArrayVar(&hosts, "host", nil, "host running the image (repeatable)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List recorded deployments",
		RunE: func(*cobra.Command, []string) error {
			if repo == "" {
				return fmt.Errorf("--repo is required")
			}
			c, err := client()
			if err != nil {
				return err
			}
			path := "/api/v1/deployments?repository=" + url.QueryEscape(repo)
			if environment != "" && environment != "production" {
				path += "&environment=" + url.QueryEscape(environment)
			}
			var response struct {
				Deployments []deploymentJSON `json:"deployments"`
			}
			if err := c.get(path, &response); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, response)
			}
			t := newTable("when", "env", "status", "digest", "commit", "confidence", "by")
			for _, d := range response.Deployments {
				t.add(humanAge(d.StartedAt), d.Environment, deployStatus(d.Status),
					shortDigest(d.Digest), shortCommit(d.CommitSHA),
					confidenceLabel(d.Confidence), d.Performer)
			}
			t.render(os.Stdout)
			return nil
		},
	}
	list.Flags().StringVar(&repo, "repo", "", "repository (required)")
	list.Flags().StringVar(&environment, "env", "", "filter by environment")

	cmd.AddCommand(record, list)
	return cmd
}

func deployStatus(status string) string {
	switch status {
	case "active":
		return green(status)
	case "failed":
		return red(status)
	case "rolled_back":
		return yellow(status)
	default:
		return dim(status)
	}
}

func confidenceLabel(confidence string) string {
	switch confidence {
	case "reported":
		return green(confidence)
	case "verified":
		return green(confidence)
	default:
		return yellow(confidence)
	}
}
