package mainbackend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

// rig is a backend with nothing in front of it, called the way the frontend calls it: through the
// internal handler, with the service secret, the visitor's cookies and the visitor's address in
// headers. It is for the tests of the sign-in, security and two-step endpoints that need a request
// the pages never send (garbage JSON, a replayed cookie) or a store that fails.
type rig struct {
	t   *testing.T
	b   *Backend
	srv *Server // the backend alone, which is all the helpers of the second-step tests need
	st  *sqlstore.Store
	h   http.Handler
}

const rigSecret = "rig-service-secret-0123456789abcdef"

// reply is what the rig got back.
type reply struct {
	*httptest.ResponseRecorder
}

func (r reply) body() string { return r.Body.String() }

// decode reads the JSON body into v.
func (r reply) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		t.Fatalf("answer is not JSON: %v\n%s", err, r.Body.String())
	}
}

// cookie returns the cookie of that name the answer sets, or nil.
func (r reply) cookie(name string) *http.Cookie {
	for _, c := range r.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// code is the error code of an apiclient.Error answer.
func (r reply) code(t *testing.T) string {
	t.Helper()
	var e apiclient.Error
	r.decode(t, &e)
	return e.Code
}

func newRig(t *testing.T, opts ...Option) *rig {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutSetting(context.Background(), settingSignup, valueEnabled); err != nil {
		t.Fatal(err)
	}
	return newRigOn(t, st, st, opts...)
}

// newRigOn builds the rig on a store that may be a wrapper around the real one.
func newRigOn(t *testing.T, st store.Store, real *sqlstore.Store, opts ...Option) *rig {
	t.Helper()
	b, err := NewBackend(st, append([]Option{testBody(st)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.DisableInternalBudget()
	b.SetServiceSecrets(rigSecret)
	return &rig{t: t, b: b, srv: &Server{Backend: b}, st: real, h: b.InternalHandler()}
}

// do sends one request. A body that is not a string is sent as JSON.
func (g *rig) do(method, path string, body any, cookies ...*http.Cookie) reply {
	g.t.Helper()
	return g.doFrom("192.0.2.10", method, path, body, cookies...)
}

// doFrom is do from a given client address.
func (g *rig) doFrom(addr, method, path string, body any, cookies ...*http.Cookie) reply {
	g.t.Helper()
	return g.send(addr, method, path, nil, body, cookies...)
}

// doWithToken is do with a password reset token in its header.
func (g *rig) doWithToken(method, path, token string, body any) reply {
	g.t.Helper()
	return g.send("192.0.2.10", method, path, map[string]string{apiclient.HeaderResetToken: token}, body)
}

// send is the general form: address, extra headers, body, cookies.
func (g *rig) send(addr, method, path string, headers map[string]string, body any, cookies ...*http.Cookie) reply {
	g.t.Helper()
	var raw string
	switch v := body.(type) {
	case nil:
	case string:
		raw = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			g.t.Fatal(err)
		}
		raw = string(b)
	}
	req := httptest.NewRequest(method, apiclient.Prefix+path, strings.NewReader(raw))
	req.Header.Set(apiclient.HeaderService, rigSecret)
	req.Header.Set(apiclient.HeaderClientIP, addr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	var jar []string
	for _, c := range cookies {
		if c != nil {
			jar = append(jar, c.Name+"="+c.Value)
		}
	}
	if len(jar) > 0 {
		req.Header.Set(apiclient.HeaderCookie, strings.Join(jar, "; "))
	}
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	return reply{rec}
}

// signUp makes an account through the sign-up endpoint and returns its id and session cookie.
func (g *rig) signUp(name string) (int64, *http.Cookie) {
	g.t.Helper()
	rep := g.do("POST", "/auth/signup", map[string]string{"Username": name, "Email": name + "@example.com", "Password": testPassword, "Confirm": testPassword})
	ck := rep.cookie(sessionCookie)
	if rep.Code != 200 || ck == nil {
		g.t.Fatalf("sign up %s: %d %s", name, rep.Code, rep.body())
	}
	u, err := g.st.GetUserByEmail(context.Background(), name+"@example.com")
	if err != nil {
		g.t.Fatal(err)
	}
	return u.ID, ck
}

// signIn is a password sign-in; it returns the answer.
func (g *rig) signIn(name string) reply {
	g.t.Helper()
	return g.do("POST", "/auth/password", map[string]string{"Email": name + "@example.com", "Password": testPassword})
}

// audited reports whether an event of that kind was recorded.
func (g *rig) audited(kind string) bool {
	g.t.Helper()
	evs, err := g.st.ListAuditEvents(context.Background(), store.AuditFilter{})
	if err != nil {
		g.t.Fatal(err)
	}
	for _, e := range evs {
		if e.Action == kind {
			return true
		}
	}
	return false
}

// rejectWritesOn makes every INSERT, UPDATE or DELETE on the table fail while reads keep working,
// with a trigger. It is how a test makes a store write fail after the reads before it went well.
func rejectWritesOn(t *testing.T, st *sqlstore.Store, table string, ops ...string) {
	t.Helper()
	if len(ops) == 0 {
		ops = []string{"INSERT", "UPDATE", "DELETE"}
	}
	for _, op := range ops {
		err := st.ExecForTests(context.Background(), "CREATE TRIGGER reject_"+strings.ToLower(op)+"_"+table+" BEFORE "+op+" ON "+table+" BEGIN SELECT RAISE(ABORT, 'the test broke this table'); END")
		if err != nil {
			t.Fatalf("reject %s on %s: %v", op, table, err)
		}
	}
}

// formOf is a url.Values for a sealed-state test.
func formOf(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

var _ = models.ProviderLocal

// auditedWith reports whether an event of that kind with that detail was recorded.
func (g *rig) auditedWith(kind, detail string) bool {
	g.t.Helper()
	evs, err := g.st.ListAuditEvents(context.Background(), store.AuditFilter{})
	if err != nil {
		g.t.Fatal(err)
	}
	for _, e := range evs {
		if e.Action == kind && e.Detail == detail {
			return true
		}
	}
	return false
}

// blindToNonces is a store that never sees a spent nonce, as when two requests with the same
// cookie arrive together: both pass the first look and only the single-use check tells them apart.
type blindToNonces struct{ store.Store }

func (blindToNonces) NonceSeen(context.Context, string) (bool, error) { return false, nil }

// captureSignInLog collects what the backend logs while the test runs.
func captureSignInLog(t *testing.T) *logLines { return captureLog(t) }

func (b blindToNonces) Unwrap() store.Store { return b.Store }
