# Domain Docs

How the engineering skills should consume this repo's domain documentation.

Referenced from `CLAUDE.md` → "Agent skills". Layout: **single-context**.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root — the domain glossary (chain, rung, cert cap, stream plan, probe, envelope, entitlement, home, derived tile). It exists and is maintained; read it before naming things.
- **`docs/adr/`** — architecture decision records. **This directory does not exist yet.** That is expected: `/domain-modeling` creates ADRs lazily, when a decision actually gets resolved. Proceed silently; don't flag its absence and don't scaffold it upfront.

There is no `CONTEXT-MAP.md` and there should not be — this is a single-context repo despite having several language trees (`control-plane/`, `node-agent/`, `web/`). Those are deployment boundaries, not bounded contexts; they share one domain vocabulary, which is why there is one glossary.

## These are the glossary, not the architecture

`CONTEXT.md` defines *terms*. It does not supersede:

- **`docs/architecture-and-plan.md`** — the architecture of record.
- **`protocol/`** — the frozen wire contracts (a submodule of `quasar-protocol`). Contract changes happen there, gated on Opus + explicit human sign-off, then the pin is bumped here.
- **`CLAUDE.md`** — the architecture invariants and operating rules, which outrank everything in this file.

If a glossary term and a frozen contract disagree, the contract wins and the glossary is wrong.

## File structure

```
/
├── CONTEXT.md          ← the domain glossary (exists)
├── docs/adr/           ← created lazily by /domain-modeling (absent today)
├── docs/agents/        ← this directory: skill configuration
├── control-plane/      (Go)
├── node-agent/         (Rust)
└── web/                (TypeScript + React)
```

## Use the glossary's vocabulary

When output names a domain concept — an issue title, a refactor proposal, a hypothesis, a test name — use the term as `CONTEXT.md` defines it, and don't drift to a synonym it explicitly avoids. `CLAUDE.md` makes this a standing rule: *"Read it before naming things; add a term when work resolves one, rather than coining a synonym."*

If the concept isn't in the glossary, that's a signal: either the language is being invented (reconsider) or there's a real gap (note it for `/domain-modeling`).

One naming rule is load-bearing enough to repeat here: **never describe Quasar as a "clean-room successor" to Wolf.** Wolf is MIT and held in high regard.

## Flag ADR conflicts

If output contradicts an existing ADR, surface it rather than silently overriding:

> *Contradicts ADR-0007 (…), but worth reopening because…*

The same courtesy applies, with more force, to the architecture invariants in `CLAUDE.md`: those require escalation to Opus, not a flag.
