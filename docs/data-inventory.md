# Data inventory

Tables behind the public API. Add a row with every new table.

| Table | Holds | Class | Sensitive fields | Retention |
|---|---|---|---|---|
| `integrators` | Callers of the public API | Operational | `webhook_secret_encrypted` is sealed with `ENCRYPTION_KEY`; `webhook_url` can carry the integrator's token | Life of the integrator |
| `api_keys` | Credentials | Secret | No key is stored: `secret_hash` is a SHA-256 of a 256-bit random key, `key_prefix` is public | Kept after revocation, for the proof |
| `credential_events` | Rotation and revocation proof | Operational | None. Append-only by trigger | Permanent |
| `api_rate_counters` | Requests per key per minute | Operational | None | One hour, pruned by the worker |
| `webhook_deliveries` | One row per webhook attempt | Operational | None. Host only, never the path, and errors have the URL removed | 90 days, pruned by the worker in batches |
