# Security Policy

## Supported versions

Only the latest release of boxy receives security fixes.

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Email: me@niradler.com

Please include:

- A description of the vulnerability and its potential impact.
- Steps to reproduce (proof-of-concept if available).
- Which component is affected (router / operator / controller / Helm chart).

You will receive an acknowledgement within 2 business days. We aim to release a patch
within 14 days for critical issues and 30 days for others, coordinated with the reporter.

## Scope

The primary security boundary is the nsjail sandbox: code running inside a sandbox must
not be able to escape to the host or reach other sandboxes. Issues that bypass this
boundary are treated as Critical.

Secondary boundaries include: controller token and mTLS authentication between components,
RBAC minimality in the Helm chart, and environment variable filtering.

## Per-user session isolation and the `X-Session-Id` trust model

`X-Session-Id` is the per-user runtime key. The router uses it verbatim as the `Session`
name and, on first contact, provisions a session bound to the `X-Sandbox-Id` config. The
router authorizes the **calling identity** (TokenReview) and runs a SubjectAccessReview for
`get sandboxes` / `create sessions` / `update sessions` against the **specific session/sandbox
name** before resolving or using a session. It does **not** bind a session to the caller beyond
that SAR.

This means isolation between end users depends on how callers map to identities:

- **Per-end-user identity** (each end user calls the router with their own ServiceAccount /
  token): grant `sessions` verbs with `resourceNames` scoped to that user's own session id(s).
  The router's name-scoped SAR then enforces that a caller can only address its own sessions. A
  blanket namespace-wide `update sessions` grant lets any such caller drive any session by
  passing its `X-Session-Id` — do not grant it.
- **Trusted gateway** (a single front-end — e.g. the corpbot nanobot plugin — holds one
  ServiceAccount and derives `X-Session-Id` from a trusted per-message identity it never lets
  the model or end user influence): the gateway's credential is the trust boundary. Treat its
  token as privileged; anyone who obtains it can address any session.

In both models the model/tool arguments must never be able to set `X-Session-Id`.
