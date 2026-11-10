package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/edge"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// pkLocalhost makes the rig's requests come from a browser on localhost, the one place passkeys
// work without HTTPS.
var pkLocalhost = map[string]string{apiclient.HeaderHost: "localhost"}

var pkLocalSite = pkSite{host: "localhost", origin: "http://localhost"}

// handleFails is a store that cannot make the WebAuthn user handle of an account.
type handleFails struct{ store.Store }

func (handleFails) EnsurePasskeyHandle(context.Context, int64) (string, error) {
	return "", errors.New("the store lost the handle")
}
func (h handleFails) Unwrap() store.Store { return h.Store }

// providersFail is a store that cannot list the ways an account can sign in.
type providersFail struct{ store.Store }

func (providersFail) UserProviders(context.Context, int64) ([]string, error) {
	return nil, errors.New("the store lost the providers")
}
func (p providersFail) Unwrap() store.Store { return p.Store }

// beginRegistration starts adding a passkey. It returns the options, the ceremony cookie and the answer.
func (g *rig) beginRegistration(uid int64, session *http.Cookie, body map[string]string) (pkCreationOptions, *http.Cookie, reply) {
	g.t.Helper()
	rep := g.send("192.0.2.10", "POST", fmt.Sprintf("/users/%d/passkeys/begin", uid), pkLocalhost, body, session)
	var opts pkCreationOptions
	if rep.Code == 200 {
		rep.decode(g.t, &opts)
	}
	return opts, rep.cookie(passkeyCookie), rep
}

// attestation is what the authenticator k answers to the options.
func (g *rig) attestation(opts pkCreationOptions, k pkKey) string {
	g.t.Helper()
	cdj := pkClientData(g.t, "webauthn.create", opts.PublicKey.Challenge, pkLocalSite.origin)
	att := pkCBOR(g.t, map[string]any{"fmt": "none", "attStmt": map[string]any{},
		"authData": pkAuthData(pkRPID, pkFlagsReg, 0, k.attestedData(g.t))})
	return pkJSON(g.t, map[string]any{"id": k.cred(), "rawId": k.cred(), "type": "public-key",
		"response": map[string]any{"attestationObject": pkB64(att), "clientDataJSON": pkB64(cdj)}})
}

func (g *rig) finishRegistration(uid int64, payload string, cookies ...*http.Cookie) reply {
	g.t.Helper()
	return g.send("192.0.2.10", "POST", fmt.Sprintf("/users/%d/passkeys/finish", uid), pkLocalhost, payload, cookies...)
}

// addPasskey registers k for the account.
func (g *rig) addPasskey(uid int64, session *http.Cookie, k pkKey) {
	g.t.Helper()
	opts, ceremony, rep := g.beginRegistration(uid, session, map[string]string{"Name": "key", "Current": testPassword})
	if rep.Code != 200 {
		g.t.Fatalf("begin: %d %s", rep.Code, rep.body())
	}
	if rep := g.finishRegistration(uid, g.attestation(opts, k), session, ceremony); rep.Code != 200 {
		g.t.Fatalf("finish: %d %s", rep.Code, rep.body())
	}
}

// beginLogin starts a passkey sign-in.
func (g *rig) beginLogin() (string, *http.Cookie) {
	g.t.Helper()
	rep := g.send("192.0.2.10", "POST", "/auth/passkey/begin", pkLocalhost, nil)
	if rep.Code != 200 {
		g.t.Fatalf("begin sign-in: %d %s", rep.Code, rep.body())
	}
	var opts pkRequestOptions
	rep.decode(g.t, &opts)
	return opts.PublicKey.Challenge, rep.cookie(passkeyCookie)
}

func (g *rig) finishLogin(payload string, cookies ...*http.Cookie) reply {
	g.t.Helper()
	return g.send("192.0.2.10", "POST", "/auth/passkey/finish", pkLocalhost, payload, cookies...)
}

func TestPasskeyStepsRefuseAGarbledBodyAndAnotherUsersPasskey(t *testing.T) {
	g := newRig(t)
	alice, aliceSession := g.signUp("alice")
	_, bobSession := g.signUp("bob")
	k := pkNewKey(t)
	g.addPasskey(alice, aliceSession, k)
	bob, _ := g.st.GetUserByEmail(context.Background(), "bob@example.com")

	for _, action := range []string{"begin", "{cred}/rename"} {
		t.Run("a garbled body to "+action, func(t *testing.T) {
			wantStatus(t, g.do("POST", fmt.Sprintf("/users/%d/passkeys/%s", alice, strings.Replace(action, "{cred}", k.cred(), 1)), "nonsense", aliceSession), http.StatusBadRequest, action)
		})
	}
	t.Run("a garbled body to delete", func(t *testing.T) {
		wantStatus(t, g.do("DELETE", fmt.Sprintf("/users/%d/passkeys/%s", alice, k.cred()), "nonsense", aliceSession), http.StatusBadRequest, "delete")
	})
	t.Run("another user cannot rename a passkey", func(t *testing.T) {
		wantStatus(t, g.do("POST", fmt.Sprintf("/users/%d/passkeys/%s/rename", bob.ID, k.cred()), map[string]string{"Name": "mine now"}, bobSession), http.StatusNotFound, "rename")
		keys, _ := g.st.ListPasskeys(context.Background(), alice)
		if len(keys) != 1 || keys[0].Name != "key" {
			t.Fatalf("alice's passkey changed: %+v", keys)
		}
	})
	t.Run("another user cannot delete a passkey", func(t *testing.T) {
		wantStatus(t, g.do("DELETE", fmt.Sprintf("/users/%d/passkeys/%s", bob.ID, k.cred()), map[string]string{"Current": testPassword}, bobSession), http.StatusNotFound, "delete")
		if keys, _ := g.st.ListPasskeys(context.Background(), alice); len(keys) != 1 {
			t.Fatal("alice lost her passkey")
		}
	})
	t.Run("a name that is too long", func(t *testing.T) {
		wantStatus(t, g.do("POST", fmt.Sprintf("/users/%d/passkeys/%s/rename", alice, k.cred()), map[string]string{"Name": strings.Repeat("n", maxPasskeyName+1)}, aliceSession), http.StatusUnprocessableEntity, "rename")
	})
	t.Run("another user's path is not found", func(t *testing.T) {
		wantStatus(t, g.do("POST", fmt.Sprintf("/users/%d/passkeys/begin", alice), map[string]string{}, bobSession), http.StatusNotFound, "begin")
	})
}

func TestARegistrationIsRefusedWhenItShouldBe(t *testing.T) {
	g := newRig(t)
	uid, session := g.signUp("alice")
	body := map[string]string{"Name": "key", "Current": testPassword}

	t.Run("from an address that is not a host name over HTTPS", func(t *testing.T) {
		rep := g.do("POST", fmt.Sprintf("/users/%d/passkeys/begin", uid), body, session)
		wantStatus(t, rep, http.StatusUnprocessableEntity, "http on an address")
		if !strings.Contains(rep.body(), "HTTPS") {
			t.Fatal(rep.body())
		}
	})
	t.Run("without the password", func(t *testing.T) {
		_, _, rep := g.beginRegistration(uid, session, map[string]string{"Name": "key", "Current": "wrong"})
		wantStatus(t, rep, http.StatusUnprocessableEntity, "wrong password")
	})
	t.Run("with a name that is too long", func(t *testing.T) {
		_, _, rep := g.beginRegistration(uid, session, map[string]string{"Name": strings.Repeat("é", maxPasskeyName+1), "Current": testPassword})
		wantStatus(t, rep, http.StatusUnprocessableEntity, "long name")
	})
	t.Run("a name left empty becomes Passkey", func(t *testing.T) {
		opts, ceremony, rep := g.beginRegistration(uid, session, map[string]string{"Current": testPassword})
		wantStatus(t, rep, http.StatusOK, "empty name")
		g.finishRegistration(uid, g.attestation(opts, pkNewKey(t)), session, ceremony)
		keys, _ := g.st.ListPasskeys(context.Background(), uid)
		if len(keys) != 1 || keys[0].Name != defaultPasskeyName {
			t.Fatalf("passkeys %+v", keys)
		}
	})
}

func TestTheRegistrationCeremonyBelongsToOneUserAndWorksOnce(t *testing.T) {
	g := newRig(t)
	alice, aliceSession := g.signUp("alice")
	bob, bobSession := g.signUp("bob")
	k := pkNewKey(t)
	opts, ceremony, _ := g.beginRegistration(alice, aliceSession, map[string]string{"Name": "key", "Current": testPassword})
	payload := g.attestation(opts, k)

	t.Run("without the ceremony cookie", func(t *testing.T) {
		wantStatus(t, g.finishRegistration(alice, payload, aliceSession), http.StatusBadRequest, "no cookie")
	})
	t.Run("with a ceremony cookie made for another user", func(t *testing.T) {
		rep := g.finishRegistration(bob, payload, bobSession, ceremony)
		wantStatus(t, rep, http.StatusBadRequest, "alice's cookie used by bob")
		if keys, _ := g.st.ListPasskeys(context.Background(), bob); len(keys) != 0 {
			t.Fatal("bob got a passkey from alice's ceremony")
		}
	})
	t.Run("with an answer the authenticator did not give", func(t *testing.T) {
		rep := g.finishRegistration(alice, `{"id":"x"}`, aliceSession, ceremony)
		wantStatus(t, rep, http.StatusUnprocessableEntity, "garbage answer")
	})
	t.Run("with a challenge from another ceremony", func(t *testing.T) {
		other, _, _ := g.beginRegistration(alice, aliceSession, map[string]string{"Name": "key", "Current": testPassword})
		wantStatus(t, g.finishRegistration(alice, g.attestation(other, k), aliceSession, ceremony), http.StatusUnprocessableEntity, "wrong challenge")
	})
	t.Run("a second time with the same cookie", func(t *testing.T) {
		wantStatus(t, g.finishRegistration(alice, payload, aliceSession, ceremony), http.StatusOK, "first")
		rep := g.finishRegistration(alice, payload, aliceSession, ceremony)
		wantStatus(t, rep, http.StatusBadRequest, "replay")
		if !strings.Contains(rep.body(), "expired") {
			t.Fatal(rep.body())
		}
		// two requests that arrive together both pass the first look; the single-use check tells them apart
		racing := newRigOn(t, blindToNonces{g.st}, g.st)
		rep = racing.finishRegistration(alice, payload, aliceSession, ceremony)
		wantStatus(t, rep, http.StatusBadRequest, "race")
		if !strings.Contains(rep.body(), "already used") {
			t.Fatal(rep.body())
		}
	})
	t.Run("a passkey that another account already holds", func(t *testing.T) {
		opts, ceremony, _ := g.beginRegistration(bob, bobSession, map[string]string{"Name": "key", "Current": testPassword})
		rep := g.finishRegistration(bob, g.attestation(opts, k), bobSession, ceremony)
		wantStatus(t, rep, http.StatusUnprocessableEntity, "same credential")
		if !strings.Contains(rep.body(), "already registered") {
			t.Fatal(rep.body())
		}
	})
}

func TestARegistrationFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	body := map[string]string{"Name": "key", "Current": testPassword}
	t.Run("begin: the password cannot be checked", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "user_identities")
		logs := captureSignInLog(t)
		_, _, rep := g.beginRegistration(uid, session, body)
		if rep.Code != 500 || !strings.Contains(logs.String(), "passkey re-authentication") {
			t.Fatalf("%d %s", rep.Code, logs)
		}
	})
	t.Run("begin: the user handle cannot be made", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		rejectWritesOn(t, g.st, "users", "UPDATE")
		_, _, rep := g.beginRegistration(uid, session, body)
		wantStatus(t, rep, 500, "handle")
	})
	t.Run("begin: the passkeys cannot be listed", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		breakTable(t, g.st, "passkeys")
		_, _, rep := g.beginRegistration(uid, session, body)
		wantStatus(t, rep, 500, "passkeys")
	})
	t.Run("begin: the library refuses the user handle", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		tooLong := strings.Repeat("A", 100) // 75 bytes, past the 64 the specification allows
		if err := g.st.ExecForTests(context.Background(), "UPDATE users SET passkey_handle = ? WHERE id = ?", tooLong, uid); err != nil {
			t.Fatal(err)
		}
		logs := captureSignInLog(t)
		_, _, rep := g.beginRegistration(uid, session, body)
		if rep.Code != 500 || !strings.Contains(logs.String(), "passkey registration could not start") {
			t.Fatalf("%d %s", rep.Code, logs)
		}
	})
	t.Run("finish: each step the ceremony needs", func(t *testing.T) {
		for _, sc := range []struct {
			name  string
			setup func(*testing.T, *rig) *rig
		}{
			{"the passkeys cannot be listed", func(t *testing.T, g *rig) *rig { breakTable(t, g.st, "passkeys"); return g }},
			{"the passkey cannot be saved", func(t *testing.T, g *rig) *rig { rejectWritesOn(t, g.st, "passkeys", "INSERT"); return g }},
		} {
			t.Run(sc.name, func(t *testing.T) {
				g := newRig(t)
				uid, session := g.signUp("alice")
				opts, ceremony, _ := g.beginRegistration(uid, session, body)
				payload := g.attestation(opts, pkNewKey(t))
				g = sc.setup(t, g)
				wantStatus(t, g.finishRegistration(uid, payload, session, ceremony), 500, sc.name)
			})
		}
		t.Run("the user handle cannot be read", func(t *testing.T) {
			g := newRig(t)
			uid, session := g.signUp("alice")
			opts, ceremony, _ := g.beginRegistration(uid, session, body)
			payload := g.attestation(opts, pkNewKey(t))
			w := newRigOn(t, handleFails{g.st}, g.st)
			// the same database and cookie key, so the session and the ceremony cookie carry over
			wantStatus(t, w.finishRegistration(uid, payload, session, ceremony), 500, "handle")
		})
	})
}

func TestPasskeyCeremoniesFailCleanlyOnAHostThatTheLibraryCannotUse(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	u, _ := g.st.GetUser(context.Background(), uid)
	// "localhost" with a port that is not a number still passes for localhost, but is no valid URL
	newReq := func() *http.Request {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"Current":"`+testPassword+`"}`))
		req.Host = "localhost:80%"
		return req
	}
	if _, err := g.b.relyingParty(newReq()); err == nil {
		t.Fatal("a host that is not a valid URL gave a relying party")
	}
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"begin registration":  func(w http.ResponseWriter, r *http.Request) { g.b.internalPasskeyRegisterBegin(w, r, u) },
		"finish registration": func(w http.ResponseWriter, r *http.Request) { g.b.internalPasskeyRegisterFinish(w, r, u) },
		"begin sign-in":       g.b.internalPasskeyLoginBegin,
		"finish sign-in":      g.b.internalPasskeyLoginFinish,
	} {
		t.Run(name, func(t *testing.T) {
			r := newReq()
			switch name {
			case "finish registration":
				sealed, _, _ := g.b.sealValue(sealPasskeyRegister, passkeyCeremony{UID: uid}, passkeyTTL)
				r.AddCookie(&http.Cookie{Name: passkeyCookie, Value: sealed})
			case "finish sign-in":
				sealed, _, _ := g.b.sealValue(sealPasskeyLogin, passkeyCeremony{}, passkeyTTL)
				r.AddCookie(&http.Cookie{Name: passkeyCookie, Value: sealed})
			}
			rec := httptest.NewRecorder()
			call(rec, r)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestPasskeysAreOnlyOfferedWhereTheBrowserCanUseThem(t *testing.T) {
	for _, tc := range []struct {
		host   string
		secure bool
		want   bool
	}{
		{"localhost", false, true},
		{"dexa.example.com", false, false},
		{"dexa.example.com", true, true},
		{"192.0.2.1", true, false},
		{"localhost:80%", false, true},
		{"a b:80", false, false},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = tc.host
		if tc.secure {
			req = req.WithContext(edge.WithSecure(req.Context(), true))
		}
		if got := passkeysReady(req); got != tc.want {
			t.Errorf("host %q secure %v: ready %v, want %v", tc.host, tc.secure, got, tc.want)
		}
	}
}

func TestAPasskeyUserIsNamedAfterTheAddressOrTheName(t *testing.T) {
	if got := (&passkeyUser{user: models.User{Name: "Pat", Email: "pat@example.com"}}).WebAuthnName(); got != "pat@example.com" {
		t.Errorf("with an address: %q", got)
	}
	if got := (&passkeyUser{user: models.User{Name: "Pat"}}).WebAuthnName(); got != "Pat" {
		t.Errorf("without an address: %q", got)
	}
}

func TestStoredPasskeysAreReadCarefully(t *testing.T) {
	g := newRig(t)
	uid, session := g.signUp("alice")
	k := pkNewKey(t)
	g.addPasskey(uid, session, k)
	ctx := context.Background()
	u, _ := g.st.GetUser(ctx, uid)
	handle, err := g.st.EnsurePasskeyHandle(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.b.passkeyUserFor(ctx, u, "!!not base64!!"); err == nil {
		t.Error("a handle that is not base64 was accepted")
	}
	if err := g.st.ExecForTests(ctx, "UPDATE passkeys SET credential = 'not json'"); err != nil {
		t.Fatal(err)
	}
	logs := captureSignInLog(t)
	pu, err := g.b.passkeyUserFor(ctx, u, handle)
	if err != nil || len(pu.creds) != 0 || !strings.Contains(logs.String(), "skipping an unreadable passkey") {
		t.Fatalf("creds %d, err %v, log %s", len(pu.creds), err, logs)
	}
	breakTable(t, g.st, "passkeys")
	if _, err := g.b.passkeyUserFor(ctx, u, handle); err == nil {
		t.Error("a passkey list that cannot be read gave a user")
	}
	if _, err := g.b.passkeysFor(httptest.NewRequest("GET", "/", nil), u); err == nil {
		t.Error("the settings section was built without the passkeys")
	}
}

func TestPasskeyFailuresCarryTheirCodes(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusUnauthorized:        apiclient.CodeUnauthorized,
		http.StatusTooManyRequests:     apiclient.CodeThrottled,
		http.StatusInternalServerError: apiclient.CodeInternal,
		http.StatusBadRequest:          apiclient.CodeInvalid,
	} {
		rec := httptest.NewRecorder()
		passkeyFail(rec, status, "x")
		var e apiclient.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != want || rec.Code != status {
			t.Errorf("status %d: %+v (%v), want code %s", status, e, err, want)
		}
	}
}

func TestDeletingAPasskeyFailsCleanlyWhenTheStoreLetsItDown(t *testing.T) {
	remove := func(g *rig, uid int64, session *http.Cookie, k pkKey) reply {
		return g.do("DELETE", fmt.Sprintf("/users/%d/passkeys/%s", uid, k.cred()), map[string]string{"Current": testPassword}, session)
	}
	t.Run("the passkeys cannot be listed", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		k := pkNewKey(t)
		g.addPasskey(uid, session, k)
		breakTable(t, g.st, "passkeys")
		wantStatus(t, remove(g, uid, session, k), 500, "list")
	})
	t.Run("the ways to sign in cannot be listed", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		k := pkNewKey(t)
		g.addPasskey(uid, session, k)
		w := newRigOn(t, providersFail{g.st}, g.st)
		wantStatus(t, remove(w, uid, session, k), 500, "providers")
	})
	t.Run("the passkey cannot be removed", func(t *testing.T) {
		g := newRig(t)
		uid, session := g.signUp("alice")
		k := pkNewKey(t)
		g.addPasskey(uid, session, k)
		rejectWritesOn(t, g.st, "passkeys", "DELETE")
		wantStatus(t, remove(g, uid, session, k), 500, "delete")
	})
}

func TestPasskeySignInRefusesWhatIsNotTheRegisteredKeyAndWorksOnce(t *testing.T) {
	g := newRig(t)
	uid, session := g.signUp("alice")
	k := pkNewKey(t)
	g.addPasskey(uid, session, k)
	handle, _ := g.st.EnsurePasskeyHandle(context.Background(), uid)

	t.Run("a user handle nobody has", func(t *testing.T) {
		challenge, ceremony := g.beginLogin()
		a := pkGood(pkLocalSite, k, challenge, pkB64([]byte("nobody")), 1)
		rep := g.finishLogin(pkAssertionJSON(t, a), ceremony)
		wantStatus(t, rep, http.StatusUnauthorized, "unknown handle")
		if rep.cookie(sessionCookie) != nil || !g.audited(auditPasskeyFailed) {
			t.Fatal("no refusal was audited or a session was set")
		}
	})
	t.Run("the same ceremony a second time", func(t *testing.T) {
		challenge, ceremony := g.beginLogin()
		first := g.finishLogin(pkAssertionJSON(t, pkGood(pkLocalSite, k, challenge, handle, 1)), ceremony)
		if first.cookie(sessionCookie) == nil {
			t.Fatalf("the first sign-in failed: %d %s", first.Code, first.body())
		}
		second := g.finishLogin(pkAssertionJSON(t, pkGood(pkLocalSite, k, challenge, handle, 2)), ceremony)
		wantStatus(t, second, http.StatusBadRequest, "replay")
		if second.cookie(sessionCookie) != nil || !strings.Contains(second.body(), "expired") {
			t.Fatalf("replay: %s", second.body())
		}
		racing := newRigOn(t, blindToNonces{g.st}, g.st)
		third := racing.finishLogin(pkAssertionJSON(t, pkGood(pkLocalSite, k, challenge, handle, 3)), ceremony)
		wantStatus(t, third, http.StatusBadRequest, "race")
		if third.cookie(sessionCookie) != nil || !strings.Contains(third.body(), "already used") {
			t.Fatalf("race: %s", third.body())
		}
	})
	t.Run("the use of the passkey cannot be recorded, but the sign-in goes on", func(t *testing.T) {
		challenge, ceremony := g.beginLogin()
		rejectWritesOn(t, g.st, "passkeys", "UPDATE")
		logs := captureSignInLog(t)
		rep := g.finishLogin(pkAssertionJSON(t, pkGood(pkLocalSite, k, challenge, handle, 5)), ceremony)
		if rep.cookie(sessionCookie) == nil || !strings.Contains(logs.String(), "could not record the passkey use") {
			t.Fatalf("%d %s, log %s", rep.Code, rep.body(), logs)
		}
	})
}
