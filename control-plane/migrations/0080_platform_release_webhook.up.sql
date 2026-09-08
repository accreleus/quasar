-- 0080 — outbound notification when a platform release appears (#123).
--
-- PURELY ADDITIVE: two defaulted columns on instance_settings and one new
-- table. Nothing existing changes shape, and an instance that never configures
-- a webhook behaves exactly as before — release_webhook_enabled defaults false,
-- so the upgrade is a behavioural no-op.

-- ── instance_settings: where a notification goes ────────────────────────────
--
-- WHY HERE AND NOT ITS OWN TABLE. This is one instance-wide operator setting
-- with the same custody model as release_channel beside it: one control plane,
-- one value, set by an admin through PATCH /v1/admin/settings, which already
-- carries the admin gate, the one-transaction apply and the changed-keys audit
-- record. A second table would need all three again.
--
-- THE SHARED SECRET IS NOT HERE. It is an instance_secrets row
-- ('platform.release_webhook.secret', migration 0040), encrypted at rest under
-- a key that is never in the database. A plaintext credential column would put
-- the one thing a database dump must not yield straight into the dump.
--
-- No CHECK on the URL: a CHECK cannot express "https, no userinfo, a host we
-- are willing to dial", and a half-expression of that rule in SQL would be read
-- as the whole rule. The PATCH handler validates it (settings.ValidReleaseWebhookURL)
-- and internal/outbound re-checks it at delivery time, so a row edited by hand
-- still cannot become an unguarded request.
ALTER TABLE instance_settings
    ADD COLUMN release_webhook_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN release_webhook_url     TEXT    NOT NULL DEFAULT '';

COMMENT ON COLUMN instance_settings.release_webhook_enabled IS
    '#123: whether a detected platform release is POSTed to release_webhook_url. Default false — an instance that has never been configured sends nothing.';
COMMENT ON COLUMN instance_settings.release_webhook_url IS
    '#123: the https URL one release notification is POSTed to. Empty = none configured; enabling with an empty URL delivers nothing and is reported as skipped, never as a failure.';

-- ── platform_release_notifications: the dedupe state ────────────────────────
--
-- Detection runs on a schedule AND on demand ("Check now"), so the same release
-- is re-observed indefinitely. This table is what makes an announced release
-- stay announced: one row per platform_releases row, and a row with
-- delivered_at set is never notified again for as long as that release exists.
--
-- WHY KEYED ON THE RELEASE, NOT ON A TIMESTAMP HIGH-WATER MARK. A watermark
-- ("notified everything published before T") silently swallows a release
-- published out of order, or re-detected after a channel switch. The identity of
-- the thing announced is the only key that cannot drift.
--
-- ON DELETE CASCADE: the release cache is a cache — 0074's down migration drops
-- it wholesale — and a notification record for a release that no longer exists
-- would suppress nothing and explain nothing.
CREATE TABLE platform_release_notifications (
    release_id       UUID        PRIMARY KEY REFERENCES platform_releases(id) ON DELETE CASCADE,

    -- 'delivered' is terminal. 'failed' is retried on the next detection pass
    -- until attempts reaches the notifier's cap, which is a constant in Go
    -- rather than a CHECK here: it is a policy the code owns, and a schema that
    -- also encoded it would be a second place to change it.
    status           TEXT        NOT NULL,

    -- How many DELIVERY PASSES this release has cost, not HTTP requests: the
    -- retries within one pass are the notifier's business. It is what bounds a
    -- permanently-broken URL to a finite amount of noise.
    attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),

    last_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The receiver's HTTP status, or NULL when the request never got one (DNS,
    -- TLS, a refused dial, the allowlist). Those are different failures and an
    -- operator reading this needs to tell them apart.
    last_status_code INTEGER     NULL,
    -- Operator prose, bounded by the writer. NEVER the URL's credentials or the
    -- signing secret: nothing that writes this row has either in scope.
    last_error       TEXT        NULL,
    -- Set once, on the delivery that succeeded, and never rewritten.
    delivered_at     TIMESTAMPTZ NULL,

    CONSTRAINT platform_release_notifications_status_check
        CHECK (status IN ('delivered', 'failed')),
    -- The two halves of "delivered" cannot disagree: a delivered row has a
    -- time, a failed row does not.
    CONSTRAINT platform_release_notifications_delivered_check
        CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

COMMENT ON TABLE platform_release_notifications IS
    '#123: one row per platform release an outbound notification has been attempted for. A delivered row is the dedupe record; a failed row is retried on the next detection pass until the notifier''s attempt cap.';
