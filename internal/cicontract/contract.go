// Package cicontract makes the "all gates required / strictly offline" CI
// contract machine-checkable.
//
// r10 removed the last advisory and made the real-engine E2E a required gate
// on every event. r12 added a SECOND consumer of the same SHA256-pinned cache
// (`engine-tls-capability`, `needs: quack-e2e`). From that point on, a whole
// class of drift became possible that no individual gate can detect: a cache
// key or pin changed in one job but not the other, a job that can be skipped,
// a tag-pinned action, or a quietly reintroduced `continue-on-error`. These
// were previously only guaranteed by comments and prose.
//
// This package parses the real workflow files and asserts the contract. It is
// deliberately a normal Go test package, so it runs as part of the already
// required `go test ./...` job (Go 1.25 / 1.26 / 1.27 matrix) with `-race` and
// no new CI job, no downloads and no extra runtime. The negative controls in
// contract_test.go mutate the real YAML to prove each check is live rather
// than a tautology.
package cicontract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// RequiredGates are the nine real-system gates that must be unconditionally
// required on every event. Keep in sync with README.md and docs/SECURITY.md.
var RequiredGates = []string{
	"quack-e2e",
	"engine-tls-capability",
	"crash-drill",
	"deploy-verify",
	"systemd-drill",
	"vuln-scan",
	"plaintext-audit",
	"vip-failover",
	"windows-runtime-guard",
}

// cacheDirMarker identifies the shared pinned-engine cache steps.
const cacheDirMarker = "quack-e2e-cache"

var shaPinRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}$`)

// Workflow is the subset of a GitHub Actions workflow this contract cares
// about. Unknown keys are intentionally ignored.
type Workflow struct {
	Jobs map[string]Job `yaml:"jobs"`
}

// Job is a workflow job. `Needs` accepts both the scalar and sequence forms.
type Job struct {
	Name            string     `yaml:"name"`
	Needs           StringList `yaml:"needs"`
	If              string     `yaml:"if"`
	ContinueOnError bool       `yaml:"continue-on-error"`
	Steps           []Step     `yaml:"steps"`
}

// Step is a workflow step.
type Step struct {
	Name            string            `yaml:"name"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	With            map[string]string `yaml:"with"`
	Env             map[string]string `yaml:"env"`
	ContinueOnError bool              `yaml:"continue-on-error"`
}

// StringList unmarshals a YAML scalar or sequence into a string slice.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var one string
		if err := n.Decode(&one); err != nil {
			return err
		}
		if one == "" {
			*s = nil
		} else {
			*s = []string{one}
		}
		return nil
	case yaml.SequenceNode:
		var many []string
		if err := n.Decode(&many); err != nil {
			return err
		}
		*s = many
		return nil
	default:
		return fmt.Errorf("needs must be a scalar or a sequence, got kind %d", n.Kind)
	}
}

// ParseWorkflow parses workflow YAML text.
func ParseWorkflow(data []byte) (*Workflow, error) {
	var wf Workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, err
	}
	if wf.Jobs == nil {
		return nil, fmt.Errorf("no jobs found")
	}
	return &wf, nil
}

// CheckRequiredGates asserts every required gate job exists.
func CheckRequiredGates(wf *Workflow) []error {
	var errs []error
	for _, id := range RequiredGates {
		if _, ok := wf.Jobs[id]; !ok {
			errs = append(errs, fmt.Errorf("required gate job %q is missing", id))
		}
	}
	return errs
}

// CheckNoAdvisory asserts no gate can be skipped or silently fail.
// A job-level `if` is rejected because a conditionally skipped required gate
// is not required. Step-level and job-level `continue-on-error` are rejected
// because r5 showed it can hide a step that never actually ran.
func CheckNoAdvisory(wf *Workflow) []error {
	var errs []error
	for id, job := range wf.Jobs {
		if job.ContinueOnError {
			errs = append(errs, fmt.Errorf("job %q sets continue-on-error", id))
		}
		if strings.TrimSpace(job.If) != "" {
			errs = append(errs, fmt.Errorf("job %q has a conditional if: (a required gate must not be skippable)", id))
		}
		for i, step := range job.Steps {
			if step.ContinueOnError {
				errs = append(errs, fmt.Errorf("job %q step %d sets continue-on-error", id, i))
			}
		}
	}
	return errs
}

// CheckSHAPins asserts every `uses:` is pinned to a full 40-hex commit SHA.
func CheckSHAPins(wf *Workflow, label string) []error {
	var errs []error
	for id, job := range wf.Jobs {
		for i, step := range job.Steps {
			if step.Uses == "" {
				continue
			}
			if !shaPinRE.MatchString(step.Uses) {
				errs = append(errs, fmt.Errorf("%s: job %q step %d uses unpinned action %q", label, id, i, step.Uses))
			}
		}
	}
	return errs
}

// CheckCacheContract asserts all consumers of the pinned engine cache use the
// IDENTICAL path and key, and that the key still carries all three SHA256 pin
// prefixes from scripts/prefetch-quack-e2e.sh. This is the r12 `needs:
// quack-e2e` linkage that no individual gate can verify.
func CheckCacheContract(wf *Workflow, pinPrefixes []string) []error {
	var errs []error
	var wantPath, wantKey string
	count := 0
	for id, job := range wf.Jobs {
		for i, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "actions/cache@") {
				continue
			}
			if !strings.Contains(step.With["path"], cacheDirMarker) {
				continue
			}
			count++
			path, key := step.With["path"], step.With["key"]
			if path == "" || key == "" {
				errs = append(errs, fmt.Errorf("job %q step %d: cache path/key is empty", id, i))
				continue
			}
			if count == 1 {
				wantPath, wantKey = path, key
			} else {
				if path != wantPath {
					errs = append(errs, fmt.Errorf("job %q step %d: cache path %q != %q", id, i, path, wantPath))
				}
				if key != wantKey {
					errs = append(errs, fmt.Errorf("job %q step %d: cache key %q != %q (pin/key drift breaks needs: quack-e2e)", id, i, key, wantKey))
				}
			}
		}
	}
	if count < 2 {
		errs = append(errs, fmt.Errorf("expected at least 2 shared-cache consumers, found %d", count))
		return errs
	}
	for _, prefix := range pinPrefixes {
		if !strings.Contains(wantKey, prefix) {
			errs = append(errs, fmt.Errorf("cache key %q is missing pin prefix %q", wantKey, prefix))
		}
	}
	return errs
}

// CheckNeedsLinkage asserts the downstream TLS gate waits for the cache
// producer, so the cache is guaranteed to be filled and saved first.
func CheckNeedsLinkage(wf *Workflow) []error {
	job, ok := wf.Jobs["engine-tls-capability"]
	if !ok {
		return []error{fmt.Errorf("engine-tls-capability job missing")}
	}
	for _, need := range job.Needs {
		if need == "quack-e2e" {
			return nil
		}
	}
	return []error{fmt.Errorf("engine-tls-capability needs %v, want it to include quack-e2e", []string(job.Needs))}
}

// CheckOfflineEnv asserts the strictly-offline switches are actually set in
// CI, not just available in the scripts.
func CheckOfflineEnv(wf *Workflow) []error {
	var errs []error
	type want struct {
		job, envKey, runMarker string
	}
	for _, w := range []want{
		{"quack-e2e", "QUACK_E2E_OFFLINE", "QUACK_PREFETCH_OFFLINE=1"},
		{"engine-tls-capability", "QUACK_TLS_OFFLINE", "QUACK_PREFETCH_OFFLINE=1"},
	} {
		job, ok := wf.Jobs[w.job]
		if !ok {
			errs = append(errs, fmt.Errorf("job %q missing", w.job))
			continue
		}
		foundEnv, foundRun := false, false
		for _, step := range job.Steps {
			if step.Env[w.envKey] == "1" {
				foundEnv = true
			}
			if strings.Contains(step.Run, w.runMarker) {
				foundRun = true
			}
		}
		if !foundEnv {
			errs = append(errs, fmt.Errorf("job %q does not set %s=1", w.job, w.envKey))
		}
		if !foundRun {
			errs = append(errs, fmt.Errorf("job %q does not assert %s", w.job, w.runMarker))
		}
	}
	return errs
}

// Validate runs every contract check against both workflows.
func Validate(ci, release *Workflow, pinPrefixes []string) []error {
	var errs []error
	errs = append(errs, CheckRequiredGates(ci)...)
	errs = append(errs, CheckNoAdvisory(ci)...)
	errs = append(errs, CheckSHAPins(ci, "ci.yml")...)
	errs = append(errs, CheckSHAPins(release, "release.yml")...)
	errs = append(errs, CheckCacheContract(ci, pinPrefixes)...)
	errs = append(errs, CheckNeedsLinkage(ci)...)
	errs = append(errs, CheckOfflineEnv(ci)...)
	return errs
}

var pinRE = regexp.MustCompile(`(?m)^(CLI|EXT|HTTPFS)_SHA256="([0-9a-f]{64})"`)

// PinnedPrefixes reads the three SHA256 pins from scripts/prefetch-quack-e2e.sh
// and returns their short prefixes. Tying the CI cache key to the script's
// pins means changing a pin without bumping the key is a build failure.
func PinnedPrefixes(repoRoot string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "prefetch-quack-e2e.sh"))
	if err != nil {
		return nil, err
	}
	matches := pinRE.FindAllStringSubmatch(string(data), -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		prefix := m[2][:8]
		if !seen[prefix] {
			seen[prefix] = true
			out = append(out, prefix)
		}
	}
	if len(out) != 3 {
		return nil, fmt.Errorf("expected 3 SHA256 pins (CLI/EXT/HTTPFS), found %d", len(out))
	}
	return out, nil
}
