-- +goose Up
-- +goose StatementBegin
-- The ring retention subsystem is retired (reduction wave 7). It was inert end
-- to end: nothing ever wrote tenant_line_ledger, so cutoff.Recompute always
-- derived cutoff_seq = retain_seq = 0, every log query's `seq >= 0` filter was a
-- no-op, and no row was ever deleted by retain_seq. Real retention is, and was,
-- the daily logs partitions dropped by the log-partitions worker at
-- max(plan_entitlement.window_hours) + 24 h.
--
-- This is the REVERSIBLE half of a two-phase removal. The tables are renamed
-- rather than dropped so a week of production proves nothing reads them; the
-- Down section renames them straight back. The drop belongs in a LATER
-- migration, written only after that week is clean (reduction wave 8) — do not
-- add a DROP here.
--
-- project_seq is untouched and stays: sequence allocation is live and load
-- bearing.
ALTER TABLE tenant_line_ledger RENAME TO zz_dead_tenant_line_ledger;
ALTER TABLE project_window RENAME TO zz_dead_project_window;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Genuinely reversible: this is the whole point of renaming instead of dropping.
ALTER TABLE zz_dead_project_window RENAME TO project_window;
ALTER TABLE zz_dead_tenant_line_ledger RENAME TO tenant_line_ledger;
-- +goose StatementEnd
