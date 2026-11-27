# JSON output (`--json`, `--output-json`) — the scripting contract

One page: which commands emit machine-readable JSON, what the output
promises, and what a script may rely on.

The read commands take `--json`. `data ingest` and `data delete` take
`--output-json`; `data list` accepts both. Below, "JSON mode" means either.

## Which commands emit JSON

| Command | Success statuses | Notes |
|---|---|---|
| `version` | — (plain payload, no `status` field) | `version`, `git_sha`, `build_date`, `go_version`, `platform` |
| `auth status --json` | `signed_in` · `unconfirmed` · `signed_out` · `expired` · `expired_or_revoked` · `unknown_platform` · `retired_platform` | `platform`, `account`, `secure_environment`, `expires_at`, `note` (a line the human screen prints under the rows, or `""`), `next` (the command that signs you in again, or `""`), `exit_code`. Unlike the error object, this object is written on a non-zero exit too: `expired`, `expired_or_revoked`, `unknown_platform` and `retired_platform` exit `1` and say so in `exit_code`. `--check` answers with its exit code alone and refuses `--json` |
| `client status --json` | — (a reading, no `status` field) | `name`, `state` (`online` · `offline` · `pending` · `unknown`), `last_seen` (RFC 3339, or `""`), `cluster` with `readiness` (`ready` · `starting` · `not_ready` · `no_release` · `not_reached` · `unknown`), `failing` and `not_running`. Reads once: `--wait` and `--seal` refuse `--json`. No secure environment on this machine is the error object, exit `1` |
| `account list --json`, `account current --json` | — (a listing, no `status` field) | `accounts` (each `id`, `name`, `kind` (`personal` · `organisation`), `role`, `is_default`, `chosen`), `working_in_id`, `chosen_id`, `default_id`, `stale_choice_id` (an ID is `null` when there is none; `working_in_id` is `null` while `stale_choice_id` is set, since the choice names an account you no longer have). Both commands write the same object |
| `machine --json` | — (no `status` field) | `machine_name`, `provider` (`""` when not set). No secure environment on this machine is the error object |
| `data ingest` | `succeeded` · `dry-run` · `detached` · `completed_with_failures` · `failed` · `unknown` · `auth_error` · `submit_error` · `watch_error` | result includes `name` (the dataset name you asked for, on every status), `table` (the table that was created, which can differ from `name`; empty on the error object, where none was created) and the ingest summary (row counts, success rate) when one was produced |
| `data list` | — (a listing, no `status` field) | `namespace`, `release`, `count`, `datasets` (names, unchanged), `details` (per-dataset objects: `name`, `task` (real ingest task; omitted for datasets ingested before task-persistence), `modality`, `intent`, `data_mark` (what the dataset holds: `patient_data`, `open_data` or `synthetic_data`; omitted when tracebloc could not be asked), `records`, `classes`, `format`, `size_bytes`, `ingested`) |
| `data delete` | `deleted` · `dry-run` · `declined` | result includes `database`, `table` (the case-resolved spelling), `pvc_paths`, `removed_paths`, `platform_notified` (tracebloc accepted the dataset report sent after the delete; `false` means My Data shows the dataset as Deleted only after the secure environment's next scheduled report). Never prompts — pass `--yes` (or `--dry-run`) |

Not covered: `doctor`, `resources` and `cluster info`. `version` keeps
its own `--output-json`.

## The contract

1. **stdout carries exactly one JSON object per run — nothing else.**
   All human-facing output (banners, progress, hints) goes to stderr in
   JSON mode. `… --json | jq .` always works.
2. **Exit codes are in lockstep and unchanged.** JSON mode never
   alters a command's documented exit codes; the JSON is additive.
   A non-zero exit comes with `status: "error"`, except for `data ingest`'s
   own statuses (`failed`, `detached`, …) and `auth status`'s. Every
   `data ingest` and `auth status` object
   carries `exit_code`, `0` included, and so does the error object below
   on every command; it always equals the process exit code. `failed`
   exits `9`. `detached` exits `0` after `--detach` or Ctrl-C, and `11`
   when the CLI stopped following the run on its own, so `exit_code` is
   how a script tells the two apart.
3. **Failures still emit JSON.** Any failure — a flag the command
   doesn't take, a wrong number of arguments, or a failure before or
   after the command started doing work — writes the error object below,
   so a parser never sees empty stdout. That holds even when the bad flag
   comes before `--json` on the command line.
4. **Safe endings are exit 0 — branch on `status`.** A dry run, a
   declined confirmation, and a real deletion all exit 0 (matching the
   human flow); the `status` field is what distinguishes them. Scripts
   that need "it actually happened" must check `status`, not just the
   exit code.
5. **Arrays are never `null`.** Empty lists marshal as `[]`
   (`datasets`, `details`, `pvc_paths`, `removed_paths`), so indexing is safe.
6. **JSON mode implies non-interactive.** Commands never prompt
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
`dataListJSON`, `dataDeleteJSON`, `authStatusJSON`, `clientStatusJSON`,
`accountsJSON`, `machineJSON`) — internal types stay JSON-tag-free
so this wire format can evolve deliberately.
