package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// S11 — configuration documentation must cover every backend environment variable.
// The full reference lives in docs/reference.md so the README can stay concise.
// Tests run in backend/; the injectable path lets fixtures verify the failure
// direction without modifying real documentation. Both os.Getenv and
// splitEnvList are scanned.
var s11ReferencePath = "../docs/reference.md"

var s11EnvVarPattern = regexp.MustCompile(`(?:os\.Getenv|splitEnvList)\("([A-Z_]+)"`)

// s11EnvVarsReadByBackend scans every non-test .go file in the current
// directory (backend/) and returns the sorted-by-first-appearance, deduped set
// of environment variable names read via os.Getenv or splitEnvList.
func s11EnvVarsReadByBackend(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read backend dir: %v", err)
	}
	seen := map[string]bool{}
	var vars []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range s11EnvVarPattern.FindAllStringSubmatch(string(data), -1) {
			v := m[1]
			if !seen[v] {
				seen[v] = true
				vars = append(vars, v)
			}
		}
	}
	if len(vars) == 0 {
		t.Fatalf("scan found zero env vars read by backend/*.go — the scan itself is broken")
	}
	return vars
}

// s11MissingFromReference returns the subset of vars that do not appear anywhere
// in the reference at referencePath.
func s11MissingFromReference(t *testing.T, vars []string, referencePath string) []string {
	t.Helper()
	data, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatalf("read %s: %v", referencePath, err)
	}
	content := string(data)
	var missing []string
	for _, v := range vars {
		if !strings.Contains(content, v) {
			missing = append(missing, v)
		}
	}
	return missing
}

// TestS11ReferenceDocumentsEveryBackendEnvVar is the gate itself: every env var
// read by non-test backend/*.go must be named in the real reference.
func TestS11ReferenceDocumentsEveryBackendEnvVar(t *testing.T) {
	vars := s11EnvVarsReadByBackend(t)
	missing := s11MissingFromReference(t, vars, s11ReferencePath)
	if len(missing) > 0 {
		t.Fatalf("reference.md is missing documentation for env var(s): %v", missing)
	}
}

// TestS11GateFailsOnFixtureMissingAnEnvVar proves the failure direction
// without touching the real reference: it points the injectable path at a
// synthetic fixture that omits one variable the scan found, and asserts the
// gate's own missing-list catches exactly that gap.
func TestS11GateFailsOnFixtureMissingAnEnvVar(t *testing.T) {
	vars := s11EnvVarsReadByBackend(t)
	if len(vars) == 0 {
		t.Fatal("no env vars found to build a fixture from")
	}
	omit := vars[0]

	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "reference.md")
	// A reference that documents every found var except the omitted one.
	var b strings.Builder
	b.WriteString("# Fixture reference\n\n")
	for _, v := range vars {
		if v == omit {
			continue
		}
		b.WriteString("`" + v + "`\n")
	}
	if err := os.WriteFile(fixturePath, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	missing := s11MissingFromReference(t, vars, fixturePath)
	if len(missing) != 1 || missing[0] != omit {
		t.Fatalf("gate did not catch the omitted var: missing=%v, want [%s]", missing, omit)
	}

	// Sanity check the other direction on the same fixture: a var that IS
	// present must not be reported missing.
	if len(vars) > 1 {
		present := vars[1]
		if present == omit {
			t.Fatalf("test setup error: present var equals omitted var")
		}
		for _, m := range missing {
			if m == present {
				t.Fatalf("gate falsely reported present var %q as missing", present)
			}
		}
	}
}
