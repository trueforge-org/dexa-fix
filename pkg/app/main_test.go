package app

import (
	"os"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

// The tests read better with the user ids 1, 2, 3. Random ids are tested in sqlstore and in
// TestRandomUserIDsFlow.
func TestMain(m *testing.M) {
	sqlstore.SequentialUserIDsForTests()
	os.Exit(m.Run())
}
