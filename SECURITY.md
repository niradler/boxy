# Security Policy

## Supported versions

Only the latest release of boxy receives security fixes.

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Email: nir.adler@komodor.io

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
