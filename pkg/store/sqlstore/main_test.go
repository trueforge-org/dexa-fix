package sqlstore

import (
	"os"
	"testing"
)

// Most tests read better with the user ids 1, 2, 3. userid_test.go turns random ids back on.
func TestMain(m *testing.M) {
	SequentialUserIDsForTests()
	os.Exit(m.Run())
}

// randomUserIDs makes new users get random ids for the rest of the test.
func randomUserIDs(t *testing.T) {
	t.Helper()
	sequentialUserIDs.Store(false)
	t.Cleanup(func() { sequentialUserIDs.Store(true) })
}
