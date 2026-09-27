-- Reverses migration 0104: drops the two statement-line CHECKs. Always
-- reversible (dropping a CHECK loses no data); migration 0102's own length
-- CHECKs remain.
ALTER TABLE payment_statement_lines
    DROP CONSTRAINT payment_statement_lines_merchant_reference_charset,
    DROP CONSTRAINT payment_statement_lines_asset_code_shape;
