package auth

import (
	"log/slog"
	"sync"

	"github.com/accreleus/quasar/control-plane/internal/semver"
)

type versionDecision int

const (
	versionProceed versionDecision = iota
	versionGate
)

// clientVersionHeader is the bearer-path carrier of the client's own version
// (control-api.md §Client version gate on bearer-authenticated endpoints, #380).
// It is the header twin of the login body's client_version — same grammar, same
// floor, same 426. There is deliberately no header twin of contract_version.
const clientVersionHeader = "X-Quasar-Client-Version"

// versionEval is the shared outcome of comparing a presented client version
// against the operator floor. Both callers (login body, bearer header) use the
// same comparison; they differ ONLY in what they do with a malformed version,
// which is why that fact is reported separately rather than folded into the
// decision.
type versionEval struct {
	decision versionDecision
	// malformed is true when a floor IS configured and the presented version is
	// not parseable as strict semver. With no floor configured there is nothing
	// to compare against, so nothing is malformed — the gate is off entirely.
	malformed bool
}

func evaluateClientVersion(clientVersion, minVersion string) versionEval {
	if minVersion == "" || clientVersion == "" {
		return versionEval{decision: versionProceed}
	}
	min, ok := semver.Parse(minVersion)
	if !ok {
		// A floor the operator typo'd is not a floor. Fail open, as today.
		return versionEval{decision: versionProceed}
	}
	cv, ok := semver.Parse(clientVersion)
	if !ok {
		return versionEval{decision: versionGate, malformed: true}
	}
	if semver.Compare(cv, min) < 0 {
		return versionEval{decision: versionGate}
	}
	return versionEval{decision: versionProceed}
}

// decideClientVersion is the LOGIN gate (P9-08 / #236). A malformed
// client_version gates: the client presents itself once, at a credential
// boundary, and a refusal costs it one retry.
func decideClientVersion(clientVersion, minVersion string) versionDecision {
	return evaluateClientVersion(clientVersion, minVersion).decision
}

// decideClientVersionHeader is the BEARER gate (#380). It differs from the login
// gate in exactly one case: a malformed header is treated as ABSENT (proceed)
// rather than gated. The header rides every authenticated request, so gating an
// unparseable value would take a client that is already signed in and, on one
// typo or an unexpected suffix, brick every call it makes — including the ones
// it would use to update itself. It buys nothing either: the gate is
// cooperative, and a client that wants to evade it can simply omit the header.
// The malformed value is logged instead, so an operator can still see it.
func decideClientVersionHeader(clientVersion, minVersion string) versionDecision {
	ev := evaluateClientVersion(clientVersion, minVersion)
	if ev.malformed {
		warnMalformedClientVersion(clientVersion)
		return versionProceed
	}
	return ev.decision
}

// malformedVersionWarnLimit caps how many DISTINCT malformed values this process
// will warn about. The header is on every request, so an unconditional warn is a
// log flood from a single misconfigured client; a per-value once is the useful
// signal, and the cap keeps the dedupe set bounded against a caller that varies
// the garbage.
const malformedVersionWarnLimit = 32

var malformedVersionWarned struct {
	sync.Mutex
	seen map[string]struct{}
}

func warnMalformedClientVersion(value string) {
	malformedVersionWarned.Lock()
	defer malformedVersionWarned.Unlock()
	if malformedVersionWarned.seen == nil {
		malformedVersionWarned.seen = make(map[string]struct{})
	}
	if _, dup := malformedVersionWarned.seen[value]; dup {
		return
	}
	if len(malformedVersionWarned.seen) >= malformedVersionWarnLimit {
		return
	}
	malformedVersionWarned.seen[value] = struct{}{}
	slog.Default().Warn("malformed client version header; treating as absent (not gated)",
		"header", clientVersionHeader, "value", value)
}
