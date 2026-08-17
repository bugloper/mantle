// Command mantle is the Mantle command-line interface.
//
// The CLI is complete (principle 7): anything an administrator can do, they can
// do from here against a remote instance. No capability may exist only in a
// future web interface — if `mantle-ui` can ever do something this cannot,
// that is a bug in the CLI.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is set at build time.
var Version = "dev"

// Global flags shared by every command that talks to a registry.
var (
	flagRegistry string
	flagUsername string
	flagSecret   string
	flagJSON     bool
	flagYes      bool
)

func main() {
	root := newRootCommand()
	if err := root.Execute(); err != nil {
		// Cobra has already printed usage errors; this prints the rest, with
		// the remedy line that apiError carries.
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			fmt.Fprintf(os.Stderr, "%s %s\n", red("error:"), apiErr.Error())
		} else {
			fmt.Fprintf(os.Stderr, "%s %v\n", red("error:"), err)
		}
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "mantle",
		Short: "Mantle — the registry that knows what you deployed",
		Long: `Mantle is a self-hosted OCI container registry that links every image to the
commit that built it and the servers running it.

This CLI is a client of the registry's public API. Every command works against
a remote instance, and nothing it can do requires local access to the daemon.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// A bare `mantle` should show help rather than an error.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	root.PersistentFlags().StringVar(&flagRegistry, "registry", "",
		"registry URL (default: from 'mantle login', or $MANTLE_REGISTRY)")
	root.PersistentFlags().StringVar(&flagUsername, "username", "",
		"username (default: from 'mantle login', or $MANTLE_USERNAME)")
	root.PersistentFlags().StringVar(&flagSecret, "token", "",
		"token or password (default: from 'mantle login', or $MANTLE_TOKEN)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false,
		"emit JSON instead of a table")
	root.PersistentFlags().BoolVarP(&flagYes, "yes", "y", false,
		"skip confirmation prompts")

	root.AddCommand(
		newVersionCommand(),
		newLoginCommand(),
		newDoctorCommand(),
		newOrgCommand(),
		newUserCommand(),
		newTokenCommand(),
		newRepoCommand(),
		newGCCommand(),
		newLedgerCommand(),
		newDeployCommand(),
		newSetupCommand(),
	)
	return root
}

// client builds an API client from the global flags.
func client() (*apiClient, error) {
	return newClient(flagRegistry, flagUsername, flagSecret)
}

func newVersionCommand() *cobra.Command {
	var serverToo bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version, and the registry's if reachable",
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagJSON {
				payload := map[string]string{"cli": Version}
				if serverToo {
					if c, err := client(); err == nil {
						var remote struct {
							Version string `json:"version"`
							API     string `json:"api"`
						}
						if err := c.get("/api/v1/version", &remote); err == nil {
							payload["registry"] = remote.Version
							payload["api"] = remote.API
						}
					}
				}
				return printJSON(os.Stdout, payload)
			}

			fmt.Printf("mantle %s\n", Version)
			if !serverToo {
				return nil
			}
			c, err := client()
			if err != nil {
				fmt.Println(dim("registry: not configured"))
				return nil
			}
			var remote struct {
				Version string `json:"version"`
				API     string `json:"api"`
			}
			if err := c.get("/api/v1/version", &remote); err != nil {
				fmt.Printf("%s %v\n", dim("registry:"), err)
				return nil
			}
			fmt.Printf("registry %s (API %s) at %s\n", remote.Version, remote.API, c.baseURL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&serverToo, "server", false, "also query the registry's version")
	return cmd
}

func newLoginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login <registry-url>",
		Short: "Store credentials for a registry",
		Long: `Store credentials for a registry so later commands need no flags.

The credential is written to ~/.config/mantle/credentials.yaml with owner-only
permissions. Prefer passing a token via --token or $MANTLE_TOKEN over typing a
password, which would otherwise land in your shell history.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			creds := &credentials{
				Registry: args[0],
				Username: flagUsername,
				Secret:   flagSecret,
			}
			if creds.Secret == "" {
				creds.Secret = os.Getenv("MANTLE_TOKEN")
			}
			if creds.Secret == "" {
				return fmt.Errorf(
					"no credential supplied\n" +
						"  Pass --token, or set MANTLE_TOKEN, so the secret does not\n" +
						"  end up in your shell history.")
			}

			// Verify before saving. Storing a credential that does not work is
			// a worse outcome than refusing to store it.
			probe, err := newClient(creds.Registry, creds.Username, creds.Secret)
			if err != nil {
				return err
			}
			var remote struct {
				Version string `json:"version"`
			}
			if err := probe.get("/api/v1/version", &remote); err != nil {
				return fmt.Errorf("could not authenticate against %s: %w", creds.Registry, err)
			}

			path, err := saveCredentials(creds)
			if err != nil {
				return err
			}
			fmt.Printf("%s authenticated against %s (Mantle %s)\n",
				statusMark(true), probe.baseURL, remote.Version)
			fmt.Printf("  credentials saved to %s\n", path)
			return nil
		},
	}
	return cmd
}

// confirm prompts before a destructive action unless --yes was passed.
func confirm(prompt string) bool {
	if flagYes {
		return true
	}
	fmt.Printf("%s %s [y/N]: ", yellow("?"), prompt)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return false
	}
	return answer == "y" || answer == "Y" || answer == "yes"
}
