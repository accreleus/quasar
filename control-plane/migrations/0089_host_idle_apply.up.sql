-- RH05 #338: boot-fenced idle approvals and complete journal reconciliation.
CREATE TABLE rh05_control_boot (
    id BOOLEAN PRIMARY KEY CHECK (id),
    incarnation UUID NOT NULL,
    started_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE host_config_approvals (
    id UUID PRIMARY KEY,
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    group_key TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision >= 0),
    approved_digest TEXT NOT NULL,
    prerequisites_digest TEXT NOT NULL,
    boot_incarnation UUID NOT NULL,
    review_id UUID NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('approved','offered','cancel_pending',
        'superseded','expired','accepted','revoked_unstarted')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (host_id,group_key,review_id)
);
CREATE UNIQUE INDEX host_config_approvals_one_live_host
    ON host_config_approvals(host_id)
    WHERE state IN ('approved','offered','cancel_pending');

CREATE TABLE host_approval_review_tokens (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    group_key TEXT NOT NULL,
    review_id UUID NOT NULL,
    PRIMARY KEY (host_id,group_key),
    FOREIGN KEY (host_id,group_key) REFERENCES host_setting_groups(host_id,group_key) ON DELETE CASCADE
);
CREATE TABLE host_approval_review_issued (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    group_key TEXT NOT NULL,
    review_id UUID NOT NULL,
    PRIMARY KEY (host_id,group_key,review_id),
    FOREIGN KEY (host_id,group_key) REFERENCES host_setting_groups(host_id,group_key) ON DELETE CASCADE
);
CREATE FUNCTION rh05_guard_review_token() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.review_id = OLD.review_id THEN
        RETURN NEW;
    END IF;
    LOOP
        INSERT INTO host_approval_review_issued(host_id,group_key,review_id)
        VALUES (NEW.host_id,NEW.group_key,NEW.review_id)
        ON CONFLICT DO NOTHING;
        IF FOUND THEN
            RETURN NEW;
        END IF;
        NEW.review_id := gen_random_uuid();
    END LOOP;
END
$$;
CREATE TRIGGER host_approval_review_token_guard
    BEFORE INSERT OR UPDATE OF review_id ON host_approval_review_tokens
    FOR EACH ROW EXECUTE FUNCTION rh05_guard_review_token();
INSERT INTO host_approval_review_tokens(host_id,group_key,review_id)
SELECT host_id,group_key,gen_random_uuid() FROM host_setting_groups WHERE scope='restart';

CREATE TABLE host_config_attempts (
    id UUID PRIMARY KEY,
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    group_key TEXT NOT NULL,
    approved_digest TEXT NOT NULL,
    approved_revision BIGINT NOT NULL CHECK (approved_revision >= 0),
    scope TEXT NOT NULL CHECK (scope IN ('next_session','restart')),
    boot_incarnation UUID NOT NULL,
    grant_connection UUID NULL,
    phase TEXT NOT NULL CHECK (phase IN ('offered','accepted','activating',
        'awaiting_startup','verifying','applied','failed','recovery_verifying',
        'recovery_awaiting_startup','recovered','uncertain','revoked_unstarted')),
    journal_sequence BIGINT NULL CHECK (journal_sequence >= 0),
    started_at TIMESTAMPTZ NULL,
    terminal_at TIMESTAMPTZ NULL,
    recovery_attempted BOOLEAN NOT NULL DEFAULT false,
    error_code TEXT NULL,
    error_detail TEXT NULL,
    UNIQUE (host_id,group_key,id)
);
CREATE UNIQUE INDEX host_config_attempts_one_restart_host
    ON host_config_attempts(host_id)
    WHERE scope='restart' AND terminal_at IS NULL;

CREATE TABLE host_journal_reconciliation (
    host_id UUID PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    boot_incarnation UUID NOT NULL,
    connection_incarnation UUID NULL,
    state TEXT NOT NULL CHECK (state IN ('pending','complete','quarantined')),
    completed_at TIMESTAMPTZ NULL,
    continuation_cursor TEXT NULL
);

CREATE TABLE host_journal_active_snapshots (
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    group_key TEXT NOT NULL,
    connection_incarnation UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('seeded','verified')),
    digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (host_id,group_key)
);

CREATE TABLE host_hardware_evidence (
    host_id UUID PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    connection_incarnation UUID NOT NULL,
    gpus JSONB NOT NULL CHECK (jsonb_typeof(gpus)='array'),
    readiness JSONB NOT NULL CHECK (jsonb_typeof(readiness)='array'),
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Current authenticated heartbeat is advisory idle-wait evidence. Missing or
-- stale inventory is unknown and never authorizes restart dispatch.
CREATE TABLE host_idle_inventory (
    host_id UUID PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    connection_incarnation UUID NOT NULL,
    running_sessions JSONB NOT NULL CHECK (jsonb_typeof(running_sessions)='array'),
    reported_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
