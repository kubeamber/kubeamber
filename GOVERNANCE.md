# Governance

KubeAmber is a maintainer-led open-source project. This document describes how
decisions are made today, and how governance will grow as the community does.

## Roles

- **Contributors** are anyone who opens issues, reviews, writes code or docs,
  or helps users.
- **Reviewers** have shown sustained, good-quality review in an area and are
  asked to review pull requests there.
- **Maintainers** are listed in [MAINTAINERS.md](MAINTAINERS.md). They review
  and merge pull requests, cut releases, triage security reports and set
  direction.

## Decision making

Most decisions are made by **lazy consensus** on the pull request or issue: a
proposal goes ahead if no maintainer objects within a reasonable time (at least
three working days for anything non-trivial).

These need explicit approval from a majority of maintainers, recorded on the
pull request or issue:

- changes to the `kubeamber.dev` API, the approval protocol or verdict semantics;
- changes to the risk model that alter scores for existing changes;
- adding or removing a maintainer;
- changes to this document;
- moving the project to a foundation, or changing its license.

If consensus cannot be reached, maintainers vote; a simple majority decides.
While there is a single maintainer, that maintainer decides, and records the
reasoning publicly on the issue.

## Becoming a reviewer or maintainer

Reviewers are nominated by a maintainer after several months of sustained
reviews or contributions in an area. Maintainers are nominated by an existing
maintainer, and approved by a majority of maintainers. We look for:

- sustained contribution over at least three months (code, review, docs, or support);
- sound judgement on safety: this project is a security control;
- respectful, constructive collaboration.

Maintainers who are inactive for six months may be moved to emeritus status
after a conversation with them.

## Multi-organisation maintainership

A single company should not control a safety layer that many companies'
agents depend on. Before applying for CNCF Incubation, the project aims to
have maintainers from at least two organisations. No single organisation
should then hold a majority of maintainer seats.

## Community

Discussion happens in GitHub issues and pull requests. Public community
meetings will be announced in the README once there are regular contributors.

## Code of conduct and security

All participants follow the [Code of Conduct](CODE_OF_CONDUCT.md). Security
issues are handled as described in [SECURITY.md](SECURITY.md).
