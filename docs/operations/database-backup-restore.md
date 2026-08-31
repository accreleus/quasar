# Database backup, restore, and candidate-binary preflight

Run this Wave 5 rehearsal before declaring a release candidate operationally
safe:

```bash
bash deploy/db-backup-restore-drill.sh
```

It creates a random Compose project with an in-memory Postgres 16 database. It
does **not** read `deploy/.env`, attach a host stack, or use a production volume.
The devtools image supplies `pg_dump`, `pg_restore`, Go, and the same embedded
migrations as the control-plane binary.

The rehearsal proves all of these in one disposable environment:

1. source code builds into a candidate control-plane binary;
2. that binary migrates a blank database to tracked migration head and becomes
   healthy;
3. a custom-format `pg_dump` backs up real related Quasar rows;
4. `pg_restore --exit-on-error --clean` restores to a separate database;
5. source and restored schema hashes match, seeded user/app/host/GPU/session/audit
   rows survive, and the same candidate binary starts against restored data at
   non-dirty migration head.

Keep command output with release evidence. A passing local rehearsal is not a
production backup. Before a real deployment, pause control-plane writes, capture
a timestamped custom-format dump from its actual Postgres service, checksum it,
copy it to independently retained storage, restore into an isolated Postgres
instance, and run the exact candidate image with its real deployment
configuration against that restored instance. Never restore over a live database.

## Version-upgrade gate

This repository has no release tags or supported-version manifest that identifies
the last supported control-plane/database pair. Several migrations are explicitly
one-way. Therefore an upgrade-from-last-supported rehearsal is **SKIP / blocked**,
not a pass: do not invent a predecessor image or roll a production database back.
Record a supported release manifest (image digest, control-plane commit,
migration head, Postgres major) before this gate can be rehearsed. The script only
proves backup/restore and candidate-binary/head compatibility.
