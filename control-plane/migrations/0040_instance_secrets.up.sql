-- Encrypted operator secrets — the reusable facility, not an artwork one-off.
--
-- WHY A FLAT name-KEYED TABLE, NOT A SCOPED ONE.
-- Everything this holds is an INSTANCE-WIDE operator credential, the same
-- custody model as instance_settings: one control plane, one value, set by an
-- admin, used by the server. Nothing here is per-user or per-host — a user
-- never has a secret in this table (auth_tokens and password hashes are their
-- own, differently-shaped things), and a host credential would belong to the
-- host row. A (scope, name) composite key now would be structure with no
-- consumer; namespacing instead lives in the NAME by convention
-- ('artwork.steamgriddb.api_key'), which gives the same grouping for free and
-- leaves an additive `scope` column available if a future feature genuinely
-- needs one.
--
-- WHAT IS AND IS NOT ENCRYPTED HERE.
-- ciphertext + nonce are AES-256-GCM over the plaintext, with the row's NAME
-- bound in as additional authenticated data, so a ciphertext copied onto a
-- different name fails to decrypt rather than silently becoming that other
-- secret. The nonce is random per encryption and never reused — it is stored
-- because GCM needs it to decrypt, and a nonce is not secret.
-- The master key is NOT in this table and must never be: it comes from
-- QUASAR_SECRET_KEY in the environment. A database dump is therefore not enough
-- to read these values, which is the entire point.
CREATE TABLE instance_secrets (
    -- Stable identifier the code asks for, e.g. 'artwork.steamgriddb.api_key'.
    -- Only names a registered descriptor declares are settable through the API;
    -- the column itself is unconstrained so a future feature adds a secret with
    -- no migration.
    name        TEXT        PRIMARY KEY,

    -- AES-256-GCM ciphertext (includes the GCM tag) and its per-encryption
    -- random nonce. Two writes of the SAME plaintext produce different bytes in
    -- both columns; that is a correctness requirement, not a coincidence.
    ciphertext  BYTEA       NOT NULL,
    nonce       BYTEA       NOT NULL,

    -- Which master key encrypted this row. Rotation is not implemented, but it
    -- is deliberately not designed out: an operator can supply older keys as
    -- decrypt-only so a rotated deployment keeps reading rows written under the
    -- previous key, and a future re-encrypt sweep needs no schema change.
    key_version INT         NOT NULL CHECK (key_version >= 1),

    -- Last few characters of the plaintext, stored so the admin UI can say
    -- "a key ending 3f9a is configured" WITHOUT the master key being present or
    -- correct — which is exactly the situation an operator needs help
    -- diagnosing. Deliberately short (see secrets.Hint: nothing at all for a
    -- short secret), so this is an identification aid and not a head start on
    -- guessing the value.
    hint        TEXT        NOT NULL DEFAULT '',

    -- Who set it last. ON DELETE SET NULL: deleting the admin who configured a
    -- secret must not delete the secret and take the feature down with them.
    updated_by  UUID        REFERENCES users(id) ON DELETE SET NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER instance_secrets_set_updated_at BEFORE UPDATE ON instance_secrets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
