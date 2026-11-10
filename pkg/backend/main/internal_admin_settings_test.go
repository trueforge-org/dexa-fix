package mainbackend

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
)

func failing(calls ...string) func(*testing.T, *adminEnv) {
	return func(t *testing.T, e *adminEnv) { e.flaky.failOn(calls...) }
}

func brokenTable(table string) func(*testing.T, *adminEnv) {
	return func(t *testing.T, e *adminEnv) { breakTable(t, e.st, table) }
}

// appleKey is a .p8 key as Apple hands it out.
func appleKey(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestAdminSignInPolicyChangesTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	linkRoot := func(t *testing.T, e *adminEnv) {
		t.Helper()
		if err := e.st.PutIdentity(context.Background(), models.Identity{UserID: 1, Provider: "idp", Subject: "root-1"}); err != nil {
			t.Fatal(err)
		}
	}
	passwordsOff := func(t *testing.T, e *adminEnv) {
		t.Helper()
		if err := e.st.PutSetting(context.Background(), settingPasswordOn, valueDisabled); err != nil {
			t.Fatal(err)
		}
	}
	runAdminChanges(t, []adminChange{
		{name: "turning passwords on when the setting cannot be removed", setup: failing("DeleteSetting:" + settingPasswordOn), path: "/admin/password-login", form: url.Values{"enabled": {"1"}}},
		{name: "turning passwords off when the identities cannot be listed", setup: brokenTable("passkeys"), path: "/admin/password-login", form: url.Values{"enabled": {"0"}}},
		{name: "turning passwords off when the setting cannot be saved", setup: func(t *testing.T, e *adminEnv) { linkRoot(t, e); e.flaky.failOn("PutSetting:" + settingPasswordOn) }, path: "/admin/password-login", form: url.Values{"enabled": {"0"}}},
		{name: "requiring two-step verification of nobody", path: "/admin/two-factor", form: url.Values{"require": {"off"}}, wantFlash: "optional"},
		{name: "requiring two-step verification of a made-up group", path: "/admin/two-factor", form: url.Values{"require": {"martians"}}, wantFlash: "Choose who"},
		{name: "requiring two-step verification when the setting cannot be saved", setup: failing("PutSetting:" + settingRequire2FA), path: "/admin/two-factor", form: url.Values{"require": {"admins"}}},
		{name: "a reset link while passwords are off", setup: passwordsOff, path: "/admin/users/2/reset-link", form: url.Values{}, wantFlash: "nothing to reset"},
		{name: "a reset link when the links table is gone", setup: brokenTable("password_resets"), path: "/admin/users/2/reset-link", form: url.Values{}},
	})
}

func TestAdminProviderChangesTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	oauth := url.Values{
		"kind": {"oauth2"}, "slug": {"gh"}, "label": {"GitHub"}, "client_id": {"cid"}, "client_secret": {"s3cret"},
		"auth_url": {"https://idp.example/auth"}, "token_url": {"https://idp.example/token"}, "userinfo_url": {"https://idp.example/me"},
	}
	apple := url.Values{"kind": {"apple"}, "slug": {"apple"}, "label": {"Apple"}, "client_id": {"cid"}, "apple_team_id": {"T"}, "apple_key_id": {"K"}}
	withKey := func(t *testing.T) url.Values { return formWith(apple, "client_secret", appleKey(t)) }
	storedProvider := func(t *testing.T, e *adminEnv) {
		t.Helper()
		p := models.AuthProvider{Slug: "gh", Label: "GitHub", Kind: models.ProviderKindOAuth2, ClientID: "c", ClientSecret: "s",
			AuthURL: "https://idp.example/auth", TokenURL: "https://idp.example/token", UserinfoURL: "https://idp.example/me", SubjectClaim: "sub"}
		if err := e.st.PutAuthProvider(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	runAdminChanges(t, []adminChange{
		{name: "a provider without a display name", path: "/admin/providers", form: formWith(oauth, "label", ""), wantFlash: "display name and client ID"},
		{name: "a provider when the providers table is gone", setup: brokenTable("auth_providers"), path: "/admin/providers", form: oauth},
		{name: "an OpenID provider without a secret", path: "/admin/providers", form: url.Values{"kind": {"oidc"}, "slug": {"g"}, "label": {"G"}, "client_id": {"c"}, "issuer": {"https://idp.example"}}, wantFlash: "client secret is required"},
		{name: "an OpenID provider whose issuer does not answer", path: "/admin/providers", form: url.Values{"kind": {"oidc"}, "slug": {"g"}, "label": {"G"}, "client_id": {"c"}, "client_secret": {"s"}, "issuer": {"http://127.0.0.1:1"}}, wantFlash: "Could not read the OpenID configuration"},
		{name: "an Apple provider without a team", path: "/admin/providers", form: formWith(apple, "apple_team_id", ""), wantFlash: "team ID and key ID"},
		{name: "an Apple provider with a key that is not one", path: "/admin/providers", form: formWith(apple, "client_secret", "not a key"), wantFlash: ".p8 key"},
		{name: "an Apple provider with a good key", path: "/admin/providers", form: withKey(t), wantFlash: "Apple saved"},
		{name: "an OAuth provider without https URLs", path: "/admin/providers", form: formWith(oauth, "token_url", "ftp://x"), wantFlash: "https authorisation"},
		{name: "an OAuth provider with a URL that is not a URL", path: "/admin/providers", form: formWith(oauth, "auth_url", "https://idp.example/%zz"), wantFlash: "https authorisation"},
		{name: "an OAuth provider without a secret", path: "/admin/providers", form: formWith(oauth, "client_secret", ""), wantFlash: "client secret is required"},
		{name: "an OAuth provider", path: "/admin/providers", form: oauth, wantFlash: "GitHub saved"},
		{name: "a provider of an unknown type", path: "/admin/providers", form: formWith(oauth, "kind", "saml"), wantFlash: "Unknown provider type"},
		{name: "a provider the database refuses", setup: func(t *testing.T, e *adminEnv) { refuseWrites(t, e.st, "auth_providers", "slug", "gh") }, path: "/admin/providers", form: oauth},
		{name: "removing a provider the database refuses to remove", setup: func(t *testing.T, e *adminEnv) {
			storedProvider(t, e)
			refuseWrites(t, e.st, "auth_providers", "slug", "gh")
		}, path: "/admin/providers/gh/delete", form: url.Values{}},
		{name: "removing a provider when the identities cannot be listed", setup: func(t *testing.T, e *adminEnv) {
			storedProvider(t, e)
			if err := e.st.PutSetting(context.Background(), settingPasswordOn, valueDisabled); err != nil {
				t.Fatal(err)
			}
			breakTable(t, e.st, "passkeys")
		}, path: "/admin/providers/gh/delete", form: url.Values{}},
		{name: "removing a provider that is not there", path: "/admin/providers/gh/delete", form: url.Values{}, wantStatus: 404},
	})
}

func TestAdminWithingsChangesTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	runAdminChanges(t, []adminChange{
		{name: "a redirect URL that is not https", path: "/admin/withings", form: url.Values{"client_id": {"c"}, "redirect_url": {"ftp://x"}}, wantFlash: "must use https"},
		{name: "forgetting the secret", path: "/admin/withings", form: url.Values{"client_id": {"c"}, "clear_secret": {"1"}}, wantFlash: "Withings settings saved"},
		{name: "saving when the client ID cannot be stored", setup: failing("PutSetting:" + settingWithingsClientID), path: "/admin/withings", form: url.Values{"client_id": {"c"}}},
		{name: "saving a secret that cannot be stored", setup: failing("PutSetting:" + settingWithingsSecret), path: "/admin/withings", form: url.Values{"client_id": {"c"}, "client_secret": {"s"}}},
		{name: "forgetting a secret that cannot be removed", setup: failing("DeleteSetting:" + settingWithingsSecret), path: "/admin/withings", form: url.Values{"client_id": {"c"}, "clear_secret": {"1"}}},
	})
}

func TestAdminServerSettingsTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	base := url.Values{"base_url": {""}, "trusted_proxies": {"loopback"}, "audit_retention_days": {"30"}, "key_reminder_days": {"90"}}
	runAdminChanges(t, []adminChange{
		{name: "a base URL that is not a URL", path: "/admin/server", form: formWith(base, "base_url", "ftp://x"), wantFlash: "URL"},
		{name: "trusted proxies that are not addresses", path: "/admin/server", form: formWith(base, "trusted_proxies", "banana"), wantFlash: "banana"},
		{name: "the PROXY protocol without trusted proxies", path: "/admin/server", form: formWith(formWith(base, "trusted_proxies", ""), "proxy_protocol", "1"), wantFlash: "PROXY protocol needs"},
		{name: "an audit retention that is not a number", path: "/admin/server", form: formWith(base, "audit_retention_days", "many"), wantFlash: "Audit log retention"},
		{name: "a key reminder that is not a number", path: "/admin/server", form: formWith(base, "key_reminder_days", "many"), wantFlash: "key reminder"},
		{name: "settings when one cannot be stored", setup: failing("PutSetting:" + settingTrustedProxies), path: "/admin/server", form: base},
		{name: "settings that cannot be applied", setup: failing("SettingsGeneration"), path: "/admin/server", form: base},
		{name: "settings saved", path: "/admin/server", form: base, wantFlash: "They apply now"},
	})
}
