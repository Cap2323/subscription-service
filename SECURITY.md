# Security operations

Runtime authenticates RS256 JWTs with pinned local public keys, issuer, audience, exp and iat. Token lifetime is at most one hour. The local token issuer is a development tool, not an identity provider. Use real identity infrastructure in production. No password/login endpoint is provided.

User identity comes exclusively from sub. Runtime SQL includes bucket_id and user_id and PostgreSQL FORCE RLS provides a second boundary. Runtime must use a role without SUPERUSER/BYPASSRLS or migration-role membership. Admin credentials are restricted to maintenance jobs; the admin binary is absent from the API image.

Secrets are not supplied in the archive. Do not commit .env, private keys, access tokens, database dumps or production DSNs. Use secret-manager integration, encryption at rest, RBAC and audited secret rotation. Kubernetes Secret is not encryption by itself.

Run go tests, govulncheck and an image scanner for every release. Pin release images by digest and retain an SBOM. Current dependency checks are evidence for one build, not a guarantee of future safety.

Report a suspected issue privately to the repository maintainer; the owner should insert their real security contact before publication. Do not put live tokens or customer data in public issues.
