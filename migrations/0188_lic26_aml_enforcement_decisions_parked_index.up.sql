-- lic26 (N-9): the payments sweeper reads the screening-parked persons (aml_screening_parked_overdue, migration 0181) once per tenant
-- per pass. aml_enforcement_decisions (migration 0141) predates that read, so it gets its supporting index here. The statement is a
-- plain CREATE INDEX (migrations run in a transaction); the table is append-only and small relative to the ledger.
CREATE INDEX IF NOT EXISTS aml_enforcement_decisions_tenant_op_decided
    ON aml_enforcement_decisions (tenant_id, operation, decided_at DESC);
