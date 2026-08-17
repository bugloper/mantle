// Package architecture enforces the dependency rule from §7.2.
//
// The rule exists to protect principle 2 and NG-1: the pull path is the only
// path where an outage is a production incident for the customer, so no product
// feature may ever become a dependency of it. That is easy to state and easy to
// violate by accident — one import added for convenience during a refactor is
// all it takes — so it is checked mechanically rather than trusted to review.
package architecture

import (
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/mantle-sh/mantle"

// forbidden lists packages that must not appear in a package's transitive
// import graph, with the reason, so a failure explains itself.
var rules = []struct {
	pkg        string
	mustNotUse []string
	reason     string
}{
	{
		pkg: "internal/distribution",
		mustNotUse: []string{
			"internal/ledger",
			"internal/gc",
			"internal/retention",
			"internal/admin",
			"internal/server",
		},
		reason: "the pull path must not depend on any product feature (§7.2, NG-1). " +
			"Reach the ledger through the events.Sink interface instead.",
	},
	{
		pkg:        "internal/oci",
		mustNotUse: []string{"internal/"},
		reason: "oci holds the primitive value types and must stay a leaf, so every " +
			"layer can import it without creating a cycle.",
	},
	{
		pkg:        "internal/storage/driver",
		mustNotUse: []string{"internal/catalog", "internal/ledger", "internal/distribution"},
		reason: "the storage layer maps a digest to bytes and must know nothing about " +
			"repositories, tags, or permissions (§10.1).",
	},
	{
		pkg:        "internal/events",
		mustNotUse: []string{"internal/"},
		reason: "events is the vocabulary both sides share; importing either side " +
			"would defeat the indirection it exists to provide.",
	},
	{
		pkg:        "internal/ledger",
		mustNotUse: []string{"internal/distribution", "internal/admin", "internal/server"},
		reason:     "the ledger is a consumer of events, not a consumer of the HTTP surface.",
	},
	{
		pkg:        "cmd/mantle-ui",
		mustNotUse: []string{"internal/"},
		reason: "mantle-ui must be an ordinary API client with no privileged access (§14.3). " +
			"Importing anything from internal/ would give it a way to reach past the " +
			"public API, and the interface would stop being separable from the daemon.",
	},
	{
		pkg:        "cmd/mantle",
		mustNotUse: []string{"internal/catalog", "internal/ledger", "internal/gc", "internal/server"},
		reason: "every CLI command is an API call (§2.2). Reaching into the database or " +
			"the storage layer directly would break the CLI against a remote instance " +
			"and let the API quietly rot.",
	},
}

func TestDependencyRule(t *testing.T) {
	root := repoRoot(t)

	for _, rule := range rules {
		imports, err := transitiveImports(root, rule.pkg)
		if err != nil {
			t.Fatalf("resolving imports of %s: %v", rule.pkg, err)
		}

		for _, forbidden := range rule.mustNotUse {
			full := modulePath + "/" + strings.TrimSuffix(forbidden, "/")
			for _, imported := range sortedKeys(imports) {
				violates := imported == full
				// A trailing slash in the rule means "any package under here",
				// which is how the leaf-package rules are expressed.
				if strings.HasSuffix(forbidden, "/") {
					violates = strings.HasPrefix(imported, modulePath+"/"+forbidden) &&
						imported != modulePath+"/"+strings.TrimSuffix(rule.pkg, "/")
					// A package may import its own subpackages.
					if strings.HasPrefix(imported, modulePath+"/"+rule.pkg+"/") {
						violates = false
					}
				}
				if violates {
					t.Errorf("%s imports %s (via the transitive graph)\n  %s\n  path: %s",
						rule.pkg, strings.TrimPrefix(imported, modulePath+"/"),
						rule.reason, imports[imported])
				}
			}
		}
	}
}

// transitiveImports returns every in-module package reachable from pkg, mapped
// to the import chain that reaches it — so a violation reports how the
// dependency crept in rather than only that it exists.
func transitiveImports(root, pkg string) (map[string]string, error) {
	found := map[string]string{}
	var walk func(current, chain string) error

	walk = func(current, chain string) error {
		dir := filepath.Join(root, strings.TrimPrefix(current, modulePath+"/"))
		imported, err := directImports(dir)
		if err != nil {
			// A package with no Go files is not an error; it may be a
			// directory holding only SQL or testdata.
			return nil
		}
		for _, next := range imported {
			if !strings.HasPrefix(next, modulePath+"/") {
				continue // Standard library or a third-party dependency.
			}
			nextChain := chain + " → " + strings.TrimPrefix(next, modulePath+"/")
			if _, seen := found[next]; seen {
				continue
			}
			found[next] = nextChain
			if err := walk(next, nextChain); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(modulePath+"/"+pkg, pkg); err != nil {
		return nil, err
	}
	return found, nil
}

// directImports lists the non-test imports of the package in a directory.
//
// Test files are excluded deliberately: a test may legitimately import the
// ledger to assert on what a push recorded, and forbidding that would make the
// rule unusable without weakening what it actually protects — the shipped
// import graph.
func directImports(dir string) ([]string, error) {
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}
	return pkg.Imports, nil
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate the module root")
		}
		dir = parent
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The pull path's dependency budget is a design constraint worth seeing, not
// only enforcing. This reports it so a reviewer notices the graph growing.
func TestReportPullPathDependencies(t *testing.T) {
	root := repoRoot(t)
	imports, err := transitiveImports(root, "internal/distribution")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, imported := range sortedKeys(imports) {
		names = append(names, strings.TrimPrefix(imported, modulePath+"/"))
	}
	t.Logf("the distribution package transitively depends on %d in-module packages:\n  %s",
		len(names), strings.Join(names, "\n  "))

	// A soft ceiling. The pull path gets the simplest code and the fewest
	// dependencies (principle 2); crossing this should prompt a look rather
	// than an automatic failure, so the message says so.
	const budget = 12
	if len(names) > budget {
		t.Errorf("the pull path now depends on %d packages, above the budget of %d.\n"+
			"That may be fine, but it is worth justifying: %s",
			len(names), budget, fmt.Sprint(names))
	}
}
