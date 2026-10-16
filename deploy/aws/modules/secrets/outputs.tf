output "db_runtime_username" {
  value = var.db_runtime_username
}

output "db_runtime_secret_arn" {
  description = "JSON {username,password}. Consumers inject the \"password\" key only (\"<arn>:password::\")."
  value       = aws_secretsmanager_secret.db_runtime.arn
}

output "jwt_signing_secret_arn" {
  value = aws_secretsmanager_secret.jwt_signing.arn
}

output "seed_admin_password_secret_arn" {
  description = "Plain-string secret consumed by the one-off seed-admin task as SEED_ADMIN_PASSWORD."
  value       = aws_secretsmanager_secret.seed_admin.arn
}

output "staff_mfa_keys_secret_arn" {
  description = "JSON {factor_keys, mac_keys}. Consumers inject the keys individually (\"<arn>:factor_keys::\", \"<arn>:mac_keys::\")."
  value       = aws_secretsmanager_secret.staff_mfa_keys.arn
}

output "payout_instrument_keys_secret_arn" {
  description = "JSON {master_keys, fp_keys}. Consumers inject the keys individually (\"<arn>:master_keys::\", \"<arn>:fp_keys::\")."
  value       = aws_secretsmanager_secret.payout_instrument_keys.arn
}

output "actor_proof_keys_secret_arn" {
  description = "Plain-string secret (kid:base64) injected as ACTOR_PROOF_KEYS."
  value       = aws_secretsmanager_secret.actor_proof_keys.arn
}

output "alert_webhook_hmac_secret_arn" {
  description = "Plain-string HMAC secret of the platform alert webhook channel."
  value       = aws_secretsmanager_secret.alert_webhook_hmac.arn
}

output "alert_webhook_secret_ref" {
  description = "The awssm:// reference (ARN plus pinned versionId) for ALERT_WEBHOOK_SECRET_REF. Not secret."
  value       = "awssm://${aws_secretsmanager_secret.alert_webhook_hmac.arn}?versionId=${aws_secretsmanager_secret_version.alert_webhook_hmac.version_id}"
}

# The kid is not secret; it is the same string in every family (kids are per family).
output "key_family_kid" {
  description = "Active kid for the staff MFA (factor and MAC), payout instrument (master and fingerprint) and actor proof families."
  value       = local.key_kid
}

output "secret_version_ids" {
  description = "Secret-version resource IDs. Consumers depend on these so no task starts before a value exists."
  value = [
    aws_secretsmanager_secret_version.db_runtime.id,
    aws_secretsmanager_secret_version.jwt_signing.id,
    aws_secretsmanager_secret_version.seed_admin.id,
    aws_secretsmanager_secret_version.staff_mfa_keys.id,
    aws_secretsmanager_secret_version.payout_instrument_keys.id,
    aws_secretsmanager_secret_version.actor_proof_keys.id,
    aws_secretsmanager_secret_version.alert_webhook_hmac.id,
  ]
}
