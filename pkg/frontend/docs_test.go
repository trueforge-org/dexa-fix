package frontend

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var (
	helpCall = regexp.MustCompile(`\{\{\s*help\s+"([^"]+)"\s*\}\}`)
	idAttr   = regexp.MustCompile(`\bid="([^"]+)"`)
)

// Every {{help "id"}} in the templates must point at an element of the documentation.
func TestHelpIDsExistInDocs(t *testing.T) {
	docs, err := fs.ReadFile(assets, "templates/docs.html")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range idAttr.FindAllStringSubmatch(string(docs), -1) {
		ids[m[1]] = true
	}
	used := 0
	err = fs.WalkDir(assets, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		for _, m := range helpCall.FindAllStringSubmatch(string(data), -1) {
			used++
			if !ids[m[1]] {
				t.Errorf("%s: help %q has no id=%q in docs.html", p, m[1], m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if used == 0 {
		t.Fatal("no help links found in the templates")
	}
}

func TestDocsIDsUnique(t *testing.T) {
	docs, err := fs.ReadFile(assets, "templates/docs.html")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range idAttr.FindAllStringSubmatch(string(docs), -1) {
		if seen[m[1]] {
			t.Errorf("duplicate id %q in docs.html", m[1])
		}
		seen[m[1]] = true
	}
}

// Every link in the table of contents must lead to an element.
func TestDocsTOCLinksResolve(t *testing.T) {
	docs, err := fs.ReadFile(assets, "templates/docs.html")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range idAttr.FindAllStringSubmatch(string(docs), -1) {
		ids[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`href="#([^"]+)"`).FindAllStringSubmatch(string(docs), -1) {
		if !ids[m[1]] {
			t.Errorf("link #%s has no target in docs.html", m[1])
		}
	}
}

func TestHelpFunc(t *testing.T) {
	got := string(help(`a"b`))
	if !strings.Contains(got, `href="/docs#a&#34;b"`) || !strings.Contains(got, `class="help-link"`) ||
		!strings.Contains(got, `rel="noopener"`) || !strings.Contains(got, `aria-label="Help"`) {
		t.Fatalf("unexpected help link: %s", got)
	}
}
