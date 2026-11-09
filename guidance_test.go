package main

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The guidance for AI agents has one canonical source, AGENTS.md, and path-specific rules in
// .github/instructions (read by Copilot through applyTo and by Claude Code through paths, via
// symlinks in .claude/rules). These tests keep that structure honest: the two path lists agree,
// every rule is linked, and the paths named in the rules still match files, so a refactor that moves
// a directory cannot leave a rule pointing at nothing.

const (
	instructionsDir = ".github/instructions"
	rulesDir        = ".claude/rules"
)

type rule struct {
	name        string
	applyTo     []string
	paths       []string
	description string
}

func parseRule(t *testing.T, file string) rule {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\n") {
		t.Fatalf("%s: no frontmatter", file)
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		t.Fatalf("%s: frontmatter is not closed", file)
	}
	r := rule{name: filepath.Base(file)}
	inPaths := false
	for _, line := range strings.Split(s[4:4+end], "\n") {
		switch {
		case strings.HasPrefix(line, "applyTo:"):
			inPaths = false
			for _, p := range strings.Split(unquote(strings.TrimPrefix(line, "applyTo:")), ",") {
				r.applyTo = append(r.applyTo, strings.TrimSpace(p))
			}
		case strings.HasPrefix(line, "description:"):
			inPaths = false
			r.description = unquote(strings.TrimPrefix(line, "description:"))
		case strings.HasPrefix(line, "paths:"):
			inPaths = true
		case inPaths && strings.HasPrefix(strings.TrimSpace(line), "- "):
			r.paths = append(r.paths, unquote(strings.TrimPrefix(strings.TrimSpace(line), "- ")))
		}
	}
	return r
}

func unquote(s string) string { return strings.Trim(strings.TrimSpace(s), `"`) }

func rules(t *testing.T) []rule {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(instructionsDir, "*.instructions.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no instruction files: %v", err)
	}
	var out []rule
	for _, f := range files {
		out = append(out, parseRule(t, f))
	}
	return out
}

func TestRulesHaveMatchingApplyToAndPaths(t *testing.T) {
	for _, r := range rules(t) {
		if r.description == "" || len(r.applyTo) == 0 || len(r.paths) == 0 {
			t.Errorf("%s: applyTo, paths and description are all required", r.name)
			continue
		}
		a, p := append([]string(nil), r.applyTo...), append([]string(nil), r.paths...)
		sort.Strings(a)
		sort.Strings(p)
		if strings.Join(a, "|") != strings.Join(p, "|") {
			t.Errorf("%s: applyTo %v and paths %v differ: keep them in sync", r.name, r.applyTo, r.paths)
		}
	}
}

func TestEveryRuleIsLinkedForClaudeCode(t *testing.T) {
	for _, r := range rules(t) {
		link := filepath.Join(rulesDir, r.name)
		target, err := os.Readlink(link)
		if err != nil {
			t.Errorf("%s: no symlink in %s (ln -s ../../%s/%s %s/)", r.name, rulesDir, instructionsDir, r.name, rulesDir)
			continue
		}
		if want := "../../" + instructionsDir + "/" + r.name; target != want {
			t.Errorf("%s: points to %s, want %s", link, target, want)
		}
		if _, err := os.Stat(link); err != nil {
			t.Errorf("%s: dangling symlink", link)
		}
	}
	entries, _ := os.ReadDir(rulesDir)
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 {
			continue // a Claude-only rule such as dexa-fix.md
		}
		if _, err := os.Stat(filepath.Join(instructionsDir, e.Name())); err != nil {
			t.Errorf("%s/%s links to a rule that does not exist", rulesDir, e.Name())
		}
	}
}

// match reports whether file matches a glob where ** stands for any number of directories.
func match(pattern, file string) bool {
	return matchParts(strings.Split(pattern, "/"), strings.Split(file, "/"))
}

func matchParts(pat, parts []string) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		if len(pat) == 1 {
			return len(parts) > 0
		}
		for i := 0; i <= len(parts); i++ {
			if matchParts(pat[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	if ok, _ := path.Match(pat[0], parts[0]); !ok {
		return false
	}
	return matchParts(pat[1:], parts[1:])
}

func repoFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch p {
			case ".git", ".claude/worktrees", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, filepath.ToSlash(p))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRulePathsStillMatchFiles(t *testing.T) {
	files := repoFiles(t)
	for _, r := range rules(t) {
		for _, p := range r.paths {
			found := false
			for _, f := range files {
				if match(p, f) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: %q matches no file: the directory moved or the rule is stale", r.name, p)
			}
		}
	}
}

func TestMatchHelper(t *testing.T) {
	for _, c := range []struct {
		pattern, file string
		want          bool
	}{
		{"pkg/frontend/**", "pkg/frontend/server.go", true},
		{"pkg/frontend/**", "pkg/frontend/templates/a.html", true},
		{"pkg/frontend/**", "pkg/backend/server.go", false},
		{"**/*_test.go", "a_test.go", true},
		{"**/*_test.go", "pkg/x/y/a_test.go", true},
		{"**/*_test.go", "pkg/x/a.go", false},
		{"pkg/backend/**/internal*.go", "pkg/backend/main/internal_admin.go", true},
		{"pkg/backend/**/internal*.go", "pkg/backend/main/api.go", false},
		{"main.go", "main.go", true},
		{"main.go", "cmd/main.go", false},
	} {
		if got := match(c.pattern, c.file); got != c.want {
			t.Errorf("match(%q, %q) = %v", c.pattern, c.file, got)
		}
	}
}

var mdLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// Relative links in the guidance must point to files that exist.
func TestGuidanceLinksResolve(t *testing.T) {
	docs := []string{"AGENTS.md", ".github/copilot-instructions.md", ".claude/rules/dexa-fix.md"}
	rs, _ := filepath.Glob(filepath.Join(instructionsDir, "*.instructions.md"))
	docs = append(docs, rs...)
	for _, d := range docs {
		b, err := os.ReadFile(d)
		if err != nil {
			t.Errorf("%s: %v", d, err)
			continue
		}
		for _, m := range mdLink.FindAllStringSubmatch(string(b), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			if _, err := os.Stat(filepath.Join(filepath.Dir(d), target)); err != nil {
				t.Errorf("%s links to %s, which does not exist", d, m[1])
			}
		}
	}
}

func TestAgentsMdListsEveryRule(t *testing.T) {
	b, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules(t) {
		if !strings.Contains(string(b), instructionsDir+"/"+r.name) {
			t.Errorf("AGENTS.md does not link %s: add it to the table", r.name)
		}
	}
}
