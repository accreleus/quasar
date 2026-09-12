package platform

import (
	"context"
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
		if got := applyTimeoutOutput(true, reachSent, "req-1", socket); got != "" {
			t.Errorf("output = %q, want empty: a connected agent needs no relay hint", got)
		}
	})

	t.Run("the agent never came back: name the request and how to read it", func(t *testing.T) {
		got := applyTimeoutOutput(false, reachSent, "req-1", socket)
		for _, want := range []string{
			"req-1", socket, "/v1/results/", "quasar-updater", "restore",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output %q does not mention %q", got, want)
			}
		}
	})

	t.Run("nothing was sent, so there is no result to read", func(t *testing.T) {
		got := applyTimeoutOutput(false, reachNotSent, "", socket)
		if got == "" {
			t.Fatal("output is empty; an operator is owed the reason")
		}
		if strings.Contains(got, "/v1/results/") {
			t.Errorf("output %q offers a read command for a request that was never sent", got)
		}
	})

	t.Run("a request id with nothing sent still offers no command", func(t *testing.T) {
		if got := applyTimeoutOutput(false, reachNotSent, "req-1", socket); strings.Contains(got, "/v1/results/") {
			t.Errorf("output %q offers a read command for a request that was never sent", got)
		}
	})

	// `pending` is ambiguous in BOTH directions — the updater's own first result
	// state is `pending` too — so this text may claim neither history, and must
	// hand over the one read that settles it, 404 included.
	t.Run("pending claims neither history and makes the host settle it", func(t *testing.T) {
		got := applyTimeoutOutput(false, reachUnknown, "req-1", socket)
		for _, want := range []string{"req-1", socket, "/v1/results/", "404"} {
			if !strings.Contains(got, want) {
				t.Errorf("output %q does not mention %q", got, want)
			}
		}
		if strings.Contains(got, applyNotSentOutput) {
			t.Error("the ambiguous case renders the strong not-sent claim")
		}
		for _, banned := range []string{
			"Nothing on this host was pulled, recreated or changed",
			"could not be relayed",
		} {
			if strings.Contains(got, banned) {
				t.Errorf("output %q claims %q, which this control plane cannot know", got, banned)
			}
		}
	})

	// The three texts are three, not two dressed differently.
	t.Run("each reach says something different", func(t *testing.T) {
		notSent := applyTimeoutOutput(false, reachNotSent, "req-1", socket)
		unknown := applyTimeoutOutput(false, reachUnknown, "req-1", socket)
		sent := applyTimeoutOutput(false, reachSent, "req-1", socket)
		if notSent == unknown || unknown == sent || notSent == sent {
			t.Error("two reaches render the same text; the distinction is the point")
		}
	})
}

// The reach is read off the row, and the ambiguity of `pending` is the whole
// reason this mapping is not just "has a request id".
func TestAttemptReach(t *testing.T) {
	for _, tc := range []struct {
		state, requestID string
		want             applyReach
	}{
		{AttemptQueued, "", reachNotSent},
		{AttemptWaitingSessions, "", reachNotSent},
		// Minted but not yet handed over: still nothing an updater can hold.
		{AttemptQueued, "req-1", reachNotSent},
		// MintRequestID wrote this before the send AND the updater's first
		// relayed state is this — one state, two histories.
		{AttemptPending, "req-1", reachUnknown},
		{AttemptPulling, "req-1", reachSent},
		{AttemptRecreating, "req-1", reachSent},
		{AttemptVerifying, "req-1", reachSent},
		// No id is no id, whatever the state says.
		{AttemptRecreating, "", reachNotSent},
	} {
		if got := attemptReach(tc.state, tc.requestID); got != tc.want {
			t.Errorf("attemptReach(%q, %q) = %v, want %v", tc.state, tc.requestID, got, tc.want)
		}
	}
}

// The relayed output is kept, and the join stays inside the column's CHECK: an
// output over 8 KiB is refused by Postgres, and a refused FailAttempt would
// leave the attempt non-terminal for ever.
func TestJoinApplyOutputStaysWithinTheColumnCheck(t *testing.T) {
	hint := applyTimeoutOutput(false, reachSent, "req-1", "/run/quasar-updater/updater.sock")

	t.Run("a short relay is kept whole", func(t *testing.T) {
		got := joinApplyOutput("pulling: layer 1/4", hint)
		if !strings.HasPrefix(got, "pulling: layer 1/4") || !strings.HasSuffix(got, hint) {
			t.Errorf("output %q did not keep the relayed text above the hint", got)
		}
	})

	t.Run("an over-long relay gives way, on a rune boundary", func(t *testing.T) {
		// Three hint lengths, so `keep` covers every residue mod 3 and a
		// 3-byte rune is cut mid-sequence in two of them: a test pinned to one
		// length stays green with the trim deleted.
		for n := 100; n < 103; n++ {
			short := strings.Repeat("x", n)
			got := joinApplyOutput(strings.Repeat("€", 6000), short)
			if len(got) > applyOutputLimit {
				t.Errorf("hint %d: output is %d bytes, over the %d-byte CHECK",
					n, len(got), applyOutputLimit)
			}
			if !utf8.ValidString(got) {
				t.Errorf("hint %d: output is not valid UTF-8; Postgres would reject it", n)
			}
			if !strings.HasSuffix(got, short) {
				t.Errorf("hint %d: the hint — the new information — was the part that was cut", n)
			}
		}
		// And the real hint, at its real length.
		got := joinApplyOutput(strings.Repeat("€", 6000), hint)
		if len(got) > applyOutputLimit || !utf8.ValidString(got) || !strings.HasSuffix(got, hint) {
			t.Errorf("the production hint does not join within the CHECK: %d bytes, valid=%v",
				len(got), utf8.ValidString(got))
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
	// The agent relayed its progress and then died with the container it was
	// recreating: the G-B shape, and what moves the row off `pending`.
	r.HandleReleaseState(context.Background(), testHostID, ReleaseStateReport{
		RequestID: agent.sent[0].RequestID, State: AttemptRecreating,
	})
	waitFor(t, "the relayed state", func() bool { return store.snapshot(a.ID).State == AttemptRecreating })
	waitFor(t, "the deadline to fire", func() bool { return store.snapshot(a.ID).State == AttemptFailed })

	final := store.snapshot(a.ID)
	if got := *final.Reason; got != ReasonTimeout {
		t.Fatalf("reason = %q, want timeout", got)
	}
	if final.Output == "" {
		t.Fatal("output is empty: the operator is told 'timeout' and nothing else (#201)")
	}
	requestID := agent.sent[0].RequestID
	for _, want := range []string{requestID, UpdaterSocketPath, "/v1/results/"} {
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

// A restart inside the connect/ack window and an ack followed by a dropped
// socket leave the identical `pending` row. Neither claim may be made: the
// attempt says so and hands over the read that settles it.
func TestReadoptedPendingAttemptIsNotTreatedAsSent(t *testing.T) {
	a := queuedAttempt(true)
	a.State = AttemptPending
	store := newFakeStore(a)
	store.requests["req-orphan"] = a.ID // minted before the restart
	agent := &fakeAgent{ack: Ack{OK: true}}
	deps := agent.deps()
	deps.Connected = func(string) bool { return false }
	r := testRunner(store, deps)
	r.Deadline = 80 * time.Millisecond
	defer r.Close()

	r.Start(a)
	waitFor(t, "the attempt to fail", func() bool { return store.snapshot(a.ID).State == AttemptFailed })

	final := store.snapshot(a.ID)
	if agent.sentCount() != 0 {
		t.Fatal("an adopted attempt must not be re-sent")
	}
	if final.Output == "" {
		t.Fatal("output is empty: the operator is owed the reason")
	}
	// This row can equally be #201's own shape (ack delivered, socket dropped
	// before the `pulling` relay), so it must not claim the host is untouched.
	if strings.Contains(final.Output, applyNotSentOutput) {
		t.Errorf("output %q claims nothing was applied, which a `pending` row cannot establish", final.Output)
	}
	// And it carries the read that resolves it, with what a 404 there means.
	for _, want := range []string{"req-orphan", "/v1/results/", "404"} {
		if !strings.Contains(final.Output, want) {
			t.Errorf("output %q does not mention %q", final.Output, want)
		}
	}
}

// The genuinely-never-sent case keeps the strong claim: the attempt expired in
// the connect wait with no request id minted, so no updater anywhere can hold a
// result and naming the read command would send an operator after a 404.
func TestTimeoutBeforeTheSendSaysNothingWasApplied(t *testing.T) {
	for _, state := range []string{AttemptQueued, AttemptWaitingSessions} {
		t.Run(state, func(t *testing.T) {
			a := queuedAttempt(true)
			a.State = state
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
			if !strings.Contains(final.Output, applyNotSentOutput) {
				t.Errorf("output %q does not say the apply was never sent", final.Output)
			}
			if strings.Contains(final.Output, "/v1/results/") {
				t.Errorf("output %q sends the operator after a result that cannot exist", final.Output)
			}
		})
	}
}
