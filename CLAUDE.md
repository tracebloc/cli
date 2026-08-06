# CLAUDE.md

Repo-specific guidance for Claude Code sessions goes here, above the
managed org block: build/test commands, architecture notes, gotchas,
and this repo's default reviewer. Rollout: tracebloc/backend#1602.

<!-- org-standards:begin -->
## tracebloc engineering standards (org-wide)

<!-- Canonical source: tracebloc/.github/org-standards.md.
     Synced into every repo's CLAUDE.md between org-standards markers — never
     edit it inside a consuming repo; open a PR against tracebloc/.github.
     Meta-rule: the moment a rule below becomes mechanically enforced (a lint
     rule, a house-rules grep, a required check), delete the sentence here and
     let the check carry it. Prose is only for what tooling can't judge. -->

### Branches & PRs

- Branch model: `develop → staging → main`. Branch off `develop`; every PR targets `develop`. Never open PRs to `staging` or `main` — promotions are the release train's job. (Sole exception: the `docs` repo may target `main`.)
- Before starting any task: `git fetch` and branch from the current tip of `develop` — never build on a stale checkout. A branch that lives more than a day gets `develop` merged back in before review. We move fast; stale starts mean silent divergence and duplicated work.
- One self-contained change per PR. A few hundred changed lines reviews well; at 1000+ split it. Refactors ship in separate PRs from behavior changes.
- Branches are short-lived (aim to merge within a day or two), single-author, and based on `develop` — no stacked PRs on top of other open PRs.
- Names and commits: `feat/ fix/ docs/ sec/ ci/ chore/` + issue number + short slug (`fix/1234-ingest-timeout`); commit subjects `type(scope): summary`, referencing the ticket (`backend#1234`).
- When you open a PR: assign yourself and request exactly one reviewer immediately — a PR without a reviewer stalls by construction. You pick the reviewer: whoever knows the code best. There is no per-repo default, and no automation assigns one — branch protection just refuses to merge without a review.
- When you are the reviewer: first response within one business day.

### Quality bar

- Before every push: run the linter and the tests that cover your change. Never push a branch you believe is red — CI is the backstop, not the first run.
- Read the full diff before opening the PR. You own every line you ship, whoever — or whatever — wrote it.
- AI sessions end with evidence, not assertion: run the relevant check (tests, build, lint) and show the output. A change that could not be verified does not ship.
- After opening or pushing to a PR, stay on it: poll CI and Bugbot on the current head and triage every finding the same day — fix it, or reply on the thread saying why not. No silent dismissals. Unresolved threads block the merge and stall the release train's settle stage; cheap now beats expensive later.
- A finding that recurs across PRs becomes a rule: add it to `.cursor/BUGBOT.md`, and if it is grep-expressible, to code-quality's house-rules — then stop re-arguing it in comments.
- Style and naming rules live in tooling (black/ruff, eslint/prettier, house-rules), never in prose. If a rule matters, encode it; do not restate linter rules in CLAUDE.md files.
- Never commit secrets, tokens, or customer data — not in code, config, tests, issues, or commit messages. gitleaks and the PII gate will catch it; don't make them.

### Engineer kanban

- Picking up work: the team coordinates. `Ready` is the refined queue and the first choice when it's stocked; pulling from `Backlog` is normal when refinement hasn't caught up — say what you're taking.
- Merging to `develop` moves the card to `On dev` automatically; there is no dev-side review.
- Functional review happens once, on staging: when it passes, comment `/fr-pass` on the PR or drag the card to `Ready for prod`. Self-signoff is allowed.
- `fr-gate` is a required check on promotions. If it blocks, the board or the work isn't ready — fix that. `skip-fr-gate` is audited, for emergencies only.

### Releases & publishing

- The release train is the only path to `staging`, `main`, and every package registry. Never hand-cut a `v*` tag, hand-bump a version file, or publish an artifact — every legal publish path is inventoried in release-train's `PUBLISH-PATHS.md`.
- Findings on a promotion PR are fixed on the source branch (`develop`/`staging`), then the train re-prepares. Never push fixes onto a promotion PR — every push re-rolls its review.

### Filing issues

- Internal work — planning, epics, security findings, infrastructure, anything mentioning a customer — is filed in `backend` (the private catch-all), never in a public repo. When in doubt: `backend`.
- Public repos (`cli`, `client`, `docs`, `data-ingestors`, `model-zoo`, `start-training`, `.github`) only get issues a stranger could act on: about the public artifact itself, with no customer names, internal URLs, or internal paths.

### AI-assisted sessions (Claude Code, etc.)

- An AI session may open PRs and push its own branches. It never: merges a PR, closes another person's PR, deletes another person's branch, or force-pushes — each of those needs an explicit instruction from the human running it.
- If your change makes a statement in any CLAUDE.md, BUGBOT.md, or runbook false, update that file in the same PR.
<!-- org-standards:end -->
