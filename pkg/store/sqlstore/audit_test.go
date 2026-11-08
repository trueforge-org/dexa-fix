package sqlstore

import (
	"context"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

func TestAuditLog(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	admin, _ := s.CreateUser(ctx, "admin")
	alice, _ := s.CreateUser(ctx, "alice")
	bob, _ := s.CreateUser(ctx, "bob")

	add := func(actor, target *int64, action string) models.AuditEvent {
		t.Helper()
		e := models.AuditEvent{ActorID: actor, TargetID: target, Action: action, IP: "192.0.2.1"}
		if err := s.AddAuditEvent(ctx, &e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	a, b, ad := alice.ID, bob.ID, admin.ID
	add(&a, &a, "login")
	add(&ad, &a, "password_set_by_admin") // concerns alice, done by the admin
	add(&b, &b, "login")

	if err := s.AddAuditEvent(ctx, &models.AuditEvent{}); err != store.ErrInvalid {
		t.Errorf("event without action: %v", err)
	}

	got, err := s.ListAuditEvents(ctx, store.AuditFilter{UserID: alice.ID})
	if err != nil || len(got) != 2 || got[0].Action != "password_set_by_admin" {
		t.Fatalf("alice's events (newest first): %v %v", got, err)
	}
	if all, _ := s.ListAuditEvents(ctx, store.AuditFilter{}); len(all) != 3 {
		t.Errorf("all events: %d", len(all))
	}
	if page, _ := s.ListAuditEvents(ctx, store.AuditFilter{BeforeID: got[0].ID, Limit: 1}); len(page) != 1 || page[0].ID >= got[0].ID {
		t.Errorf("paging: %v", page)
	}

	// Deleting a user clears the IP addresses of their events, but keeps the events.
	if err := s.DeleteUser(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListAuditEvents(ctx, store.AuditFilter{})
	if len(all) != 3 {
		t.Fatalf("events after deleting a user: %d", len(all))
	}
	for _, e := range all {
		touchedAlice := e.Action == "password_set_by_admin" || (e.Action == "login" && e.ActorID == nil && e.TargetID == nil)
		if touchedAlice && e.IP != "" {
			t.Errorf("IP of a deleted user's event kept: %+v", e)
		}
		if !touchedAlice && e.IP == "" {
			t.Errorf("unrelated event lost its IP: %+v", e)
		}
		if e.TargetID != nil && *e.TargetID == alice.ID {
			t.Errorf("event still refers to the deleted user: %+v", e)
		}
	}

	// A refused delete (the last administrator) must not clear anything.
	own := add(&ad, &ad, "profile_updated")
	if err := s.DeleteUser(ctx, admin.ID); err != store.ErrLastAdmin {
		t.Fatalf("deleting the only admin: %v", err)
	}
	ev, _ := s.ListAuditEvents(ctx, store.AuditFilter{UserID: admin.ID})
	if len(ev) == 0 || ev[0].ID != own.ID || ev[0].IP == "" || ev[0].TargetID == nil {
		t.Errorf("refused delete changed the log: %v", ev)
	}

	n, err := s.PurgeAuditEvents(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 4 {
		t.Errorf("purge: %d %v", n, err)
	}
}
