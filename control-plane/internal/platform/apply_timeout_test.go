package platform

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// #201: a host apply that expires with its agent gone is a stranded verdict,
// not a mystery. These pin what the attempt says about where that verdict is.

func TestApplyTimeoutOutput(t *testing.T) {
	const socket = "/run/quasar-updater/updater.sock"

	t.Run("the agent is on the wire, so nothing about the relay is broken", func(t *testing.T) {
		if got := applyTimeoutOutput(true, true, "req-1", socket); got != "" {
			t.Errorf("output = %q, want empty: a connected agent needs no relay hint", got)
		}
	})

	t.Run("the agent never came back: name the request and how to read it", func(t *testing.T) {
		got := applyTimeoutOutput(false, true, "req-1", socket)
		for _, want := range []string{
			"req-1", socket, "/v1/results/", "quasar-updater", "restore",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output %q does not mention %q", got, want)
			}
		}
	})

	t.Run("nothing was sent, so there is no result to read", func(t *testing.T) {
		got := applyTimeoutOutput(false, false, "", socket)
		if got == "" {
			t.Fatal("output is empty; an operator is owed the reason")
		}
		if strings.Contains(got, "/v1/results/") {
			t.Errorf("output %q offers a read command for a request that was never sent", got)
		}
	})

	t.Run("a request id with nothing sent still offers no command", func(t *testing.T) {
		if got := applyTimeoutOutput(false, false, "req-1", socket); strings.Contains(got, "/v1/results/") {
			t.Errorf("output %q offers a read command for a request that was never sent", got)
		}
	})
}

// The relayed output is kept, and the join stays inside the column's CHECK: an
// output over 8 KiB is refused by Postgres, and a refused FailAttempt would
// leave the attempt non-terminal for ever.
func TestJoinApplyOutputStaysWithinTheColumnCheck(t *testing.T) {
	hint := applyTimeoutOutput(false, true, "req-1", "/run/quasar-updater/updater.sock")

	t.Run("a short relay is kept whole", func(t *testing.T) {
		got := joinApplyOutput("pulling: layer 1/4", hint)
		if !strings.HasPrefix(got, "pulling: layer 1/4") || !strings.HasSuffix(got, hint) {
			t.Errorf("output %q did not keep the relayed text above the hint", got)
		}
	})

	t.Run("an over-long relay gives way, on a rune boundary", func(t *testing.T) {
		// Multi-byte throughout, so a byte-wise cut lands mid-rune.
		got := joinApplyOutput(strings.Repeat("é", 6000), hint)
		if len(got) > applyOutputLimit {
			t.Errorf("output is %d bytes, over the %d-byte CHECK", len(got), applyOutputLimit)
		}
		if !utf8.ValidString(got) {
			t.Error("output is not valid UTF-8; Postgres would reject it")
		}
		if !strings.HasSuffix(got, hint) {
			t.Error("the hint — the new information — was the part that was cut")
		}
	})

	t.Run("nothing relayed leaves the hint alone", func(t *testing.T) {
		if got := joinApplyOutput("", hint); got != hint {
			t.Errorf("output = %q, want the hint unchanged", got)
		}
	})
}

// The live #173 G-B shape: the apply went out, the new agent failed its health
// wait, the updater's automatic restore failed too, and no agent ever came back
// to relay the verdict. The deadline is all the control plane has — so it says
// where the verdict is instead of failing with an empty output.
func TestTimeoutWithNoAgentPointsAtTheUpdatersResult(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: true}}
	deps := agent.deps()
	// Connected until the apply is accepted, gone afterwards: exactly the host
	// whose agent carried the command out and never returned.
	deps.Connected = func(string) bool { return agent.sentCount() == 0 }
	r := testRunner(store, deps)
	r.Deadline = 250 * time.Millisecond
	defer r.Close()

	r.Start(a)
	waitFor(t, "release_apply to be sent", func() bool { return agent.sentCount() == 1 })
	waitFor(t, "the deadline to fire", func() bool { return store.snapshot(a.ID).State == AttemptFailed })

	final := store.snapshot(a.ID)
	if got := *final.Reason; got != ReasonTimeout {
		t.Fatalf("reason = %q, want timeout", got)
	}
	if final.Output == "" {
		t.Fatal("output is empty: the operator is told 'timeout' and nothing else (#201)")
	}
	requestID := agent.sent[0].RequestID
	for _, want := range []string{requestID, ConfiguredUpdaterSocket(), "/v1/results/"} {
		if !strings.Contains(final.Output, want) {
			t.Errorf("output %q does not mention %q", final.Output, want)
		}
	}
}

// The counterpart: an agent that IS on the wire and simply never reported is a
// different failure, and the relay hint would be a lie about it.
func TestTimeoutWithTheAgentConnectedIsNotTheRelayHint(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: true}}
	deps := agent.deps()
	deps.Connected = func(string) bool { return true }
	r := testRunner(store, deps)
	r.Deadline = 250 * time.Millisecond
	defer r.Close()

	r.Start(a)
	waitFor(t, "release_apply to be sent", func() bool { return agent.sentCount() == 1 })
	waitFor(t, "the deadline to fire", func() bool { return store.snapshot(a.ID).State == AttemptFailed })

	if got := store.snapshot(a.ID).Output; got != "" {
		t.Errorf("output = %q, want empty for a host whose agent is still connected", got)
	}
}

// The other timeout with no agent: the apply was never sent, so there is no
// updater result anywhere and the host is untouched. Saying "read the result"
// there would send an operator after a 404.
func TestTimeoutBeforeTheSendSaysNothingWasApplied(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: true}}
	deps := agent.deps()
	deps.Connected = func(string) bool { return false }
	r := testRunner(store, deps)
	r.ConnectWait = 20 * time.Millisecond
	defer r.Close()

	r.Start(a)
	waitFor(t, "the attempt to fail", func() bool { return store.snapshot(a.ID).State == AttemptFailed })

	final := store.snapshot(a.ID)
	if final.Output == "" {
		t.Fatal("output is empty: an apply that was never sent is still owed an explanation")
	}
	if strings.Contains(final.Output, "/v1/results/") {
		t.Errorf("output %q sends the operator after a result that cannot exist", final.Output)
	}
}
