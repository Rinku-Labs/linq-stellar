# Data inventory

Tables behind the public API. Add a row with every new table.

| Table | Holds | Class | Sensitive fields | Retention |
|---|---|---|---|---|
| `integrators` | Callers of the public API | Operational | `webhook_secret_encrypted` is encrypted and unused until signed webhooks ship | Life of the integrator |
| `api_keys` | Credentials | Secret | No key is stored: `secret_hash` is a SHA-256 of a 256-bit random key, `key_prefix` is public | Kept after revocation, for the proof |
| `credential_events` | Rotation and revocation proof | Operational | None. Append-only by trigger | Permanent |
| `api_rate_counters` | Requests per key per minute | Operational | None | One hour, pruned by the worker |
