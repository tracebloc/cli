## Summary
<!-- 1–3 sentences. What does this PR do and why? -->

## Related
<!-- Same repo: Closes #123 · Cross-repo: Fixes tracebloc/client-runtime#456 (owner-qualified — a bare client-runtime#456 closes nothing). PRs land on develop, not the default branch, so closing keywords do not fire on merge — confirm the issue actually closed. -->

## Type of change
- [ ] Feature
- [ ] Bug fix
- [ ] Tech-debt / refactor
- [ ] Docs
- [ ] Security / hardening
- [ ] Breaking change

## Test plan
<!-- What did you test? Commands run? Manual steps? -->

## Screenshots / recordings
<!-- For UI changes. Remove if N/A. -->

## Deployment notes
<!-- Env vars, migrations, rollout order, feature flags. Remove if N/A. -->

## Checklist
- [ ] Tests added / updated and passing locally
- [ ] Docs updated if behavior or config changed
- [ ] No secrets / credentials in the diff
- [ ] For security-sensitive paths: appropriate reviewer requested
- [ ] Cross-repo issues use `Fixes tracebloc/<repo>#N` — a bare `repo#N` closes nothing
- [ ] If this depends on a change in another repo: shipped **expand-then-contract** (additive first, consumers adopt later), or **Breaking change** ticked above with the rollout order in *Deployment notes* — repos promote independently, so the other change may not ship with this one
- [ ] `go build ./...`, `go vet`, and the Lint job's checks pass locally
- [ ] Terminal output follows [STYLE.md](../STYLE.md) — Printer tones (no hardcoded colour/emoji), "secure environment" not "workspace"; `bash scripts/check-style.sh` passes
