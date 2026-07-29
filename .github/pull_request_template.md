## Summary
<!-- 1–3 sentences. What does this PR do and why? -->

## Related
<!-- Same repo: Closes #123 · Cross-repo: Fixes tracebloc/backend#456 (owner-qualified — a bare backend#456 closes nothing). PRs land on develop, not the default branch, so confirm the issue actually closed. -->

## Type of change
- [ ] Feature
- [ ] Bug fix
- [ ] Tech-debt / refactor
- [ ] Docs
- [ ] Security / hardening

## Test plan
<!-- Commands run, manual steps. -->

## Checklist
- [ ] Tests added / updated and passing locally
- [ ] `go build ./...`, `go vet`, and the Lint job's checks pass locally
- [ ] Terminal output follows [STYLE.md](../STYLE.md) — Printer tones (no hardcoded colour/emoji), "secure environment" not "workspace"; `bash scripts/check-style.sh` passes
- [ ] No secrets / credentials in the diff
- [ ] Cross-repo issues use `Fixes tracebloc/<repo>#N` — a bare `repo#N` closes nothing
