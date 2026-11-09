package web

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func TestAdminEndpointsOverTheSplit(t *testing.T) {
	bs, srv, rootSession := newInternalServer(t)
	api := splitClient(t, bs)
	ctx := context.Background()
	visitor := func(cookie string) context.Context {
		return apiclient.WithRequest(ctx, apiclient.Request{Cookie: cookie, ClientIP: "203.0.113.7", Host: "dexa.example.com", Secure: true})
	}

	// A signed-in user who is not an administrator is refused.
	bob, err := srv.store.CreateUserWithIdentity(ctx, "bob", "bob@example.com", models.Identity{Provider: "idp", Subject: "bob-1"})
	if err != nil {
		t.Fatal(err)
	}
	token := "bob-session-token-0123456789abcdef"
	if err := srv.store.CreateSession(ctx, hashToken(token), bob.ID, "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = api.AdminSetRole(visitor(sessionCookie+"="+token), bob.ID, models.RoleAdmin)
	if !apiclient.IsCode(err, apiclient.CodeForbidden) {
		t.Fatalf("non-admin: %v", err)
	}
	if _, err := api.AdminPage(visitor(sessionCookie+"="+token), ""); !apiclient.IsCode(err, apiclient.CodeForbidden) {
		t.Fatalf("non-admin page: %v", err)
	}
	if got, _ := srv.store.GetUser(ctx, bob.ID); got.IsAdmin() {
		t.Fatal("a non-admin made themselves an administrator")
	}
	// Nobody signed in at all.
	if _, err := api.AdminSetRole(visitor(""), bob.ID, models.RoleAdmin); !apiclient.IsCode(err, apiclient.CodeUnauthorized) {
		t.Fatalf("anonymous: %v", err)
	}

	// An administrator with two-step verification on needs the step-up proof.
	tfEnable(t, srv, 1)
	rootCookie := sessionCookie + "=" + rootSession.Value
	_, err = api.AdminSetRole(visitor(rootCookie), bob.ID, models.RoleAdmin)
	if !apiclient.IsCode(err, apiclient.CodeStepUp) {
		t.Fatalf("admin without step-up: %v", err)
	}
	if got, _ := srv.store.GetUser(ctx, bob.ID); got.IsAdmin() {
		t.Fatal("a role changed without the step-up proof")
	}

	// With the proof, saving the base URL answers config_changed, and a frontend sees it after RefreshConfig.
	proof := adminProof(t, srv, sealAdminStep, 1, time.Hour)
	withProof := visitor(rootCookie + "; " + proof.Name + "=" + proof.Value)
	f, err := NewFrontend(api)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	if err := f.RefreshConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cfg().baseURL != nil {
		t.Fatal("base URL set before the change")
	}
	res, err := api.AdminSaveServer(withProof, apiclient.ServerForm{BaseURL: "https://dexa.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.ConfigChanged || res.Message != "Server settings saved. They apply now." {
		t.Fatalf("result %+v", res)
	}
	if f.cfg().baseURL != nil {
		t.Fatal("the frontend changed before it asked")
	}
	f.afterAdminChange(httptest.NewRequest("POST", "/admin/server", nil), res)
	if l := f.cfg(); l.baseURL == nil || l.baseURL.String() != "https://dexa.example.com" {
		t.Fatalf("frontend settings after RefreshConfig: %+v", l)
	}

	// A base URL for another host is refused: it would lock the administrator out.
	res, err = api.AdminSaveServer(withProof, apiclient.ServerForm{BaseURL: "https://other.example.org"})
	if err != nil || res.ConfigChanged || res.Message == "" {
		t.Fatalf("lockout check: %+v %v", res, err)
	}

	// The page data carries nothing of the frontend's.
	page, err := api.AdminPage(withProof, "")
	if err != nil || len(page.Users) != 2 || page.Server.BaseURL != "https://dexa.example.com" {
		t.Fatalf("page: %+v %v", page, err)
	}
}
