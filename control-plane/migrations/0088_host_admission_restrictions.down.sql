-- Keep hosts.status as a conservative legacy cordon on rollback. Dropping the
-- owner table must never silently reopen admission.
UPDATE hosts h SET status = 'draining'
WHERE h.status = 'online'
  AND EXISTS (SELECT 1 FROM host_admission_restrictions r WHERE r.host_id = h.id);
DROP TABLE host_admission_restrictions;
