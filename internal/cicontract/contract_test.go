package cicontract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot returns the repository root relative to this package's directory
// (go test runs the test binary with the package directory as cwd).
func repoRoot() string { return filepath.Join("..", "..") }

func readFile(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

func mustParse(t *testing.T, data []byte) *Workflow {
	t.Helper()
	wf, err := ParseWorkflow(data)
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	return wf
}

func mustReplace(t *testing.T, text, old, new string, n int) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("mutation anchor %q not found in workflow", old)
	}
	return strings.Replace(text, old, new, n)
}

// checkNames maps every contract check to a name so negative controls can
// assert that a mutation trips exactly one check (i.e. the checker is specific,
// not merely "something broke").
func checkErrors(ci, release *Workflow, pins []string) map[string][]error {
	return map[string][]error{
		"required-gates": CheckRequiredGates(ci),
		"no-advisory":    CheckNoAdvisory(ci),
		"sha-pins-ci":    CheckSHAPins(ci, "ci.yml"),
		"sha-pins-rel":   CheckSHAPins(release, "release.yml"),
		"cache-contract": CheckCacheContract(ci, pins),
		"needs-linkage":  CheckNeedsLinkage(ci),
		"offline-env":    CheckOfflineEnv(ci),
	}
}

func TestCIWorkflowContract(t *testing.T) {
	pins, err := PinnedPrefixes(repoRoot())
	if err != nil {
		t.Fatalf("read pins: %v", err)
	}
	if len(pins) != 3 {
		t.Fatalf("want 3 pin prefixes, got %v", pins)
	}
	ci := mustParse(t, readFile(t, filepath.Join(".github", "workflows", "ci.yml")))
	release := mustParse(t, readFile(t, filepath.Join(".github", "workflows", "release.yml")))
	errs := Validate(ci, release, pins)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("CI contract violation: %v", e)
		}
	}
}

func TestRequiredGateCountIsNine(t *testing.T) {
	if len(RequiredGates) != 9 {
		t.Fatalf("README/SECURITY claim nine required gates, contract lists %d", len(RequiredGates))
	}
	seen := map[string]bool{}
	for _, id := range RequiredGates {
		if seen[id] {
			t.Fatalf("duplicate gate %q", id)
		}
		seen[id] = true
	}
}

// TestContractRejectsMutations is the negative-control suite. Each case makes
// ONE targeted change to the real workflow YAML and asserts that exactly the
// intended check fails while all others still pass. A checker that passed all
// of these would be a tautology, not a gate.
func TestContractRejectsMutations(t *testing.T) {
	ciText := string(readFile(t, filepath.Join(".github", "workflows", "ci.yml")))
	relText := string(readFile(t, filepath.Join(".github", "workflows", "release.yml")))
	pins, err := PinnedPrefixes(repoRoot())
	if err != nil {
		t.Fatalf("read pins: %v", err)
	}
	goodRelease := mustParse(t, []byte(relText))

	const pinSHA = "11d5960a326750d5838078e36cf38b85af677262"
	const cacheKey = "quack-e2e-v2.0.0-cli-c6507206-quack-9c893bd5-httpfs-77e6c429-v2"

	// Drift only the SECOND cache key (the downstream TLS job), leaving the
	// producer's key intact: this is the exact r12 failure mode.
	driftedKey := ciText
	idx := strings.Index(driftedKey, "  engine-tls-capability:")
	if idx < 0 {
		t.Fatal("engine-tls-capability job header not found")
	}
	head, tail := driftedKey[:idx], driftedKey[idx:]
	tail = mustReplace(t, tail, "key: "+cacheKey, "key: quack-e2e-DRIFTED", 1)
	driftedKey = head + tail

	cases := []struct {
		name        string
		ci          string
		release     *Workflow
		wantFailure string
	}{
		{
			name:        "downstream job no longer waits for cache producer",
			ci:          mustReplace(t, ciText, "\n    needs: quack-e2e\n", "\n    needs: go\n", 1),
			release:     goodRelease,
			wantFailure: "needs-linkage",
		},
		{
			name:        "downstream cache key drifts from producer",
			ci:          driftedKey,
			release:     goodRelease,
			wantFailure: "cache-contract",
		},
		{
			name:        "cache key no longer carries the pinned hashes",
			ci:          strings.ReplaceAll(ciText, cacheKey, "quack-e2e-v2.0.0-v3"),
			release:     goodRelease,
			wantFailure: "cache-contract",
		},
		{
			name:        "offline switch disabled on a required gate",
			ci:          mustReplace(t, ciText, "QUACK_TLS_OFFLINE: '1'", "QUACK_TLS_OFFLINE: '0'", 1),
			release:     goodRelease,
			wantFailure: "offline-env",
		},
		{
			name:        "advisory silently reintroduced",
			ci:          mustReplace(t, ciText, "  crash-drill:\n", "  crash-drill:\n    continue-on-error: true\n", 1),
			release:     goodRelease,
			wantFailure: "no-advisory",
		},
		{
			name:        "required gate becomes skippable",
			ci:          mustReplace(t, ciText, "  vuln-scan:\n", "  vuln-scan:\n    if: false\n", 1),
			release:     goodRelease,
			wantFailure: "no-advisory",
		},
		{
			name:        "a required gate job disappears",
			ci:          mustReplace(t, ciText, "  vip-failover:\n", "  vip-failover-old:\n", 1),
			release:     goodRelease,
			wantFailure: "required-gates",
		},
		{
			name:        "action becomes tag-pinned instead of SHA-pinned",
			ci:          mustReplace(t, ciText, "actions/checkout@"+pinSHA, "actions/checkout@v4", 1),
			release:     goodRelease,
			wantFailure: "sha-pins-ci",
		},
		{
			name:        "release workflow action becomes tag-pinned",
			ci:          ciText,
			release:     mustParse(t, []byte(mustReplace(t, relText, "actions/checkout@"+pinSHA, "actions/checkout@v4", 1))),
			wantFailure: "sha-pins-rel",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ci := mustParse(t, []byte(tc.ci))
			got := checkErrors(ci, tc.release, pins)
			failed := map[string]bool{}
			for name, errs := range got {
				if len(errs) > 0 {
					failed[name] = true
				}
			}
			if !failed[tc.wantFailure] {
				t.Fatalf("expected %s to reject the mutation; it passed. all: %+v", tc.wantFailure, got)
			}
			for name := range failed {
				if name != tc.wantFailure {
					t.Errorf("mutation also tripped unrelated check %s: %v", name, got[name])
				}
			}
		})
	}
}
