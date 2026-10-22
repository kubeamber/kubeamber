---
title: Traffic lights for AI agents on Kubernetes
description: KubeAmber scores every change an AI agent makes to a Kubernetes cluster and gives it a light. Routine changes go through, risky ones wait for a human, dangerous ones are refused with the reason.
hide:
  - navigation
  - toc
---

<div class="ka-hero" markdown>

![KubeAmber](images/kubeamber-logo.svg#only-light){ .ka-hero__logo }
![KubeAmber](images/kubeamber-logo-dark.svg#only-dark){ .ka-hero__logo }

# Traffic lights for AI agents on Kubernetes { .ka-hero__title }

AI agents now get write access to clusters. RBAC can only say yes or no to a
verb on a resource. KubeAmber scores every change an agent makes and gives it
a light, with a reason the agent can read and act on.
{ .ka-hero__lead }

[Get started](getting-started.md){ .md-button .md-button--primary }
[View on GitHub](https://github.com/kubeamber/kubeamber){ .md-button }

</div>

<div class="ka-lights" markdown>

<div class="ka-light ka-light--green" markdown>
<span class="ka-lamp" aria-hidden="true"></span>
**Allow**

A routine change. Applied at once; the agent sees its risk score.
</div>

<div class="ka-light ka-light--amber" markdown>
<span class="ka-lamp" aria-hidden="true"></span>
**Hold**

A risky change. It waits for a person to approve it, then the agent retries
it once.
</div>

<div class="ka-light ka-light--red" markdown>
<span class="ka-lamp" aria-hidden="true"></span>
**Deny**

A dangerous change. Refused, with the reason and a hint toward a safer path.
</div>

</div>

## How it works

![How KubeAmber works: AI agents' changes go through kube-apiserver to KubeAmber, which scores each one and gives it a light. Allow is applied at once, hold creates a ChangeRequest that a person approves before the agent retries, deny is refused with the reason. Humans and controllers are never sent to KubeAmber.](images/architecture-overview.png){ .ka-diagram }

<div class="grid cards" markdown>

-   **Agents only**

    ---

    Generated CEL match conditions send only agent identities to the webhook.
    People and controllers never reach it, so if KubeAmber is down, only
    agents wait.

-   **Explainable scoring**

    ---

    A deterministic 0–100 score with a sentence for every point. No LLM in the
    decision path. [The risk model](risk-model.md)

-   **One approval, one change**

    ---

    A held change becomes a ChangeRequest. An approval covers exactly that
    change, once, and never comes from the agent or the person who launched it.

-   **Self-protection**

    ---

    Agents cannot touch KubeAmber's namespace, CRD, API service, RBAC or
    change requests, whatever the policy says. [Threat model](threat-model.md)

-   **Fits change management**

    ---

    `kubeamber bridge` turns held changes into ServiceNow change requests: the
    CAB approves there, and the ticket closes itself.
    [Integrations](integrations.md)

-   **Tells people, and keeps a record**

    ---

    Slack, Teams and webhook messages, Kubernetes Events, Prometheus metrics,
    and an OpenTelemetry span for every decision.

</div>

## Vendor-neutral by design

<!-- include: README.md#Vendor-neutral by design -->

## How it compares

<!-- include: README.md#How it compares -->

<div class="ka-cta" markdown>

**Try it on your laptop in a few minutes.** `make kind-demo` builds a kind
cluster and plays an agent with `cluster-admin` through every scenario.

[Get started](getting-started.md){ .md-button .md-button--primary }

</div>
