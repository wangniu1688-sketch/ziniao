# Production key management

## KEK key ring

Shop credentials, proxy passwords, and other envelope-encrypted secrets are protected by a
32-byte Key Encryption Key (KEK). The KEK wraps per-record Data Encryption Keys (DEKs);
the encrypted record stores the KEK version. The application must keep every KEK version
that is still referenced by stored ciphertext.

For production, provide the key ring through the process environment or a secret manager
that injects environment variables:

- `ZINIAO_KEK_KEYS`: comma-separated `version:base64` entries.
- `ZINIAO_KEK_CURRENT_VERSION`: version used for newly written ciphertext.
- `ZINIAO_JWT_SECRET`: independent random secret, at least 32 bytes.

Example format (the values below are placeholders; do not use them):

```sh
ZINIAO_KEK_KEYS='1:<base64-encoded-32-byte-key>,2:<base64-encoded-32-byte-key>'
ZINIAO_KEK_CURRENT_VERSION=2
ZINIAO_JWT_SECRET='<independent-random-secret-at-least-32-bytes>'
```

The current version must exist in the key ring. Keep version 1 while any stored credential
still has ciphertext prefixed with `v1.`. Do not delete an old key merely because a new
version has been deployed.

## Rotation procedure

1. Back up the secret manager entry and the database.
2. Add a new version to `ZINIAO_KEK_KEYS`, keeping all old versions.
3. Set `ZINIAO_KEK_CURRENT_VERSION` to the new version and restart the service.
4. Run the application's credential rekey operation for every tenant and verify that no
   ciphertext references the old version.
5. Only then remove the old key in a separately reviewed change.

Never generate a fresh random KEK on a production restart. Losing a KEK makes the DEKs
wrapped by that key unrecoverable. The in-memory random KEK remains available only for the
memory-backed development configuration.

This environment-variable key ring is an operational baseline, not a substitute for a
managed KMS/HSM. Production deployments should use a secret manager/KMS with access
controls, audit logs, backup, and a documented recovery process.
