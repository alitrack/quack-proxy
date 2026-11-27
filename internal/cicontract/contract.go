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
	"sort"
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

// Validate runs every contract check against both workflows AND against the
// shell scripts that declare the pinned engine hashes and gate image digests.
func Validate(ci, release *Workflow, pinPrefixes []string, repoRoot string) []error {
	var errs []error
	errs = append(errs, CheckRequiredGates(ci)...)
	errs = append(errs, CheckNoAdvisory(ci)...)
	errs = append(errs, CheckSHAPins(ci, "ci.yml")...)
	errs = append(errs, CheckSHAPins(release, "release.yml")...)
	errs = append(errs, CheckCacheContract(ci, pinPrefixes)...)
	errs = append(errs, CheckNeedsLinkage(ci)...)
	errs = append(errs, CheckOfflineEnv(ci)...)
	errs = append(errs, CheckNoImageOverrides(ci)...)
	errs = append(errs, CheckScriptPins(repoRoot)...)
	errs = append(errs, CheckImagePins(repoRoot)...)
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

// pinnedScripts are the shell scripts that hardcode the DuckDB engine pins.
// The producer (prefetch-quack-e2e.sh) is the source of truth; quack-e2e.sh
// and verify-engine-tls.sh consume the very same cache. Before r14 the
// "keep all three in sync when upgrading DuckDB" rule existed only as prose
// in REMEDIATION.md — a manual convention, not an invariant.
var pinnedScripts = []string{
	"prefetch-quack-e2e.sh",
	"quack-e2e.sh",
	"verify-engine-tls.sh",
}

var scriptConstRE = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)="([^"]*)"$`)

// scriptPins reads only the constants that describe the pinned engine, so
// unrelated shell variables cannot create false drift.
func scriptPins(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range scriptConstRE.FindAllStringSubmatch(string(data), -1) {
		switch m[1] {
		case "DUCKDB_VERSION", "CLI_SHA256", "EXT_SHA256", "HTTPFS_SHA256", "EXT_RELPATH":
			out[m[1]] = m[2]
		}
	}
	return out, nil
}

// CheckScriptPins asserts every offline gate declares the SAME pinned engine
// version, extension hashes and extension path as the cache producer, and
// that the TLS gate's CLI hash matches too. Upgrading DuckDB now requires a
// single coordinated edit instead of relying on a human remembering to check
// three scripts plus the CI cache key.
func CheckScriptPins(repoRoot string) []error {
	read := func(name string) (map[string]string, error) {
		return scriptPins(filepath.Join(repoRoot, "scripts", name))
	}
	base, err := read(pinnedScripts[0])
	if err != nil {
		return []error{err}
	}
	producerWide := []string{"DUCKDB_VERSION", "CLI_SHA256", "EXT_SHA256", "HTTPFS_SHA256", "EXT_RELPATH"}
	for _, k := range producerWide {
		if base[k] == "" {
			return []error{fmt.Errorf("%s is missing %s (the cache producer is the source of truth)", pinnedScripts[0], k)}
		}
	}

	var errs []error
	// Every consumer must carry these shared values explicitly.
	shared := []string{"DUCKDB_VERSION", "EXT_SHA256", "HTTPFS_SHA256", "EXT_RELPATH"}
	for _, name := range pinnedScripts[1:] {
		got, err := read(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, k := range shared {
			if got[k] == "" {
				errs = append(errs, fmt.Errorf("%s is missing %s", name, k))
				continue
			}
			if got[k] != base[k] {
				errs = append(errs, fmt.Errorf("%s %s=%q != %s %s=%q (pinned engine drift; update all scripts and the CI cache key together)", name, k, got[k], pinnedScripts[0], k, base[k]))
			}
		}
	}
	// verify-engine-tls.sh additionally hardcodes the CLI hash and must match
	// the producer exactly (quack-e2e.sh does not use the CLI tarball).
	tls, err := read("verify-engine-tls.sh")
	if err != nil {
		errs = append(errs, err)
	} else if tls["CLI_SHA256"] == "" {
		errs = append(errs, fmt.Errorf("verify-engine-tls.sh is missing CLI_SHA256"))
	} else if tls["CLI_SHA256"] != base["CLI_SHA256"] {
		errs = append(errs, fmt.Errorf("verify-engine-tls.sh CLI_SHA256=%q != %s CLI_SHA256=%q", tls["CLI_SHA256"], pinnedScripts[0], base["CLI_SHA256"]))
	}
	return errs
}

// ---------------------------------------------------------------------------
// r17: container-image supply chain.
//
// Every `uses:` has been SHA-pinned since r5 and machine-checked by
// CheckSHAPins, but the images that the REQUIRED real-system gates actually
// execute were still floating tags (including alpine:latest, which is
// republished constantly). A republished tag silently changes what a required
// gate runs, so "nine gates are unconditionally required" was only as strong
// as an unversioned image. The scripts' DEFAULT images are now pinned by
// manifest digest. Operators may still override them locally; CI must not
// (CheckNoImageOverrides), otherwise the pin is decorative.
// ---------------------------------------------------------------------------

// imageDefaultRE captures the default value of a shell image constant, e.g.
//
//	IMAGE="${PLAINTEXT_IMAGE:-python@sha256:...}"
//
// It deliberately anchors at the start of a line so usage examples in
// comments cannot be mistaken for the value CI actually uses.
var imageDefaultRE = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*_IMAGE|IMAGE)="\$\{[A-Z][A-Z0-9_]*:-([^"]+)\}"$`)

// digestImageRE is the supply-chain standard for gate images: a repository
// plus a FULL 64-hex sha256 manifest digest, never a floating tag.
var digestImageRE = regexp.MustCompile(`^[a-z0-9._/-]+@sha256:[0-9a-f]{64}$`)

// imageOverrideMarkers are the ways a workflow step could bypass a
// digest-pinned default by supplying a (possibly tag-based) image of its own.
var imageOverrideMarkers = []string{
	"--image", "--client-image",
	"PLAINTEXT_IMAGE=", "PLAINTEXT_HAPROXY_IMAGE=",
	"VIP_IMAGE=", "VIP_CLIENT_IMAGE=",
}

// CheckImagePins asserts every gate image default is digest-pinned and that
// the same repository is pinned to the SAME digest across scripts (e.g. the
// shared haproxy image must be rotated in both drills together). It scans all
// scripts/*.sh, so a newly added drill is covered without editing this file.
func CheckImagePins(repoRoot string) []error {
	paths, err := filepath.Glob(filepath.Join(repoRoot, "scripts", "*.sh"))
	if err != nil {
		return []error{err}
	}
	var errs []error
	total := 0
	byRepo := map[string]map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return []error{err}
		}
		name := filepath.Base(path)
		for _, m := range imageDefaultRE.FindAllStringSubmatch(string(data), -1) {
			total++
			val := m[2]
			if !digestImageRE.MatchString(val) {
				errs = append(errs, fmt.Errorf("%s: %s defaults to %q, which is not a sha256 digest-pinned image (floating tags are forbidden in required gates)", name, m[1], val))
				continue
			}
			parts := strings.SplitN(val, "@sha256:", 2)
			if byRepo[parts[0]] == nil {
				byRepo[parts[0]] = map[string]string{}
			}
			byRepo[parts[0]][name] = parts[1]
		}
	}
	// Guard against the regex quietly going stale (e.g. a renamed variable
	// style) and turning this check into a tautology.
	if total < 4 {
		errs = append(errs, fmt.Errorf("expected at least 4 digest-pinned default gate images, found %d (image pin regex may be stale)", total))
	}
	for repo, perScript := range byRepo {
		var scripts []string
		for script := range perScript {
			scripts = append(scripts, script)
		}
		sort.Strings(scripts)
		want := perScript[scripts[0]]
		for _, script := range scripts[1:] {
			if perScript[script] != want {
				errs = append(errs, fmt.Errorf("image %q is pinned to different digests across gate scripts (%s); rotate them together", repo, strings.Join(scripts, ", ")))
				break
			}
		}
	}
	return errs
}

// CheckNoImageOverrides asserts CI never passes its own image to a gate.
// Without this, a digest-pinned default could be replaced by a floating tag
// in one workflow step while every contract check still passed.
func CheckNoImageOverrides(wf *Workflow) []error {
	var errs []error
	for id, job := range wf.Jobs {
		for i, step := range job.Steps {
			for _, marker := range imageOverrideMarkers {
				if strings.Contains(step.Run, marker) {
					errs = append(errs, fmt.Errorf("ci.yml: job %q step %d sets %q, which bypasses the digest-pinned default image", id, i, marker))
				}
			}
		}
	}
	return errs
}
