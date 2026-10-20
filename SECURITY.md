# Security policy

KubeAmber is a security control, so bypasses matter more than in most projects.
Thank you for reporting responsibly.

## Reporting a vulnerability

Report privately through GitHub's private vulnerability reporting:

**https://github.com/kubeamber/kubeamber/security/advisories/new**

Please do not open a public issue, pull request or discussion for a suspected
vulnerability. Include what you can:

- the KubeAmber version or commit, and the Kubernetes version;
- the policy (or the relevant part) and the request that got through;
- steps to reproduce, and the impact you expect.

Examples of what we want to hear about: an agent change that should be held or
denied but is allowed; a way for an agent to approve or forge an approval, or
reuse one; a way to tamper with KubeAmber's own resources; and denial of
service against the webhook from an agent identity.

## What to expect

| Step | Target |
|---|---|
| Acknowledge the report | 3 working days |
| Initial assessment and severity | 7 working days |
| Fix or mitigation for high or critical issues | 30 days |
| Public disclosure | When a fix is released, or 90 days after the report, whichever comes first, coordinated with the reporter |

Fixed vulnerabilities are published as GitHub Security Advisories with a CVE
where appropriate, and noted in [CHANGELOG.md](CHANGELOG.md). Reporters are
credited unless they ask not to be.

## Supported versions

KubeAmber is pre-1.0. Security fixes go to the latest minor release only.

| Version | Supported |
|---|---|
| 0.1.x | Yes |
| < 0.1 | No |

## Scope

The threat model, including what KubeAmber does not protect against, is in
[docs/threat-model.md](docs/threat-model.md). Issues that fall under its
stated non-goals (for example, reads by agents, or agents using a human's
credentials) are hardening suggestions, not vulnerabilities. We still welcome
them as regular issues.
