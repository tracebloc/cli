# JSON output (`--output-json`) — the scripting contract

One page: which commands emit machine-readable JSON, what the output
promises, and what a script may rely on.

## Which commands emit JSON

| Command | Success statuses | Notes |
|---|---|---|
| `version` | — (plain payload, no `status` field) | `version`, `git_sha`, `build_date`, `go_version`, `platform` |
| `data ingest` | `succeeded` · `dry-run` · `detached` · `completed_with_failures` · `failed` · `unknown` · `auth_error` · `submit_error` · `watch_error` | result includes the ingest summary (row counts, success rate) when one was produced |
| `data list` | — (a listing, no `status` field) | `namespace`, `release`, `count`, `datasets` (names, unchanged), `details` (per-dataset objects: `name`, `task` (real ingest task; omitted for datasets ingested before task-persistence), `modality`, `intent`, `records`, `classes`, `format`, `size_bytes`, `ingested`) |
| `data delete` | `deleted` · `dry-run` · `declined` | result includes `database`, `table` (the case-resolved spelling), `pvc_paths`, `removed_paths`. Never prompts — pass `--yes` (or `--dry-run`) |

Not covered (yet): `doctor`, `resources`, `auth status` — extending
`--output-json` to the read-only diagnostics is deferred pending the
epic's OQ5 decision. `auth status --check` is exit-code-only by design.

## The contract

1. **stdout carries exactly one JSON object per run — nothing else.**
   All human-facing output (banners, progress, hints) goes to stderr in
   `--output-json` mode. `… --output-json | jq .` always works.
2. **Exit codes are in lockstep and unchanged.** `--output-json` never
   alters a command's documented exit codes; the JSON is additive.
   A non-zero exit always comes with `status: "error"`, and the
   `exit_code` field always equals the process exit code.
3. **Failures still emit JSON.** Any failure — before or after the
   command started doing work — writes the error object below, so a
   parser never sees empty stdout.
4. **Safe endings are exit 0 — branch on `status`.** A dry run, a
   declined confirmation, and a real deletion all exit 0 (matching the
   human flow); the `status` field is what distinguishes them. Scripts
   that need "it actually happened" must check `status`, not just the
   exit code.
5. **Arrays are never `null`.** Empty lists marshal as `[]`
   (`datasets`, `details`, `pvc_paths`, `removed_paths`), so indexing is safe.
6. **`--output-json` implies non-interactive.** Commands never prompt
   in JSON mode: `data ingest` treats it as `--no-input`; `data delete`
   requires an explicit `--yes` (or `--dry-run`) and otherwise fails
   closed (exit 3).

## The error shape

Identical across every JSON-emitting command:

```json
{
  "status": "error",
  "error": "<human-readable message>",
  "exit_code": 7
}
```

## Stability promise

- **Additive evolution only.** New fields may appear in any release;
  existing fields are not renamed, removed, or re-typed. Parse
  tolerantly (ignore unknown fields).
- **Status vocabularies may grow.** Treat an unrecognized `status` as
  "not the success you were looking for", not as an error in your
  parser.
- **Formatting is not part of the contract.** Output is currently
  indented JSON; scripts must parse it as JSON, not scrape lines.
- **Breaking changes** (renaming/removing a field, changing a type,
  repurposing a status) require a major version bump and will be called
  out in the release notes.

The shapes are owned by the CLI presentation layer
(`internal/cli/*.go`: `versionPayload`, `pushJSONResult`,
`dataListJSON`, `dataDeleteJSON`) — internal types stay JSON-tag-free
so this wire format can evolve deliberately.
