package apiclient

import "context"

// The /status page: the health of each part of a deployment. Anybody may see the summary (a state
// per component, nothing else: no versions, host names, worker names or error texts), so it is safe
// to show to visitors and to monitors. An administrator also gets the detail.

// Component states.
const (
	StateUp       = "up"       // works
	StateDegraded = "degraded" // works, but something is wrong; no component reports it yet, the page and Overall handle it
	StateDown     = "down"     // does not work
	StateOff      = "off"      // not in use (for example no worker has ever registered): not a fault
)

// StatusComponent is one part of the deployment.
type StatusComponent struct {
	Name  string // a stable key: web, api, database, body, valkey, workers.<provider>
	Label string
	State string
}

// Status is the summary everybody may see.
type Status struct {
	CheckedAt  string // UTC, when the backend last checked (it keeps an answer for a few seconds)
	Components []StatusComponent
}

// StatusWorker is a worker process, for administrators.
type StatusWorker struct {
	ID       string
	Version  string
	Kinds    []string
	LastSeen string
	Live     bool // asked for work recently enough to count as running
}

// StatusDetail is the summary plus what only administrators see.
type StatusDetail struct {
	Status
	Notes   map[string]string // component name to the reason it is not up
	Workers []StatusWorker
}

// Overall is the worst state among the components, ignoring those that are off: StateDown,
// StateDegraded or StateUp.
func (s Status) Overall() string {
	out := StateUp
	for _, c := range s.Components {
		switch c.State {
		case StateDown:
			return StateDown
		case StateDegraded:
			out = StateDegraded
		}
	}
	return out
}

// Status returns the summary of the backend and the parts it depends on.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	if err := c.Get(ctx, "/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// AdminStatus returns the detail. It needs an administrator who passed the step-up check.
func (c *Client) AdminStatus(ctx context.Context) (*StatusDetail, error) {
	var s StatusDetail
	if err := c.Get(ctx, "/admin/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}
