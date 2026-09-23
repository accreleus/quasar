-- RH05-09 (#342): canonical app host selection. A newly created canonical app
-- defaults to dynamic all-eligible membership; derived tiles borrow the parent.
BEGIN;

CREATE TABLE app_placement (
    app_id UUID PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK (mode IN ('all_eligible', 'fixed')),
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0)
);

CREATE TABLE app_placement_hosts (
    app_id UUID NOT NULL REFERENCES app_placement(app_id) ON DELETE CASCADE,
    host_id UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, host_id)
);

CREATE FUNCTION rh05_canonical_app_placement_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM apps WHERE id=NEW.app_id AND parent_app_id IS NOT NULL) THEN
        RAISE EXCEPTION 'derived tiles inherit parent placement' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER rh05_canonical_app_placement
    BEFORE INSERT OR UPDATE ON app_placement FOR EACH ROW EXECUTE FUNCTION rh05_canonical_app_placement_fn();

INSERT INTO app_placement (app_id, mode)
SELECT id, 'all_eligible' FROM apps WHERE parent_app_id IS NULL;

CREATE FUNCTION rh05_app_placement_default_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.parent_app_id IS NULL THEN
        INSERT INTO app_placement (app_id, mode) VALUES (NEW.id, 'all_eligible');
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER rh05_app_placement_default
    AFTER INSERT ON apps FOR EACH ROW EXECUTE FUNCTION rh05_app_placement_default_fn();

-- The supported app editor can detach a derived tile or assign a parent to
-- a standalone app. Keep its independent placement in the same transaction as
-- that identity change, so no committed canonical app lacks a policy row.
CREATE FUNCTION rh05_app_placement_parent_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.parent_app_id IS DISTINCT FROM NEW.parent_app_id THEN
        IF NEW.parent_app_id IS NULL THEN
            INSERT INTO app_placement (app_id, mode) VALUES (NEW.id, 'all_eligible')
                ON CONFLICT (app_id) DO NOTHING;
        ELSE
            DELETE FROM app_placement WHERE app_id = NEW.id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER rh05_app_placement_parent
    AFTER UPDATE OF parent_app_id ON apps FOR EACH ROW EXECUTE FUNCTION rh05_app_placement_parent_fn();

COMMIT;
