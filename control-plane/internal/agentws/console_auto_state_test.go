package agentws

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestConsoleAutoStateAllowsOnlyOneConcurrentLaunch(t *testing.T) {
	state := newConsoleAutoState()
	const contenders = 32
	start := make(chan struct{})
	var winners atomic.Int32
	var wg sync.WaitGroup

	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if state.claimLaunch("host-1") {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("launch claim winners = %d, want 1", got)
	}
	state.finishLaunch("host-1", "session-1", true)
	if state.claimLaunch("host-1") {
		t.Fatal("recorded session admitted a second launch")
	}
}

func TestConsoleAutoStateReleasesFailedLaunch(t *testing.T) {
	state := newConsoleAutoState()
	if !state.claimLaunch("host-1") {
		t.Fatal("initial launch claim rejected")
	}
	state.finishLaunch("host-1", "", false)
	if !state.claimLaunch("host-1") {
		t.Fatal("failed launch claim was not released")
	}
}
