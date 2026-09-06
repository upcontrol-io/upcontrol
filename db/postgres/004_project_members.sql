-- +goose Up
-- +goose StatementBegin
-- Teams and channels move from the workspace to the project: a workspace has
-- exactly one owner, everyone else is a member of the projects they were
-- invited to, and a channel, an invite and the scanner's memory all belong to
-- one project rather than to the whole account.

-- The owner is a column on the workspace, not a role in a membership table:
-- there is exactly one, it is never granted and never revoked.
ALTER TABLE tenant ADD COLUMN owner_person_id bigint REFERENCES person(id) ON DELETE SET NULL;
-- The sign-in door inserts the person and then the tenant, so the creator is
-- the newest person at or before the tenant's own creation; an older teammate
-- invited later sorts below. Unclaimed anonymous tenants keep NULL.
UPDATE tenant t SET owner_person_id = (
  SELECT tm.person_id FROM tenant_member tm JOIN person p ON p.id = tm.person_id
   WHERE tm.tenant_id = t.id AND tm.role = 'login' AND tm.status = 'active'
   ORDER BY (p.created_at <= t.created_at) DESC, p.created_at DESC, p.id
   LIMIT 1);
CREATE INDEX tenant_owner_idx ON tenant (owner_person_id) WHERE owner_person_id IS NOT NULL;

-- tenant_id rides along for the tenant-scoped-table invariant (invariant 3 in
-- 001_init.sql). The owner needs no row: ownership is tenant.owner_person_id.
CREATE TABLE project_member (
  project_id    bigint NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  person_id     bigint NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  role          text NOT NULL,                       -- notify|login
  status        text NOT NULL DEFAULT 'pending',     -- pending|active
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, person_id)
);
CREATE INDEX project_member_person_idx ON project_member (person_id);

-- A workspace member becomes a member of its oldest project only: the old row
-- said nothing about which project the person was meant to see.
INSERT INTO project_member (project_id, person_id, tenant_id, role, status)
SELECT (SELECT min(id) FROM project WHERE tenant_id = tm.tenant_id), tm.person_id, tm.tenant_id, tm.role, tm.status
  FROM tenant_member tm JOIN tenant t ON t.id = tm.tenant_id
 WHERE tm.person_id IS DISTINCT FROM t.owner_person_id
   AND EXISTS (SELECT 1 FROM project WHERE tenant_id = tm.tenant_id);
DROP TABLE tenant_member;

ALTER TABLE alert_channel ADD COLUMN project_id bigint REFERENCES project(id) ON DELETE CASCADE;
UPDATE alert_channel c SET project_id = (SELECT min(id) FROM project WHERE tenant_id = c.tenant_id);
DELETE FROM alert_channel WHERE project_id IS NULL;
ALTER TABLE alert_channel ALTER COLUMN project_id SET NOT NULL;
CREATE INDEX alert_channel_project_idx ON alert_channel (project_id);

ALTER TABLE telegram_invite ADD COLUMN project_id bigint REFERENCES project(id) ON DELETE CASCADE;
UPDATE telegram_invite i SET project_id = (SELECT min(id) FROM project WHERE tenant_id = i.tenant_id);
DELETE FROM telegram_invite WHERE project_id IS NULL;
ALTER TABLE telegram_invite ALTER COLUMN project_id SET NOT NULL;

-- The scanner's memory becomes per project: two projects of one workspace can
-- carry the same fingerprint, and one alerting must not silence the other.
ALTER TABLE error_alert_state ADD COLUMN project_id bigint REFERENCES project(id) ON DELETE CASCADE;
UPDATE error_alert_state s SET project_id = (SELECT min(id) FROM project WHERE tenant_id = s.tenant_id);
DELETE FROM error_alert_state WHERE project_id IS NULL;
ALTER TABLE error_alert_state ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE error_alert_state DROP CONSTRAINT error_alert_state_pkey;
ALTER TABLE error_alert_state ADD PRIMARY KEY (tenant_id, project_id, fingerprint, kind);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE error_alert_state DROP COLUMN project_id;
ALTER TABLE error_alert_state ADD PRIMARY KEY (tenant_id, fingerprint, kind);
ALTER TABLE telegram_invite DROP COLUMN project_id;
DROP INDEX IF EXISTS alert_channel_project_idx;
ALTER TABLE alert_channel DROP COLUMN project_id;

CREATE TABLE tenant_member (
  tenant_id     bigint NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
  person_id     bigint NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  role          text NOT NULL,
  status        text NOT NULL DEFAULT 'pending',
  PRIMARY KEY (tenant_id, person_id)
);
INSERT INTO tenant_member (tenant_id, person_id, role, status)
SELECT id, owner_person_id, 'login', 'active' FROM tenant WHERE owner_person_id IS NOT NULL
ON CONFLICT DO NOTHING;
INSERT INTO tenant_member (tenant_id, person_id, role, status)
SELECT DISTINCT ON (tenant_id, person_id) tenant_id, person_id, role, status FROM project_member
ON CONFLICT DO NOTHING;
DROP TABLE project_member;

DROP INDEX IF EXISTS tenant_owner_idx;
ALTER TABLE tenant DROP COLUMN owner_person_id;
-- +goose StatementEnd
