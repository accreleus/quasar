Harmless test hooks for `make e2e-run HOOKS=scripts/validate/stream/fixtures/hooks`: they print what
they were asked and change nothing. A hook that changes a real network knows a lab address and
lives outside this repository (docs/testing-e2e-suite.md, "Hooks").
