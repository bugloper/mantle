package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// --- garbage collection ---

type gcStatsJSON struct {
	DryRun               bool  `json:"dry_run"`
	SessionsCleaned      int   `json:"sessions_cleaned"`
	ManifestsQuarantined int   `json:"manifests_quarantined"`
	BlobsQuarantined     int   `json:"blobs_quarantined"`
	Unquarantined        int   `json:"unquarantined"`
	ManifestsSwept       int   `json:"manifests_swept"`
	BlobsSwept           int   `json:"blobs_swept"`
	BytesReclaimed       int64 `json:"bytes_reclaimed"`
	Candidates           []struct {
		Kind   string `json:"kind"`
		Digest string `json:"digest"`
		Size   int64  `json:"size_bytes"`
		Reason string `json:"reason"`
	} `json:"candidates"`
	PhaseDurations map[string]string `json:"phase_durations"`
	Duration       string            `json:"duration"`
	Errors         []string          `json:"errors"`
}

func newGCCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Garbage collection",
		Long: `Reclaim storage that no image references any more.

Collection is online — it never blocks a pull or a push — and it is two-phase:
objects are quarantined first and their bytes removed only after the quarantine
window, so a mistake stays recoverable. Nothing that is deployed can be
collected, by construction.`,
	}

	var dryRun bool
	run := &cobra.Command{
		Use:   "run",
		Short: "Run garbage collection",
		Long: `Run a collection cycle.

Start with --dry-run. It reports exactly what would be quarantined and why,
changes nothing, and is the documented first step before any real run.`,
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			path := "/api/v1/gc/run"
			if dryRun {
				path += "?dry_run=true"
			} else if !confirm("Run garbage collection now?") {
				fmt.Println("cancelled")
				return nil
			}

			var stats gcStatsJSON
			if err := c.post(path, map[string]any{}, &stats); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, stats)
			}
			renderGCStats(&stats)
			return nil
		},
	}
	run.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be collected, changing nothing")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the last collection and the quarantine backlog",
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var response struct {
				LastRun *struct {
					Job        string       `json:"job"`
					Status     string       `json:"status"`
					StartedAt  time.Time    `json:"started_at"`
					FinishedAt *time.Time   `json:"finished_at"`
					Error      *string      `json:"error"`
					Stats      *gcStatsJSON `json:"stats"`
				} `json:"last_run"`
				QuarantinedBlobs int    `json:"quarantined_blobs"`
				QuarantinedBytes int64  `json:"quarantined_bytes"`
				StuckDeleting    int    `json:"stuck_deleting"`
				Alert            string `json:"alert"`
				Note             string `json:"note"`
			}
			if err := c.get("/api/v1/gc/status", &response); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, response)
			}

			if response.LastRun == nil {
				fmt.Println(dim(firstNonEmpty(response.Note,
					"garbage collection has not run on this instance yet")))
			} else {
				run := response.LastRun
				fmt.Printf("%s  %s  %s\n", bold("LAST RUN"),
					gcOutcome(run.Status), dim(humanAge(run.StartedAt)))
				if run.Stats != nil {
					fmt.Printf("  reclaimed %s from %d blob(s), quarantined %d\n",
						humanBytes(run.Stats.BytesReclaimed), run.Stats.BlobsSwept,
						run.Stats.BlobsQuarantined)
				}
				if run.Error != nil && *run.Error != "" {
					fmt.Printf("  %s %s\n", red("error:"), *run.Error)
				}
			}

			fmt.Printf("\n%s  %d blob(s), %s\n", bold("QUARANTINED"),
				response.QuarantinedBlobs, humanBytes(response.QuarantinedBytes))
			fmt.Printf("  %s\n", dim("no longer served, recoverable until the quarantine window expires"))

			if response.StuckDeleting > 0 {
				fmt.Printf("\n%s %d blob(s) stuck in the deleting state\n",
					red("ALERT"), response.StuckDeleting)
				if response.Alert != "" {
					fmt.Printf("  %s\n", response.Alert)
				}
			}
			return nil
		},
	}

	reconcile := &cobra.Command{
		Use:   "reconcile",
		Short: "Compare the blob store against the catalog",
		Long: `Walk the blob store and compare it with the catalog.

This reports and never deletes. The two findings mean opposite things:
dangling rows are a correctness alarm — images that will fail to pull — while
orphan bytes are only a cost alarm.`,
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var report struct {
				OrphanBytes []struct {
					Digest string `json:"digest"`
					Size   int64  `json:"size_bytes"`
				} `json:"orphan_bytes"`
				DanglingRows []struct {
					Digest string `json:"digest"`
					Size   int64  `json:"size_bytes"`
					State  string `json:"state"`
				} `json:"dangling_rows"`
				BlobsInStorage  int    `json:"blobs_in_storage"`
				BlobsInCatalog  int    `json:"blobs_in_catalog"`
				OrphanByteCount int64  `json:"orphan_byte_count"`
				Truncated       bool   `json:"truncated"`
				Duration        string `json:"duration"`
			}
			if err := c.post("/api/v1/gc/reconcile", map[string]any{}, &report); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(os.Stdout, report)
			}

			fmt.Printf("%s %d blob(s) in storage, %d in the catalog  %s\n",
				bold("RECONCILE"), report.BlobsInStorage, report.BlobsInCatalog,
				dim("("+report.Duration+")"))

			fmt.Println()
			if len(report.DanglingRows) == 0 {
				fmt.Printf("%s no dangling rows — every catalogued blob has its content\n", statusMark(true))
			} else {
				fmt.Printf("%s %s: %d catalog row(s) have no stored content\n",
					statusMark(false), red("correctness alarm"), len(report.DanglingRows))
				fmt.Printf("  %s\n", dim("images referencing these will fail to pull"))
				for i, row := range report.DanglingRows {
					if i >= 10 {
						fmt.Printf("  %s\n", dim(fmt.Sprintf("… and %d more",
							len(report.DanglingRows)-i)))
						break
					}
					fmt.Printf("  %s  %s\n", shortDigest(row.Digest), humanBytes(row.Size))
				}
			}

			fmt.Println()
			if len(report.OrphanBytes) == 0 {
				fmt.Printf("%s no orphan bytes — nothing stored that the catalog does not know\n",
					statusMark(true))
			} else {
				fmt.Printf("%s %s: %d stored file(s) with no catalog row, %s wasted\n",
					yellow("!"), yellow("cost alarm"), len(report.OrphanBytes),
					humanBytes(report.OrphanByteCount))
				fmt.Printf("  %s\n", dim("no image is affected; this is wasted storage only"))
			}
			if report.Truncated {
				fmt.Printf("\n%s\n", dim("the report was truncated; there are more findings than shown"))
			}
			return nil
		},
	}

	cmd.AddCommand(run, status, reconcile)
	return cmd
}

func gcOutcome(status string) string {
	switch status {
	case "succeeded":
		return green(status)
	case "failed":
		return red(status)
	default:
		return yellow(status)
	}
}

func renderGCStats(stats *gcStatsJSON) {
	if stats.DryRun {
		fmt.Printf("%s nothing was changed\n\n", bold("DRY RUN"))
	}

	if len(stats.Candidates) > 0 {
		t := newTable("kind", "digest", "size", "reason")
		var total int64
		for i, candidate := range stats.Candidates {
			if i < 25 {
				t.add(candidate.Kind, shortDigest(candidate.Digest),
					humanBytes(candidate.Size), dim(candidate.Reason))
			}
			total += candidate.Size
		}
		t.render(os.Stdout)
		if len(stats.Candidates) > 25 {
			fmt.Printf("%s\n", dim(fmt.Sprintf("… and %d more", len(stats.Candidates)-25)))
		}
		fmt.Printf("\n%s %d object(s), %s\n", bold("WOULD QUARANTINE"),
			len(stats.Candidates), humanBytes(total))
		fmt.Printf("  %s\n",
			dim("quarantined objects stop being served but remain recoverable "+
				"until the quarantine window expires"))
		return
	}

	fmt.Printf("%s in %s\n", bold("GARBAGE COLLECTION"), stats.Duration)
	fmt.Printf("  sessions cleaned      %d\n", stats.SessionsCleaned)
	fmt.Printf("  manifests quarantined %d\n", stats.ManifestsQuarantined)
	fmt.Printf("  blobs quarantined     %d\n", stats.BlobsQuarantined)
	if stats.Unquarantined > 0 {
		fmt.Printf("  restored              %d %s\n", stats.Unquarantined,
			dim("(became reachable again)"))
	}
	fmt.Printf("  blobs swept           %d\n", stats.BlobsSwept)
	fmt.Printf("  %s        %s\n", bold("reclaimed"), green(humanBytes(stats.BytesReclaimed)))

	if len(stats.Errors) > 0 {
		fmt.Printf("\n%s\n", red("errors:"))
		for _, e := range stats.Errors {
			fmt.Printf("  %s\n", e)
		}
	}
}

// --- doctor ---

func newDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check a running instance's health",
		Long: `Run health checks against a live instance.

Doctor is a feature, not a debugging aid (principle 4): it is the first thing to
run when something is wrong, and every failing check names what to do next.`,
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}

			type check struct {
				Name    string `json:"name"`
				OK      bool   `json:"ok"`
				Detail  string `json:"detail"`
				Elapsed string `json:"elapsed"`
			}
			var readiness struct {
				Status  string  `json:"status"`
				Version string  `json:"version"`
				Checks  []check `json:"checks"`
			}

			// /readyz answers without credentials, so doctor still works when
			// the problem is the credential itself.
			err = c.get("/readyz", &readiness)
			var apiErr *apiError
			if err != nil && !asAPIError(err, &apiErr) {
				return err
			}

			if flagJSON {
				return printJSON(os.Stdout, readiness)
			}

			fmt.Printf("%s %s\n", bold("MANTLE DOCTOR"), dim(c.baseURL))
			fmt.Println()

			allOK := true
			for _, ch := range readiness.Checks {
				fmt.Printf("  %s %-14s %s\n", statusMark(ch.OK), ch.Name, dim(ch.Elapsed))
				if !ch.OK {
					allOK = false
					fmt.Printf("      %s\n", red(ch.Detail))
					if remedy := doctorRemedy(ch.Name); remedy != "" {
						fmt.Printf("      %s\n", dim(remedy))
					}
				}
			}

			// Credentialed checks, which only run if we have a working credential.
			var gcStatus struct {
				QuarantinedBlobs int    `json:"quarantined_blobs"`
				QuarantinedBytes int64  `json:"quarantined_bytes"`
				StuckDeleting    int    `json:"stuck_deleting"`
				Alert            string `json:"alert"`
			}
			if err := c.get("/api/v1/gc/status", &gcStatus); err == nil {
				stuckOK := gcStatus.StuckDeleting == 0
				fmt.Printf("  %s %-14s %s\n", statusMark(stuckOK), "storage sweep",
					dim(fmt.Sprintf("%d quarantined, %s",
						gcStatus.QuarantinedBlobs, humanBytes(gcStatus.QuarantinedBytes))))
				if !stuckOK {
					allOK = false
					fmt.Printf("      %s\n", red(gcStatus.Alert))
				}
			} else {
				fmt.Printf("  %s %-14s %s\n", yellow("?"), "storage sweep",
					dim("not checked: no administrator credential"))
			}

			fmt.Println()
			if allOK && readiness.Status == "ready" {
				fmt.Printf("%s instance is healthy (Mantle %s)\n", green("✓"), readiness.Version)
				return nil
			}
			fmt.Printf("%s instance is not healthy\n", red("✗"))
			return fmt.Errorf("one or more checks failed")
		},
	}
}

// doctorRemedy maps a failing check to its next action.
func doctorRemedy(check string) string {
	switch check {
	case "database":
		return "Check that PostgreSQL is running and database.url is correct."
	case "migrations":
		return "Restart mantled to apply pending migrations, or run 'mantle upgrade --check'."
	case "storage":
		return "Check that the storage path exists, is writable, and has free space."
	case "startup":
		return "The daemon is still starting. If this persists, check its log."
	case "shutdown":
		return "The node is draining. This is expected during a restart."
	default:
		return ""
	}
}

func asAPIError(err error, target **apiError) bool {
	if e, ok := err.(*apiError); ok {
		*target = e
		return true
	}
	return false
}

// --- setup (§13.6) ---

func newSetupCommand() *cobra.Command {
	var repo, org, pack string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Create tokens and print the integration snippets for a repository",
		Long: `Set a repository up for use: create a builder token and a pull token, then
print the provenance labels and the deploy-reporting snippet.

The snippets are copy-pasteable text rather than a package, deliberately. A
package needs releases, a compatibility matrix, and security advisories; thirty
lines someone can read is a thing they can trust.`,
		RunE: func(*cobra.Command, []string) error {
			if repo == "" {
				return fmt.Errorf("--repo is required, e.g. --repo acme/web")
			}
			if org == "" {
				org, _, _ = strings.Cut(repo, "/")
			}
			c, err := client()
			if err != nil {
				return err
			}

			var version struct {
				Version string `json:"version"`
			}
			if err := c.get("/api/v1/version", &version); err != nil {
				return err
			}
			fmt.Printf("  %s Registry %s reachable (Mantle %s)\n",
				statusMark(true), c.baseURL, version.Version)

			builderSecret, err := createSetupToken(c, repo+" builder", org, repo, "contributor")
			if err != nil {
				return err
			}
			fmt.Printf("  %s Created deploy token '%s builder'\n", statusMark(true), repo)

			pullSecret, err := createSetupToken(c, repo+" servers", org, repo, "reader")
			if err != nil {
				return err
			}
			fmt.Printf("  %s Created pull token   '%s servers'\n", statusMark(true), repo)

			host := trimScheme(c.baseURL)
			fmt.Println()
			fmt.Println(bold("  Tokens — shown once:"))
			fmt.Printf("    builder  %s\n", builderSecret)
			fmt.Printf("    servers  %s\n", pullSecret)

			section(os.Stdout, "  Build — provenance labels (Tier 0)")
			fmt.Printf(`    docker buildx build \
      --label org.opencontainers.image.source="$(git remote get-url origin)" \
      --label org.opencontainers.image.revision="$(git rev-parse HEAD)" \
      --label org.opencontainers.image.created="$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" \
      -t %s/%s:"$(git rev-parse --short HEAD)" \
      --push .
`, host, repo)

			printDeploySnippet(pack, host, repo)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repository to set up (required)")
	cmd.Flags().StringVar(&org, "org", "", "organization (default: the repository's first path component)")
	cmd.Flags().StringVar(&pack, "pack", "compose", "deploy snippet: compose, systemd, ansible, ci, or curl")
	return cmd
}

func createSetupToken(c *apiClient, name, org, repo, role string) (string, error) {
	var created struct {
		Secret string `json:"secret"`
	}
	body := map[string]any{
		"name": name, "kind": "deploy_token", "organization": org,
		"namespace": repo, "role": role,
	}
	if err := c.post("/api/v1/tokens", body, &created); err != nil {
		return "", err
	}
	return created.Secret, nil
}

// printDeploySnippet emits the integration pack for one deploy tool (§13.6).
//
// Every snippet is failure-tolerant. Recording a deployment must never be able
// to fail a deployment (REQ-LEDGER-02), so each one ends in `|| true` or its
// equivalent — and that is a property of the snippets, not advice alongside them.
func printDeploySnippet(pack, host, repo string) {
	switch pack {
	case "systemd":
		section(os.Stdout, "  Deploy — systemd drop-in")
		fmt.Printf(`    [Service]
    ExecStartPost=-/usr/local/bin/mantle deploy record \
      --repo %s --tag %%i --env production --host %%H --status active
`, repo)
		fmt.Printf("\n    %s\n",
			dim("The leading '-' makes systemd ignore a failure, so reporting cannot fail the unit."))

	case "ansible":
		section(os.Stdout, "  Deploy — Ansible task")
		fmt.Printf(`    - name: Record deployment in Mantle
      ansible.builtin.uri:
        url: "https://%s/api/v1/deployments"
        method: POST
        headers: { Authorization: "Bearer {{ mantle_deploy_token }}" }
        body_format: json
        body:
          repository: %s
          digest: "{{ image_digest }}"
          environment: production
          hosts: "{{ ansible_play_hosts }}"
          external_id: "{{ ansible_date_time.iso8601 }}"
          status: active
      failed_when: false
`, host, repo)

	case "ci":
		section(os.Stdout, "  Deploy — CI step")
		fmt.Printf(`    - name: Record deployment
      run: |
        mantle deploy record \
          --repo %s --digest "${IMAGE_DIGEST}" \
          --env production --performer "${GITHUB_ACTOR:-ci}" \
          --deploy-id "${GITHUB_RUN_ID:-$(date +%%s)}" \
          --status active || true
      env:
        MANTLE_REGISTRY: https://%s
        MANTLE_TOKEN: ${{ secrets.MANTLE_DEPLOY_TOKEN }}
`, repo, host)

	case "curl":
		section(os.Stdout, "  Deploy — curl one-liner")
		fmt.Printf(`    curl -sf -X POST https://%s/api/v1/deployments \
      -H "Authorization: Bearer $MANTLE_TOKEN" \
      -H 'Content-Type: application/json' \
      -d '{"repository":"%s","digest":"'"$DIGEST"'","environment":"production",
           "host":"'"$(hostname)"'","status":"active"}' || true
`, host, repo)

	default:
		section(os.Stdout, "  Deploy — Compose")
		fmt.Printf(`    docker compose pull && docker compose up -d
    DIGEST=$(docker inspect --format='{{index .RepoDigests 0}}' %s/%s)
    mantle deploy record --repo %s --digest "${DIGEST#*@}" \
      --env production --host "$(hostname)" --performer "$USER" \
      --status active || true
`, host, repo, repo)
	}
	fmt.Println()
}
