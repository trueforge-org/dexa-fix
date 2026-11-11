package apiclient

import (
	"context"
	"net/http"
	"testing"
)

func TestAnErrorBodyThatSaysStatusZeroKeepsTheRealStatus(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"status":0,"code":"not_found","message":"No such thing."}`))
	})
	err := c.Get(context.Background(), "/x", nil)
	if StatusOf(err) != http.StatusNotFound || !IsCode(err, CodeNotFound) || err.Error() != "No such thing." {
		t.Fatalf("got %v (status %d)", err, StatusOf(err))
	}
}
