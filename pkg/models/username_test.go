package models

import (
	"strings"
	"testing"
)

func TestValidUsername(t *testing.T) {
	for s, want := range map[string]bool{
		"al": true, "Alice": true, "a.b_c-d": true, "9lives": true, strings.Repeat("x", 32): true,
		"": false, "a": false, "_a": false, ".a": false, "-a": false, "a b": false, "a@b": false,
		"ää": false, strings.Repeat("x", 33): false,
	} {
		if got := ValidUsername(s); got != want {
			t.Errorf("ValidUsername(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestCleanPersonName(t *testing.T) {
	for _, c := range []struct {
		in, out string
		ok      bool
	}{
		{"  Ana  ", "Ana", true},
		{"", "", true},
		{"Zoë", "Zoë", true},
		{strings.Repeat("ä", 64), strings.Repeat("ä", 64), true},
		{strings.Repeat("ä", 65), "", false},
		{"a\nb", "", false},
	} {
		if out, ok := CleanPersonName(c.in); out != c.out || ok != c.ok {
			t.Errorf("CleanPersonName(%q) = %q, %v", c.in, out, ok)
		}
	}
}

func TestDisplayAndFullName(t *testing.T) {
	u := User{Username: "ana99"}
	if u.DisplayName() != "ana99" || u.FullName() != "ana99" {
		t.Fatalf("without names: %q %q", u.DisplayName(), u.FullName())
	}
	u.FirstName = "Ana"
	if u.DisplayName() != "Ana" || u.FullName() != "Ana" {
		t.Fatalf("first name only: %q %q", u.DisplayName(), u.FullName())
	}
	u.LastName = "Silva"
	if u.DisplayName() != "Ana" || u.FullName() != "Ana Silva" {
		t.Fatalf("both: %q %q", u.DisplayName(), u.FullName())
	}
	u.FirstName = ""
	if u.DisplayName() != "ana99" || u.FullName() != "Silva" {
		t.Fatalf("last name only: %q %q", u.DisplayName(), u.FullName())
	}
}
