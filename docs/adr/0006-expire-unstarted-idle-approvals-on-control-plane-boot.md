---
status: accepted
date: 2026-09-23
---
# Unstarted idle-apply approvals expire on every control-plane boot

An approval to restart an idle host is tied to the control plane incarnation
that presented the reviewed change. Every control-plane restart invalidates
unstarted approvals before dispatch resumes; saved settings remain pending.
This also prevents a supported stopped-stack database restore from replaying
an old approval. Started attempts are recovered from the authenticated agent's
durable journal, including entries absent from a restored database, while
admission remains protected during uncertainty.

## Considered options

- Keep approval across restart with a separate durable restore fence. This
  preserves convenience but adds an external authority and a more complex
  backup protocol.
- Expire on boot (accepted): the operator reapproves an unstarted disruptive
  action. This follows the owner's RH05 Q27 decision and avoids treating a
  restored database row as authorization.

## Consequences

Reconnection within one control-plane boot can preserve a still-matching
approval. A delivered grant cannot be treated as revoked merely because its
database row changed; the agent must confirm nonacceptance or reveal a started
attempt. A restore under a running control plane is not a supported guarantee.
The frozen wire/schema amendment was owner-approved by explicit override of
an Opus `CHANGES REQUIRED` verdict; that verdict is recorded in #334 rather
than represented as reviewer sign-off.
