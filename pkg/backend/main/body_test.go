package mainbackend

import (
	"sync"

	bodybackend "github.com/trueforge-org/dexa-fix/pkg/backend/body"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// A backend needs a body service. The tests run one in the same process, over the same store, the
// way the combined `serve` does (bodybackend.NewInProcess): every call goes through the real body
// handler, headers and tokens. (Test files may import the body package; the backend itself may not.)

var testBodies sync.Map // *Backend -> *bodybackend.Service

// testBody returns the option that gives a backend its own in-process body service over st.
func testBody(st store.Store) Option {
	c, svc, err := bodybackend.NewInProcess(st)
	if err != nil {
		panic(err)
	}
	return func(b *Backend) {
		WithBody(c)(b)
		testBodies.Store(b, svc)
	}
}

// testBodyOf returns the body service a backend of the tests calls.
func testBodyOf(b *Backend) *bodybackend.Service {
	svc, _ := testBodies.Load(b)
	return svc.(*bodybackend.Service)
}
