package mainbackend

import (
	"os"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

// The tests read better with the user ids 1, 2, 3. Random ids are tested in sqlstore and in
// TestRandomUserIDsFlow.
func TestMain(m *testing.M) {
	sqlstore.SequentialUserIDsForTests()
	// The logger is set once and never replaced: a goroutine of an earlier test may still log while
	// the next test captures the log, so a test swaps the writer behind it (captureLog) instead.
	log.Logger = zerolog.New(logSink).With().Timestamp().Logger()
	os.Exit(m.Run())
}
