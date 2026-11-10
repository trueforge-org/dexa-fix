package jobs

import "testing"

func TestAStoppedGroupRefusesNewJobs(t *testing.T) {
	g := New()
	ran := make(chan struct{})
	if !g.Start(func() { close(ran) }) {
		t.Fatal("a running group refused a job")
	}
	g.Stop() // waits for the job
	<-ran
	if g.Start(func() { t.Error("a job ran after Stop") }) {
		t.Fatal("a stopped group accepted a job")
	}
	if g.Ctx.Err() == nil {
		t.Fatal("Stop did not cancel the context")
	}
}
