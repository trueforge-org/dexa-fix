package web

import (
	"html/template"
	"net/http"
)

// help returns a small "?" link to the section of the documentation page with the given id. The
// ids are constants in the templates; docs_test.go checks that each one exists in docs.html.
func help(id string) template.HTML {
	return template.HTML(`<a class="help-link" href="/docs#` + template.HTMLEscapeString(id) +
		`" target="_blank" rel="noopener" title="Help: what is this?" aria-label="Help">?</a>`)
}

// docsPage serves the documentation. It is public: signed-in visitors get their own navigation.
func (s *Server) docsPage(w http.ResponseWriter, r *http.Request) {
	data := pageData{Title: "Documentation · dexa-fix", Tab: "docs", User: currentUser(r)}
	s.render(w, "docs.html", http.StatusOK, data)
}
