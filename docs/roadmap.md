# Roadmap

Design level only: what comes next and why. Nothing here is built unless the
[README](../README.md) status table says so. The product vision is the
[PR/FAQ](https://claude.ai/artifact/8UnmdjQ2HzodM9keAQnKoi).

Two tracks run in this order:

1. **Regulated pilot**: what a bank's auditor asks for first. See the PR/FAQ
   ("Roadmap to a regulated pilot") and [architecture.md](architecture.md#known-gaps-and-next-steps).
2. **Agent scale**: what changes when most changes are written by AI agents. This document.

## Why an agent-scale track

In 2026 the large AI labs and code hosts reported the same problem: agents write code
faster than source control, review and CI can absorb it. They answered with the same
storage pattern Strata already uses (immutable content-addressed data in object storage,
compare-and-swap on one pointer per ref). They solved throughput. None of them published
an answer for a regulator: who or what wrote a line, under which rules, approved by whom.

A review of [ASPEN](https://github.com/reason-machines/aspen) and of published work from
OpenAI, Google, Microsoft/GitHub and Anthropic (November 2026) found Strata's base sound
and three gaps:

- no signed provenance per change,
- Layers are not yet treated as a trust boundary,
- one S3 object per content object.

## Wave A: trust in agent changes (before private preview)

| # | Item | Design change | Biggest design risk |
| --- | --- | --- | --- |
| A1 | Agent blueprints | An agent belongs to a class with scopes, a budget, an expiry and a required human sponsor. One operation disables a whole class. Credentials are short-lived and push only to the agent's own branches. | The kill switch must work from the standby region and during an IdP outage. |
| A2 | Independence rule and risk tiers | The sponsor or launcher of an agent never counts as its approver. The Cedar context gains a risk tier: docs or comments, logic, sensitive paths. | Tier rules that are easy to game, such as logic hidden in a docs change. |
| A3 | Signed provenance record | Each change gets a server-signed record: principal, blueprint, sponsor, model and version, Layer digests in effect, prompt digest, session ID. It comes from the session and credential, never from commit text. Client signing is keyless through the IdP. | Prompts may hold personal data. Agent runtimes report different metadata. |
| A4 | Layers as capability grants | Layer bundles are signed and clients verify the digest before loading. Three classes: text rules, executable hooks, MCP servers. Allow-lists name the server and the permitted tools and scopes. Nothing from a Layer or repository runs before trust. | Clients Strata does not control may skip verification. |
| A5 | Protected agent-instruction files | `AGENTS.md`, `CLAUDE.md`, skills and hook files in a repository are protected paths: four-eyes to change, and they cannot shadow a locked Layer file. | The list of file names changes with every agent product. |
| A6 | Storage layout decision | Decide now: one immutable segment per push plus an index, compacted in the background. Hashes, ledger and export must not depend on the physical layout. Build in wave B. | Compaction must not rewrite locked data or break reproducible exports and replication. |

## Wave B: adoption and operations (public preview)

| # | Item | Design change | Biggest design risk |
| --- | --- | --- | --- |
| B1 | Query API and MCP server | History, blame, diff, merge-base, Layer resolution and provenance lookup by API. Agents and CI need no checkout. Comes before virtual checkout. | Cost of blame on deep history in Lambda. |
| B2 | Mirror mode | First Git bridge step: Strata sits in front of the incumbent host, read- and write-through, and records evidence. GitHub first. | Two sources of truth while mirroring. Force-pushes on the host. |
| B3 | Health-gated waves and conditional Layers | Waves stop and roll back on CI failure deltas, policy-deny spikes and agent error rates. Layers can target repository tags such as `pci`, as Cedar context. | Reaching 100% in under an hour while waiting for health signals. |
| B4 | Secrets and PII scan at push | Scan before a change lands. Results become Cedar context alongside required checks. | False positives blocking urgent fixes. |
| B5 | Decision telemetry and read logging | OpenTelemetry events for policy decisions, Layer fetches, approvals and break-glass, each with its ledger ID. Agent bulk reads are logged as one record per session. | Volume and cost of read events. |

## Wave C: scale and proof (general availability)

| # | Item | Design change | Biggest design risk |
| --- | --- | --- | --- |
| C1 | Server-side merge and batched landing | Conflicts recorded in history. Changes land on protected refs in batches; each keeps its own approval binding. | A failed batch must not void unrelated approvals. |
| C2 | Path-level permissions | Cedar on path prefixes inside a repository. | Evaluation cost on large trees. |
| C3 | Evidence attachments | Agent session logs, plans and test runs stored as annotations and carried in the export sidecar. | Size and retention of logs. |
| C4 | Formal and differential verification | A written model of ref compare-and-swap, ledger chaining and fenced failover. The Git test suite against the bridge. Start the model during wave A. | The model drifting from the build. |

## What stays the same, on purpose

- **Branches stay.** Some labs discourage refs for scale. Regulated teams need release and hotfix branches.
- **Tests run before landing.** Post-merge testing only is not acceptable to a bank. Batched landing (C1) is the middle path.
- **No servers.** No always-on cache fleet. Segments and compaction (A6) address small-object latency.

## Risks this track adds

- **A Layer is an attack path, not only an outage risk.** A bad Layer could push a hostile hook or a broad allow-list to every agent. Mitigation: A4, waves and the class kill switch.
- **Self-reported provenance can be faked.** Mitigation: A3 takes it from the server session and signs it.
- **Approval fatigue.** Four-eyes on every agent change turns review into rubber-stamping. Mitigation: risk tiers in A2.
- **Landing on protected refs becomes the bottleneck**, not pushes. Mitigation: C1.

## Proposed measures

- Any line traced to its agent, sponsor, model, Layer versions and approver in under 1 minute, without engineering help.
- An agent class disabled in every repository in under 5 minutes.
- 100% of agent changes carry a complete signed provenance record.
- Supported clients never load a Layer bundle without verifying its digest.

## Open questions

- How much of a prompt does provenance keep: a digest only, or the full text under retention and erasure?
- Adopt the Agent Trace line-attribution format as is, or a superset with Layer digests and signatures?
- What segment size and compaction policy fit the 15-minute anchor and Object Lock?
- Which agent runtimes will send model and session metadata, and through what: OpenTelemetry, session logs or hooks?
- Can a wave reach 100% in under an hour and still wait for health signals?

## Sources

Vendor numbers in these are self-reported. ASPEN is pre-alpha.

- [ASPEN](https://github.com/reason-machines/aspen)
- [OpenAI: Scaling Git at OpenAI](https://openai.com/index/scaling-git-at-openai/) and [Running Codex safely](https://openai.com/index/running-codex-safely/)
- [GitHub: Building Git infrastructure for agent-scale development](https://github.blog/engineering/architecture-optimization/building-git-infrastructure-for-agent-scale-development/) and [Copilot cloud agent signs its commits](https://github.blog/changelog/2026-04-03-copilot-cloud-agent-signs-its-commits/)
- [Microsoft Entra Agent ID](https://learn.microsoft.com/en-us/entra/agent-id/security-for-ai-overview)
- [Anthropic: Redesigning source control](https://anthropic.com/engineering/redesigning-source-control) and [How we contain Claude](https://www.anthropic.com/engineering/how-we-contain-claude)
- [Google: How is Google using AI for internal code migrations?](https://arxiv.org/pdf/2501.06972)
