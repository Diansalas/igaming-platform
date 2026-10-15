#!/usr/bin/env bash
# Backup-set signing and verification (A12 security review H3). SOURCED by backup.sh and restore.sh.
#
# The set is authenticated by an Ed25519 signature over SHA256SUMS (which lists every artefact including manifest.json), made with
# `openssl pkeyutl -sign -rawin` (OpenSSL >= 3.0; no other tool needed). Keys are supplied BY REFERENCE and never live in the
# backup directory or the repository:
#   signing   (backup runner)  BACKUP_SIGNING_KEY_FILE      PEM private key, mode 0600/0400, materialised from the secret store / KMS-wrapped
#   verifying (restorer)       RESTORE_VERIFY_PUBKEY_FILE   PEM public key, and
#                              RESTORE_VERIFY_PUBKEY_SHA256 the pinned SHA-256 of its DER encoding (a fingerprint the operator wrote down
#                                                            or holds in change control), compared BEFORE the key is trusted
# A backup directory is verified BEFORE anything is checksummed, decrypted or restored and before any database contact. A set that is
# unsigned, signed by another key, or altered after signing is refused. There is no "allow unsigned" switch.

# shellcheck shell=bash

bk_pubkey_fingerprint() { openssl pkey -pubin -in "$1" -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1; }

# bk_check_key_file <path> <what> : exists, regular, private
bk_check_secret_file() {
  [ -f "$1" ] || bk_die "$2 missing: $1"
  case "$(stat -c %a "$1")" in 600|400) ;; *) bk_die "$2 $1 must be mode 0600 or 0400" ;; esac
}

# bk_sign_set <run dir> : writes SHA256SUMS.sig for <run dir>/SHA256SUMS; the key must not be inside the run dir
bk_sign_set() {
  local dir="$1" key="${BACKUP_SIGNING_KEY_FILE:-}"
  [ -n "$key" ] || bk_die "BACKUP_SIGNING_KEY_FILE is required: backup sets must be signed (A12 security review H3)"
  bk_check_secret_file "$key" "signing key"
  case "$(readlink -f "$key")" in "$(readlink -f "$dir")"/*) bk_die "the signing key must not live inside the backup directory" ;; esac
  openssl pkeyutl -sign -rawin -inkey "$key" -in "${dir}/SHA256SUMS" -out "${dir}/SHA256SUMS.sig" 2>/dev/null \
    || bk_die "signing failed (is ${key} an Ed25519 private key and OpenSSL >= 3.0 available?)"
}

# bk_verify_set_signature <run dir> : refuse unless SHA256SUMS carries a valid signature by the PINNED key
bk_verify_set_signature() {
  local dir="$1" pub="${RESTORE_VERIFY_PUBKEY_FILE:-}" pin="${RESTORE_VERIFY_PUBKEY_SHA256:-}" fp
  [ -n "$pub" ] && [ -n "$pin" ] || bk_die "RESTORE_VERIFY_PUBKEY_FILE and RESTORE_VERIFY_PUBKEY_SHA256 (pinned fingerprint) are required: a backup set is only restored if its signature verifies against a key pinned outside the backup directory"
  [ -f "$pub" ] || bk_die "verification key missing: ${pub}"
  case "$(readlink -f "$pub")" in "$(readlink -f "$dir")"/*) bk_die "the verification key must not be read from the backup directory" ;; esac
  [[ "$pin" =~ ^[0-9a-f]{64}$ ]] || bk_die "RESTORE_VERIFY_PUBKEY_SHA256 must be 64 lower-case hex characters"
  fp="$(bk_pubkey_fingerprint "$pub")"
  [ "$fp" = "$pin" ] || bk_die "the verification key's fingerprint (${fp}) does not equal the pinned RESTORE_VERIFY_PUBKEY_SHA256; refusing"
  [ -f "${dir}/SHA256SUMS" ] && [ -f "${dir}/SHA256SUMS.sig" ] || bk_die "UNSIGNED backup set (SHA256SUMS or SHA256SUMS.sig missing) in ${dir}: refusing; nothing was restored"
  openssl pkeyutl -verify -rawin -pubin -inkey "$pub" -in "${dir}/SHA256SUMS" -sigfile "${dir}/SHA256SUMS.sig" >/dev/null 2>&1 \
    || bk_die "SIGNATURE VERIFICATION FAILED for ${dir}: the set was not signed by the pinned key or was altered; nothing was restored"
}
