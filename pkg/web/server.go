// Package web serves the HTML frontend (Go templates + htmx) and the JSON API.
package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

//go:embed templates static
var assets embed.FS

// Server wires the store to HTTP handlers.
type Server struct {
	store store.Store
	tmpl  map[string]*template.Template
}

var pages = []string{"index.html", "dexa.html", "scale.html"}

// New builds a Server and parses the embedded templates.
func New(st store.Store) (*Server, error) {
	funcs := template.FuncMap{
		"day": func(t time.Time) string { return t.Format("2006-01-02") },
		"dt":  func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	}
	s := &Server{store: st, tmpl: map[string]*template.Template{}}
	for _, p := range pages {
		t, err := template.New(p).Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		s.tmpl[p] = t
	}
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /users", s.createUser)
	mux.HandleFunc("GET /u/{id}", s.user(s.home))

	mux.HandleFunc("GET /u/{id}/dexa", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) { s.dexaPage(w, r, u, "", http.StatusOK) }))
	mux.HandleFunc("POST /u/{id}/dexa", s.user(s.dexaCreate))
	mux.HandleFunc("POST /u/{id}/dexa/{item}/delete", s.user(s.dexaDelete))

	mux.HandleFunc("GET /u/{id}/scale", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) { s.scalePage(w, r, u, "", http.StatusOK) }))
	mux.HandleFunc("POST /u/{id}/scale", s.user(s.scaleCreate))
	mux.HandleFunc("POST /u/{id}/scale/{item}/delete", s.user(s.scaleDelete))
	return logRequests(mux)
}

// pageData is the data passed to every template.
type pageData struct {
	Title  string
	Tab    string
	User   *models.User
	Users  []models.User
	Error  string
	Fields []field
	Rows   []row
	Today  string
	Now    string
}

func (s *Server) render(w http.ResponseWriter, page string, status int, data pageData) {
	var buf bytes.Buffer
	if err := s.tmpl[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Error().Err(err).Str("page", page).Msg("render failed")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrExists):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		log.Error().Err(err).Msg("request failed")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// user resolves {id} to a user (404 if unknown) before calling h.
func (s *Server) user(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		u, err := s.store.GetUser(r.Context(), id)
		if err != nil {
			s.fail(w, err)
			return
		}
		h(w, r, u)
	}
}

func itemID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	return id, err == nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug().Str("method", r.Method).Str("path", r.URL.Path).Dur("took", time.Since(start)).Msg("request")
	})
}

// ---- users ----

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.renderIndex(w, r, "", http.StatusOK)
}

func (s *Server) renderIndex(w http.ResponseWriter, r *http.Request, errMsg string, status int) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "index.html", status, pageData{Title: "dexa-fix", Users: users, Error: errMsg})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	u, err := s.store.CreateUser(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrExists):
		// no auth: entering an existing name simply opens that user
		users, lerr := s.store.ListUsers(r.Context())
		if lerr != nil {
			s.fail(w, lerr)
			return
		}
		for _, existing := range users {
			if existing.Name == name {
				http.Redirect(w, r, userPath(existing.ID, ""), http.StatusSeeOther)
				return
			}
		}
		s.fail(w, err)
	case errors.Is(err, store.ErrInvalid):
		s.renderIndex(w, r, "Please enter a name of 1-64 characters.", http.StatusUnprocessableEntity)
	case err != nil:
		s.fail(w, err)
	default:
		http.Redirect(w, r, userPath(u.ID, ""), http.StatusSeeOther)
	}
}

func userPath(id int64, tab string) string {
	p := "/u/" + strconv.FormatInt(id, 10)
	if tab != "" {
		p += "/" + tab
	}
	return p
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, u models.User) {
	http.Redirect(w, r, userPath(u.ID, "scale"), http.StatusSeeOther)
}
