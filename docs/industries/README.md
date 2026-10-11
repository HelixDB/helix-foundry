# Industry playbooks

A playbook is Foundry's opinion of how to read one industry's data: what matters, what must be tracked, how the data should be linked, and what the owner should do next to understand their customers better. It is a starting point, not a fixed template. Each workspace keeps its own profile that starts from the playbook and keeps evolving with the questions people ask.

Playbooks live in this folder, one markdown file per industry, about 3–4 pages each. [ecommerce.md](ecommerce.md) is the first. Adding an industry means adding a file in this format; no code changes are needed unless the industry needs a new kind of check.

## Architecture

Three layers, each with one owner and one job.

```text
 Playbook (this folder)          Workspace profile (stored)           Recommendations (computed)
 ─────────────────────           ──────────────────────────           ──────────────────────────
 versioned with the code    ──▶  industry, branch, role,         ──▶  ranked list for the
 opinion: areas, roles,          concept mapping, definitions,        Ontology section, Home,
 metrics, checks, advice,        interests learned from questions,    Analyst and #53 findings
 insight rules, glossary         gaps, dismissals, notes
```

1. **Playbook: the opinion.** Read-only, shipped with the app, reviewed like code. It never contains a workspace's data. The frontmatter is what code reads; the prose is what people review and what the model reads, one branch at a time.
2. **Workspace profile: what is true for this business.** One stored resource per workspace (`industry-profile`), plus a small per-person part for role and interests. It records:
   - the detected and confirmed industry, business-model branch and stage;
   - the **concept mapping**: which entity and column plays each playbook role (order, customer, amount, order date, traffic source, ...), with how it was found (detected, AI, or confirmed by the owner);
   - **definitions** the owner confirmed or corrected, for example "an order counts when it is completed, not when it is placed";
   - **interests** learned from questions (see below);
   - **gaps** found from questions that could not be answered;
   - dismissed and completed recommendations, and the playbook version it was built from.
3. **Recommendations: what to do next.** A pure function of playbook × profile × data checks. Nothing is stored except the last result, cached by publication generation and profile revision. Every recommendation is one of four kinds:
   - **Connect** a source that fills a missing area;
   - **Change your system** to capture data nobody records yet;
   - **Fix the data** already here (links, duplicates, definitions);
   - **Ask** a question the data can now answer well.

Detection, mapping and checks are deterministic, so the whole thing works with no AI. AI only confirms detection, proposes mappings the rules missed, and classifies questions.

## How the profile evolves

The playbook is fixed; the profile is not. It changes from four kinds of evidence:

| Evidence | What changes |
| --- | --- |
| **Questions asked** in the Analyst or by voice | Each question is classified into playbook areas and metrics. Areas asked about often rise in priority for that person, and the metrics they keep asking about move up on Home. |
| **Questions that could not be answered well** (missing table, missing column, broken link, low confidence) | Recorded as a gap with the question as evidence. Repeated gaps become **Connect** or **Change your system** recommendations quoting the questions: "You asked about ad spend 4 times this month." |
| **Corrections** in answers or the Ontology section ("returns aren't sales") | Saved as workspace definitions; every later metric, answer and finding uses them. |
| **New data** (a sync, a new source, a rebuilt ontology) | Mapping and checks re-run; recommendations it satisfied are marked done, and newly possible questions are suggested. |

Rules that keep this predictable:

- Learning never changes the playbook or a confirmed definition by itself. It only re-ranks, adds gaps, or proposes a definition for the owner to confirm.
- Interests decay: a question counts less over time, so the profile follows what the business cares about now.
- Interests are per person; definitions, mapping and gaps belong to the workspace.
- Question text is stored only as short evidence on a gap, kept for 90 days, and never sent anywhere new.
- The profile shows why each recommendation is there, and the owner can reset learned interests without losing definitions.

## File format

Each playbook is markdown with YAML frontmatter.

```yaml
---
id: ecommerce                # stable identifier, also the file name
title: Online retail
version: 1                   # bump when checks or metrics change meaning
signals:                     # how to recognise the industry
  require: [order, amount]   # roles that must map
  boost: [product, order_line, session, shipment, refund]
  against: [subscription_plan, patient, policy]
roles:                       # the concepts the playbook talks about
  order: { entity: true, names: [order, purchase, transaction, sale] }
  amount: { of: order, type: number, names: [net_amount, total, amount, grand_total] }
areas:                       # business areas, in default priority order
  - id: revenue
    metrics: [net_revenue, orders, aov]
metrics:                     # defined by role, never by column
  aov: { title: Average order value, value: "sum(order.amount) / count(order)", where: completed }
checks:                      # coverage and data-health checks
  - { id: orders_link_customers, kind: link, from: order, to: customer, good: 0.95 }
advice:                      # recommendations, tied to the gap they close
  - { id: utm, kind: change, when: "missing session.campaign", area: marketing }
branches:                    # personalisation
  model: [direct, multi_brand, marketplace, subscription]
  role: [founder, operations, marketing, finance]
---
```

The prose follows the same sections in the same order, so playbooks are easy to compare:

1. Signals
2. Concepts and how to map them
3. Business areas and what matters
4. Tracking coverage
5. Data health
6. How to read the numbers
7. What each person needs
8. What to change in your own systems
9. Insight rules
10. Glossary
11. Branches

## Adding an industry

1. Copy [ecommerce.md](ecommerce.md) and keep the section order.
2. Reuse role names where the meaning is the same (`customer`, `amount`, `created_at`), so shared checks and the Analyst glossary keep working.
3. Give every metric, check and piece of advice an `id`; never reuse an `id` for a different meaning. Bump `version` when a meaning changes, so profiles built on the old version are re-checked.
4. Add a fixture workspace that is detected as the new industry, and one that must not be.
