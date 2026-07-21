# Terminal output style

The `tracebloc` CLI and the installer share **one** terminal style system. This
is the reference; `scripts/check-style.sh` enforces the mechanical parts in CI.

## The idea

From the tracebloc.io homepage gradient — **cyan orients, lime moves**:

- **cyan** `#01a5cc` = *structure* — headings, step titles, links, "where you are"
- **lime** `#91e947` = *action* — commands, the primary CTA, "what to do next"

Everything else stays quiet (dim neutral). Colour is never load-bearing: headings
and commands also carry **bold**, and alerts carry a distinct **glyph**, so the
output still reads under `NO_COLOR`, in a pipe, or for a colour-blind reader.

## Roles → tones

All colour goes through the `Printer` in `internal/ui` — never inline an escape
or hex elsewhere. The tone table (`internal/ui/ui.go`) maps each role:

| Role | Printer method / tone | Colour | Weight / glyph |
|------|-----------------------|--------|----------------|
| Heading / section / step | `Section`, `Step`, `Banner` (`toneHeading`) | cyan `#01a5cc` | bold |
| Command (to run) | `Command`, `MenuRow` cmd (`toneCommand`) | lime `#91e947` | bold |
| Description / supporting | `MenuRow` desc (`toneDesc`) | soft lime `#a7ed6c` | — |
| Link / URL | `toneLink` | cyan `#01a5cc` | underline |
| Success ✔ / online ● | `Successf`, `CheckLine` (`toneGo`) | lime `#91e947` | glyph |
| Warning ⚠ | `Warnf`, `WarnLine` (`toneWarn`) | amber `#ffc62b` | glyph |
| Error ✖ | `Errorf` (`toneErr`) | red `#f64c4c` | bold glyph |
| Label : value | `Field`, `Stat` (`toneLabel`) | dim neutral | — |

**Emoji are welcome** — used with intent, for warmth (👋 greeting, 💚 sign-off,
🚀 sent) and for status (🟢 online, 🟡 starting, 🔴 offline, ⚠ caution). They're a
brand touch, not policed by the guard — just don't overuse them.

The engine renders exact 24-bit hex on truecolor terminals, the **deep shade**
(`#01637a` / `#578c2b`) on light backgrounds, the nearest ANSI-16 otherwise, and
nothing when colour is off (`NO_COLOR` / non-TTY / `TERM=dumb` / `--plain`). The
exact brand SGR is pinned by `internal/ui/brand_tones_test.go`, so a drift in the
tone table fails CI. The installer mirrors this in `scripts/lib/common.sh`.

## Terminology

Source of truth: the docs repo `TERMINOLOGY.md`. In user-facing output:

| Use | Not |
|-----|-----|
| secure environment | workspace, hub, client (as a noun for the environment) |
| ingest | upload, import |
| delete | remove, uninstall (for offboarding) |
| Online / Offline | connected / disconnected, up / down |
| collaborators | users, members |
| task | job, experiment type |

`client` stays valid as the CLI verb (`tracebloc client create`) and in code
identifiers (`exitNoWorkspace`, etc.) — the guard matches `workspace` as a whole
word in output text only.

## What's enforced vs reviewed

`scripts/check-style.sh` (CI Lint job, blocking) catches the **mechanical**
violations: hardcoded brand colour outside `internal/ui`, and `workspace` in
user-facing text. Run it locally with `make check-style` (also part
of `make ci`) or directly: `bash scripts/check-style.sh`.

It can't police **judgement** — using the right *role* for a token (a command in
the command tone, not the heading tone), or the softer terminology calls. Those
stay with review; `STYLE.md` and `scripts/check-style.sh` are CODEOWNER-gated so
the rules can't be quietly weakened.

To intentionally exempt a line, append `// style-guard: allow` with a reason.
