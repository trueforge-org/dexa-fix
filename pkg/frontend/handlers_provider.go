package frontend

import (
	"net/http"
)

// providerStart redirects to the provider, to sign in or, for a signed-in POST, to link it. Linking
// is POST-only (and so same-origin checked): a link anyone could put on a web page must not be able
// to attach someone else's provider account to the visitor's session. The backend decides who is
// signed in, asks for the current password and keeps the state cookie.
func (s *Frontend) providerStart(w http.ResponseWriter, r *http.Request) {
	link := r.Method == http.MethodPost
	o, err := s.api.ProviderStart(r.Context(), r.PathValue("provider"), link, r.PostFormValue("current"))
	if err != nil && link {
		if u := currentUser(r); u != nil {
			s.settingsError(w, r, *u, err)
			return
		}
	}
	s.signInOutcome(w, r, o, err)
}

// providerCallback finishes a sign-in. The provider's answer arrives in the query or, for Apple, in
// a cross-site form post; the backend gets every parameter.
func (s *Frontend) providerCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid or expired sign-in, please start again", http.StatusBadRequest)
		return
	}
	params := make(map[string]string, len(r.Form))
	for k := range r.Form {
		params[k] = r.Form.Get(k)
	}
	o, err := s.api.ProviderCallback(r.Context(), r.PathValue("provider"), params)
	s.signInOutcome(w, r, o, err)
}
