package frontend

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog/log"
)

// staticVersions maps every embedded static file to a short content hash.
func staticVersions(fsys fs.FS) map[string]string {
	out := map[string]string{}
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		out[p] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	return out
}

// staticHandler serves the embedded files. Embedded files have no modification time, so without
// help browsers would download them again on every page. Pages reference them as
// /static/name?v=<hash>, which can be cached for good; a bare URL revalidates through its ETag.
func staticHandler(fsys fs.FS, versions map[string]string) http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if name == "" || strings.HasSuffix(name, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		if v, ok := versions[name]; ok {
			w.Header().Set("ETag", `"`+v+`"`)
			if r.URL.Query().Get("v") == v {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
		}
		files.ServeHTTP(w, r)
	})
}

// newCrossOriginProtection builds the same-origin check that guards the cookie-authenticated
// forms. The Apple callback arrives as a cross-site POST and is protected by its signed state
// instead. A configured base URL is a trusted origin too.
func newCrossOriginProtection(base *url.URL) *http.CrossOriginProtection {
	cop := http.NewCrossOriginProtection()
	cop.AddInsecureBypassPattern("POST /auth/{provider}/callback")
	if base != nil {
		if err := cop.AddTrustedOrigin(base.Scheme + "://" + base.Host); err != nil {
			log.Error().Err(err).Msg("base URL not accepted as a trusted origin")
		}
	}
	return cop
}
