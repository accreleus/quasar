# REVIEW.md: how changes are reviewed

Read this when you review a change, or when you decide whether a change needs review. Writers read it too: the reviewer
holds you to it.

## Does it need a review, and how many reviewers?
- **Two reviewers** for the streaming path, auth or security, concurrency or durability, the update path, a migration,
  or a contract under `protocol/`. They get different briefs: one checks line by line against the issue, the contract
  and the evidence; the other is adversarial (how does this break?).
- **One reviewer** for docs, the changelog, tests only, CI, tooling and small UI wording.
- **No review round** for a follow-up delta that is tests only, wording, or a changelog line. The lead reads it.
- A clean merge with no commit of its own needs no review.

## The round budget
- **At most 3 review rounds per change.**
- A follow-up round reviews only the delta, and only the findings that reviewer raised.
- If round 3 still has a must-fix, stop reviewing. The writer gets a design directive: name the invariant the fixes
  keep missing, and fix it at the root. The count restarts once after that. A second miss goes to the operator.

## What the reviewer checks
1. **Root cause.** It fixes the cause the issue names, on every path.
2. **Reversion proof.** Each must-fix comes with a test that fails with the fix reverted. A test that passes either way
   is itself a must-fix.
3. **Invariant, not sites.** When a fix guards one rule at named call sites, ask what other path reaches the same state.
   If one does, the finding is the design.
4. **Cross-side order.** Anything that answers or replays a message from the other side of a contract (agent and
   control plane, client and server) is tested against the other side's real order, read from its code.
5. **Contract.** No documented behaviour changes without its contract sentence. Quote the sentence.
6. **Size.** Every added line is needed. Name anything speculative.
7. **Bench.** A streaming-path change (encoder, capture, ABR, transport, what the client presents) has its bench
   check for the head, or the landing says why not (see `AGENTS.md`, "Performance evidence").
8. **Public hygiene.** No hostname, address, ssh alias or operator path anywhere; role names only. The changelog line
   is present and true.

## Report
- First line: `APPROVE`, `APPROVE WITH NITS` or `REQUEST CHANGES`, at the exact commit SHA.
- Then a findings table: id, must / should / nit, `file:line`, one line each.
- Then details only for the musts. Keep the whole report short.
- The reviewer runs only the tests the change adds; CI and the writer's gates (`AGENTS.md`, "Verification levels") are
  the full test gate.
