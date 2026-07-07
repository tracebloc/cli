# RFC 0002 — `tracebloc data ingest`: flow, terminology & task taxonomy

> **Status: DRAFT — for discussion.** Owner: @LukasWodka. Last updated: 2026-07-07.
>
> This RFC captures the redesign of the `tracebloc data ingest` user
> experience: how the user is guided through an ingest, the words we use,
> how the ML task is chosen, and the contract that keeps the task list
> consistent across the CLI, the ingestor, and the backend. It is grounded
> in a code-level audit of the shipped command (CLI `develop`, the
> data-ingestors ingestor, and the backend), plus data-scientist-facing
> vocabulary research (Hugging Face, scikit-learn, Papers With Code).
>
> Related shipped work: the destination guard + extension handling
> ([cli#149](https://github.com/tracebloc/cli/pull/149)), preflight parity
> ([cli#150](https://github.com/tracebloc/cli/pull/150)), the staging-copy
> reclaim ([cli#167](https://github.com/tracebloc/cli/pull/167)), and the
> "honest waits" pass ([cli#173](https://github.com/tracebloc/cli/pull/173)).
> This RFC supersedes the copy in #173 where they conflict (see §6).

## 1. Summary

`tracebloc data ingest` works, but it is shaped around the machine, not the
user. It asks for the **task first** (a 16-item wall) before it knows
anything about the data; it requires a **directory** (never a file); it
speaks Kubernetes ("stage Pod", "port-forward", "Job"), which contradicts
the product's whole premise; and the word **upload** frames the data as
leaving the user's control when it never does.

This RFC proposes to **invert the flow** — ask for the data first, detect
what it is, then offer only the tasks that fit it — and to make the whole
run speak the language of a data scientist whose data never leaves their
own infrastructure. It also fixes the taxonomy **contract**: the list of ML
tasks currently lives, hand-maintained, in five places across three repos.

## 2. Principles

1. **The data never leaves the user's infrastructure.** Copy never says
   "upload", "push", "send", or "transfer to us". The umbrella verb is
   **ingest**; the byte movement (laptop → the user's *own* cluster) is
   **"copy into your workspace's storage"**. Reassure at the moment of
   movement that it stays on their infrastructure and that collaborators
   train on it without seeing the raw files.
2. **These are ML tasks, not "categories."** Use **task** everywhere the
   user sees it.
3. **One environment.** One machine = one client = one cluster
   (RFC-0001 §15). The user never chose a cluster/namespace/PVC and cannot
   act on them — so they are ceremony and must not appear in the happy path.
4. **Ask the least.** Infer what the data can tell us (the task *family*);
   only ask for genuine modeling intent (the *task* within a family) and
   dataset facts we cannot see.
5. **One-liners.** Every task, flag, and prompt carries a plain one-line
   gloss of what it means / predicts.

## 3. Current state (as shipped on `develop`)

- **Input is always a directory.** `Discover` / `DiscoverTabular` /
  `DiscoverText` / `DiscoverObjectDetection` each reject a non-directory
  ("… is not a directory"). Even the tabular family — whose dataset *is* a
  single CSV — requires a *folder containing exactly one `.csv`*. The user
  cannot pass a file.
- **Task is asked first and defaults silently.** `--category` defaults to
  `image_classification`; the interactive picker shows the tasks before any
  data is inspected. A tabular user who forgets `--category` gets an
  *image*-flavored error.
- **Destination table** is a required, explicit `--table` — no default.
- **Four equal "Step N/4" steps** present one user step (check dataset) and
  three system steps (connect / stage / run) as peers. Step 2 "Connect to
  your workspace" prints `cluster`, `chart`, `PVC`, `Bound` — all ceremony.
- **Copy uses egress framing** — the banner opens "This uploads a
  dataset … Your files are sent to the Kubernetes cluster"; the staging
  lines say "upload channel" / "Uploading". `stage`/`Job`/`port-forward`
  leak throughout.
- **Task taxonomy is duplicated in five places** (see §9) with only one
  enforced edge.
- **Label handling is task-specific but only partially:** the CLI skips the
  label column solely for `masked_language_modeling`.

## 4. Input format — flexible path, not folder-only (answers "what do we ask for?")

Today the answer is "always a folder." That is wrong for the tabular family
and surprising everywhere. Proposed:

**Accept either a file or a directory**, and let the shape tell us the family:

| The user points at… | We read it as | Family |
|---|---|---|
| a single `.csv` file | the dataset itself | tabular / time series |
| a directory with one `.csv`, no media | the dataset CSV | tabular / time series |
| a directory with `images/` (+ optional `annotations/`, `masks/`) | image dataset | image |
| a directory with `texts/` or `sequences/` (+ optional `labels.csv`, `tokenizer.json`) | text dataset | text |

Media/label datasets (image, detection, keypoint, segmentation, supervised
text) are inherently multi-file and remain **directories** — you cannot
ingest one JPEG as a "dataset" because the labels and siblings live
alongside it. The only family where a bare **file** is natural is
tabular/time (one CSV), and that is exactly the case that fails today. So
"flexible path" concretely means: **accept a `.csv` file directly; otherwise
expect a directory**, and always say which in the error.

**The user's mental model (this is the whole point):** the user points at
**their data, in whatever shape it already has — they never wrap or
rearrange it.** If their dataset is one table, that's a CSV file; if it's a
collection (images/texts + labels), that's the folder holding them. A data
scientist already knows which their data is; we don't make them learn our
convention. Then — critically — the tool **echoes what it found** before
going further:

```
? Where is your data? (file or folder)   ~/data/churn.csv
  ✔ Found a CSV table — this is tabular data.
```
```
? Where is your data? (file or folder)   ~/data/cats-and-dogs/
  ✔ Found 1,240 images + labels.csv — this is an image dataset.
```

That echo line is where the "file or folder?" confusion is removed: the user
points at their data and the tool confirms it understood, rather than making
the user reason about our layout rules up front.

The full per-family layout is in Appendix A (§10).

## 5. The flow — inverted: data first, then task

The current order (task → data) forces the user to choose among 16 tasks
before the tool knows anything about their data. We invert it:

### Phase 1 — Your dataset (the only place the user decides anything)

Order (settled with @LukasWodka): commit to *what* you're creating, then
point at *where* it is. The same four questions run for every data type:

1. **Split** — "Is this training or test data?" (`--split train|test`,
   default `train`; renamed from `--intent`).
2. **Name it** — the dataset's identifier: how you'll refer to it when you
   start a training run (`--name`; renamed from `--table`, the wire field
   stays `table`). It is **not** the same as the split — but the convention
   is to bake the split into the name, e.g. `churn_train` / `chest_xrays_test`
   (the code's own examples). Because the name is asked before the path, it
   is a deliberate prompt with an example placeholder, not an
   auto-filled-from-the-path default — a small, accepted friction trade for
   the clearer "name it, then point at it" model.
3. **Point at the data** — "Where is your data? (file or folder)" — a file
   or a directory (§4). The one unavoidable input. The tool immediately
   **detects the family and echoes it** ("✔ Found a CSV table — this is
   tabular data"). Where the layout uniquely pins a task (`annotations/` ⇒
   object detection, `sequences/`+`tokenizer.json` ⇒ masked language
   modeling) it pre-selects that task. **The sniff is a hint, never a
   lock** — see §5.1.
4. **Pick the task** — show only the tasks in the detected family, each as
   `Display name — one-liner · task_id`, split into **Available now** and
   **Not yet in the CLI** (greyed, with the reason). Never the flat 16-item
   wall. (§7 for the taxonomy.)
5. **Task-specific questions, only when the task needs them** (§8): the
   **label column lives here, not as a core prompt** — it is asked only for
   the tasks that use one, worded per task ("the class label" vs "the value
   to predict"), and skipped entirely for the self-supervised tasks.
   Likewise `--number-of-keypoints` (keypoint), `--time-column` (survival).
6. **Review + confirm** — a single "Proceed with the ingest?" gate.

### Phase 2 — Ingesting (progress only; zero decisions, zero Kubernetes)

- Check your data (the local preflight parity checks).
- **Copy into your workspace's storage** — *stays on your infrastructure*
  (progress bar).
- **Validate + load on your cluster** — a live spinner during the
  otherwise-silent pod-start, then streamed progress.
- **Done** — "Ingested N records into `<name>` — ready for training," with
  the dashboard link.

Everything else — connecting, discovering storage, the staging pod, the SA
token, the port-forward, the job watch, the staging reclaim — is **silent
unless `--verbose`**. The "Step 2/4 Connect to your workspace" screen and
its `cluster/chart/PVC/Bound` fields are removed.

### 5.1 What if the folder is ambiguous or has mixed formats?

The sniff only **pre-selects a default in the interactive picker**; it never
locks the choice and correctness never depends on it:

- Conflicting signals (e.g. both `images/` and a stray `.csv`) ⇒ no
  pre-selection; ask the family plainly.
- `--task` on the command line always wins over the sniff.
- The chosen task's `Discover` still validates the layout and errors
  clearly if it doesn't match. So a wrong guess degrades to "ask" or "a
  clear error," never "silently wrong."
- Non-interactive runs don't sniff — `--task` is required there.

## 6. Terminology map

| Today (banned) | Proposed |
|---|---|
| "This **uploads** a dataset … files are **sent to** the cluster" | "This **ingests** a dataset onto your own cluster — it never leaves your infrastructure." |
| "Opened a secure **upload channel**" | "Opened a secure channel into your cluster's own storage." |
| "**Uploading** N files" / "**Uploaded** N files" | "**Copying** N files into your storage" / "**Copied** N files — all on your own cluster." |
| "**Stage** your files" (step title) | "Copy your files into your workspace." |
| progress bar "**Staging** `<table>`" | "Copying `<table>` into your storage." |
| "**Opening port-forward to jobs-manager**" | "Connecting to your workspace to submit the run." |
| `--category` / "category" | `--task` / "task" |
| `push` command alias | removed (no legacy `push` to deprecate). |
| `stage Pod`, `Job`, `PVC`, `namespace`, `kubectl` in happy-path copy | removed; `--verbose` only. |

`stage`/`staging` remain valid **in code**; they must not appear in user
copy. The `kubectl logs` reconnect hint stays *only* on the detach/error
path, as a labelled optional follow, until a `tracebloc data logs`/`status`
re-attach verb exists (§8, open).

## 7. Task taxonomy (the 15 tasks, in DS language)

The task is **required, load-bearing platform metadata** — not CLI
over-asking. It is a stored field (`UserDataSet.category`) and the backend
branches on it in ≥8 places (model-zoo template directory, model↔dataset
compatibility gate, model-head `output_classes`, time-series feature
adjustment, min-labels guard, BIO-tag explosion, train/test compat skips,
tokenizer-fit). The coarser `data_format` the backend also gets is strictly
weaker (4 CV tasks all map to "image"). See §7.1 for what's inferable.

**Display rule:** lead with the name a data scientist would search for; keep
the exact `task_id` visible (it is the `--task` value and the model-zoo
directory name) and the one-liner alongside.

**Image family**

| `task_id` | Display | One-liner | CLI |
|---|---|---|---|
| `image_classification` | Image classification | Assigns one label to a whole image | ✅ now |
| `object_detection` | Object detection | Locates + classifies objects with boxes | ✅ now |
| `keypoint_detection` | Keypoint detection (pose estimation) | Locates landmark points on a body/object | ✅ now |
| `semantic_segmentation` | Semantic segmentation | Labels every pixel with a class | ⏳ CLI-pending |

**Text family**

| `task_id` | Display | One-liner | CLI |
|---|---|---|---|
| `text_classification` | Text classification (sentiment / topic / intent) | Assigns one label to a whole text | ✅ now |
| `masked_language_modeling` | Masked language modeling (fill-mask) | Predicts masked tokens to pretrain an encoder | ✅ now |
| `token_classification` | Token classification (NER / POS) | Labels each token in a sequence | ⏳ CLI-pending |
| `sentence_pair_classification` | Sentence-pair classification (NLI / paraphrase) | Classifies the relation between two sentences | ⏳ CLI-pending |
| `causal_language_modeling` | Causal language modeling (text generation) | Predicts the next token; autoregressive | ⏳ CLI-pending |
| `seq2seq` | Sequence-to-sequence (translation / summarization) | Maps an input sequence to an output sequence | ⏳ CLI-pending |
| `embeddings` | Text embeddings (contrastive) | Learns vector representations from pairs / triplets | ⏳ CLI-pending |

**Tabular / time-series family**

| `task_id` | Display | One-liner | CLI |
|---|---|---|---|
| `tabular_classification` | Tabular classification | Predicts a categorical label from table rows | ✅ now |
| `tabular_regression` | Tabular regression | Predicts a continuous value from table rows | ✅ now |
| `time_series_forecasting` | Time-series forecasting | Predicts future values from historical data | ✅ now |
| `time_to_event_prediction` | Survival analysis (time-to-event) | Predicts if and when an event will occur | ✅ now |

Three display names deliberately diverge from the raw id because that is
what a DS searches for: `time_to_event_prediction` → **Survival analysis**;
`masked_language_modeling` → **fill-mask**; `seq2seq` → **translation /
summarization**. The id stays the accepted value.

**"CLI-pending" ≠ unsupported.** The 5 text tasks marked ⏳ are
**already supported by the schema, the ingestor, and the backend** — the
only missing piece is the CLI's local layout discovery/staging for their
`texts/` file formats. §11 wires them so the CLI matches the platform.
(`semantic_segmentation` is blocked deeper, on ingestor mask-sidecar
support, data-ingestors#136; `instance_segmentation` is a registry-only
placeholder not in the schema — see §9.)

### 7.1 Inferable (family) vs. a real decision (task)

The **family** is recoverable from the layout (§4). The **task within a
family is not** and is genuine modeling intent:
- The 4 tabular/time tasks are byte-identical on disk (one CSV) — classify
  vs. regress vs. forecast vs. survival is the user's call.
- `image_classification` and `keypoint_detection` share `labels.csv +
  images/`, separated only by `--number-of-keypoints`.

So we sniff the family and only ask the task within it.

## 8. The label — wording, convention, and validation (highest-risk input)

The label is the per-example ground truth the model learns. Getting it
wrong is the most dangerous mistake in the whole flow: today the validators
match a label column *case-insensitively* while the ingestor's
`RecordProcessor` reads it **exactly**, so a mis-cased or whitespace-off
name passes validation and then **silently ingests a NULL label for every
row** (data-ingestors#340). So the label needs precise wording *and*
airtight validation. **Never say the ambiguous "label to predict."**

**Three shapes — say the right word:**
- **Class** (categorical) — classification tasks. Say **"class."**
- **Target / value to predict** (continuous) — the regression family
  (`tabular_regression`, `time_series_forecasting`,
  `time_to_event_prediction`). Say **"value to predict" / "target,"** never
  "class."
- **Absent from the CSV** — the label is carried in a *sidecar*
  (`object_detection` Pascal-VOC XML, `semantic_segmentation` PNG mask,
  `keypoint_detection` annotations), embedded *per-token*
  (`token_classification` BIO tags), or **does not exist** because the task
  is self-supervised (MLM, causal-LM, seq2seq, embeddings). For all of
  these we **do not ask for a label column at all.** (This drops the CLI's
  current vestigial `--label-column` for object detection / keypoint.)

**Convention:** the label is a **single column header** in the CSV
(`labels.csv` for image/text, the data CSV for tabular). The default is a
column literally named **`label`**; for survival the time column defaults
to **`time`**.

**Validation — pick from the real headers (this kills #340):** whenever a
label column is needed, read the CSV header row and have the user **pick
from the actual column names** — they cannot type a column that doesn't
exist or mis-case one. Default-select `label` if present.
- **Interactive:** a select over the real headers; a real column must be
  chosen.
- **Flag / non-interactive (`--label-column`):** validate the value against
  the real header set with an **exact (case- and whitespace-sensitive)
  match**. On no match, **hard-fail with a non-zero exit and print the
  available columns** — never fall through to a NULL-labeled ingest, never
  a partial write. The same rule applies on both paths; no loose matching
  anywhere.

**Policy (regression family only):** the target is emitted as
`{column, policy}` with `policy` defaulting to **`bucket`**, which hashes
the raw continuous value to a bucket id so the actual number never leaves
the on-prem cluster. `passthrough` is opt-in. Classification implicitly uses
passthrough (a no-op).

## 8.1 Task-specific questions — all 15 tasks

The exact per-task inputs (verified against the CLI spec builder + the
ingestor's `modalities/registry.py` + `validators.py`). "Ask" = an
interactive prompt / flag; everything else is auto-detected or
sidecar-discovered. `✅` = CLI-supported today; `⏳` = ingestor-ready, CLI
staging pending (§11 phase 4); `🚧` = blocked deeper.

| Task | Label question | Other task-specific questions | CLI |
|---|---|---|---|
| `image_classification` | **class** — "Which column holds the class?" (pick from headers) | `--target-size` W×H (blank = auto-detect from first image); extension auto-detected | ✅ |
| `object_detection` | **none** — classes live in the XML sidecars | `--target-size`; requires `images/` + `annotations/*.xml` (stem-paired) | ✅ |
| `keypoint_detection` | **none** — coords are in the annotations | `--number-of-keypoints` **(required)**; `--target-size` | ✅ |
| `semantic_segmentation` | **none** — label is the PNG mask | `--target-size` (image + mask); requires `images/` + `masks/*_mask.png` | 🚧 (#136) |
| `text_classification` | **class** — "Which column holds the class?" | requires `texts/*.txt`; optional `--schema` | ✅ |
| `token_classification` | **per-token BIO tags** — "Which column holds the BIO tags?" (no min-2 gate) | requires `texts/*.txt` of whitespace-tokenised words; optional `--schema` | ⏳ |
| `sentence_pair_classification` | **class** — "Which column holds the class?" | requires `texts/*.txt` each `text_a⇥text_b`; optional `--schema` | ⏳ |
| `masked_language_modeling` | **none** — self-supervised | requires `sequences/*.txt` **and `tokenizer.json`** at the root (clear error if missing) | ✅ |
| `causal_language_modeling` | **none** — self-supervised | requires `texts/*.txt` (raw, or `prompt⇥completion`); optional `--schema` | ⏳ |
| `seq2seq` | **none** — self-supervised | requires `texts/*.txt` each `source⇥target`; optional `--schema` | ⏳ |
| `embeddings` | **none** — self-supervised contrastive | requires `texts/*.txt` each `anchor⇥positive[⇥negative]`; optional `--schema` | ⏳ |
| `tabular_classification` | **class** — "Which column holds the class?" | `--schema` (required; inferred if blank) | ✅ |
| `tabular_regression` | **target** — "Which column holds the value to predict?" `{column,policy}` | `--schema`; `--label-policy` (default `bucket`) | ✅ |
| `time_series_forecasting` | **target** — "Which column holds the value to predict?" | `--schema`; `--label-policy` (default `bucket`); needs a `timestamp` column (see §12) | ✅ |
| `time_to_event_prediction` | **target** — "Which column holds the value to predict?" | `--schema`; `--label-policy`; `--time-column` (default `time`, must be numeric ≥ 0) | ✅ |

## 9. The taxonomy contract

The task list lives, hand-maintained, in **five places across three repos**:

1. data-ingestors `schema/ingest.v1.json` — the `category` enum (the
   vendored contract). **15 tasks.**
2. data-ingestors `modalities/registry.py` — the ingestor's `ModalitySpec`
   registry.
3. backend `metaApi/models/UserDataSet.py` — `CATEGORY_CHOICES`.
4. backend `global_meta/constants.py` — a **literal duplicate** of the list
   with a `# Make sure these is in sync` comment.
5. CLI `internal/push/category.go` — `categoryRegistry`. **16 entries**
   (adds `instance_segmentation`, which is *not* in the schema).

**What is contracted:** the schema → CLI edge (`sync-schema.sh` vendors the
enum; CI fails on drift), and a *one-directional* CLI test
(`TestRegistryCoversSchemaCategories`) that every schema task is known to
the registry. That test deliberately allows registry supersets — which is
why `instance_segmentation` sits in the CLI registry unchecked.

**What is not contracted:** the backend's two copies are synced *by a
comment*, not a check; nothing ties backend ⇄ schema ⇄ ingestor registry;
there is **no single source of truth**. This is the root cause of the
recurring "the CLI is behind the schema" pain.

**Proposal:** make the `ingest.v1.json` `category` enum the single source of
truth and drift-check every consumer against it in CI:
- CLI: keep the schema⊆registry test; add a registry⊆schema test with an
  explicit `aliases` allow-list (so a deliberate placeholder like
  `instance_segmentation` is declared, not silent).
- backend: a test asserting `CATEGORY_CHOICES` and the `DatasetCategory`
  duplicate both equal the schema enum (replacing the "make sure" comment).
- data-ingestors: a test asserting `modalities.REGISTRY.keys()` equals the
  enum.

Tracked as a cross-repo governance ticket:
[backend#1005](https://github.com/tracebloc/backend/issues/1005).

## 10. Appendix A — supported formats & layouts

| Family | Task(s) | Required layout |
|---|---|---|
| Image | image_classification, keypoint_detection | `<dir>/labels.csv` + `<dir>/images/*.{jpg,jpeg,png}` (keypoint also `--number-of-keypoints`) |
| Image | object_detection | the above + `<dir>/annotations/*.xml` (Pascal VOC) |
| Image | semantic_segmentation *(CLI-pending)* | the above + `<dir>/masks/*.png` |
| Text | text_classification, token_classification, sentence_pair_classification | `<dir>/labels.csv` + `<dir>/texts/*.txt` |
| Text | masked_language_modeling | `<dir>/sequences/*.txt` + `<dir>/tokenizer.json` (no labels) |
| Text | causal_language_modeling, seq2seq, embeddings *(CLI-pending)* | `<dir>/texts/*.txt`, in-file format per task (raw / `prompt⇥completion` / `source⇥target` / `anchor⇥positive[⇥negative]`) |
| Tabular / time | tabular_{classification,regression}, time_series_forecasting, time_to_event_prediction | exactly one `.csv` — a bare file **or** a directory containing one |

Accepted image extensions: `.jpg`, `.jpeg`, `.png` (case-insensitive), one
type per dataset. v0.1 caps: 1 GiB total, 500 MiB per file.

## 11. Phased delivery

1. **Terminology + ceremony** — remove upload/push/stage from user copy
   (amends #173), collapse Step 2, silence Kubernetes behind `--verbose`,
   add the on-prem reassurance at the copy step.
2. **Inverted flow + `--task`** — the split → name → path → task ordering
   (§5), family sniff + the "✔ Found …" echo, the family-scoped task picker
   with glosses and the Available/CLI-pending split; rename
   `--category`→`--task` (required, hidden `--category` alias, drop the
   `image_classification` default); rename `--table`→`--name` and
   `--intent`→`--split` (default `train`).
3. **Flexible input + clarity + path bugs** — accept a bare `.csv`;
   per-family layout help; the `~user` and check-ordering fixes.
4. **Wire the 5 text-family layouts** — make token_classification,
   sentence_pair_classification, causal_language_modeling, seq2seq, and
   embeddings ingestable from the CLI (their `texts/` discover + staging +
   the task-aware label skip). Closes the CLI↔platform gap.
5. **The taxonomy contract** — schema as single source of truth +
   cross-repo drift checks (§9). Governance ticket in `backend`.

## 12. Open questions

1. ~~**`--split` position**~~ — *settled (Rev 2): the order is split → name →
   path → task (§5).*
2. **`semantic_segmentation` / `instance_segmentation`** — is
   `instance_segmentation` a planned task (add to the schema) or dead
   (remove from the CLI registry)? Confirm before the contract test lands.
3. **A re-attach verb** — `tracebloc data logs`/`status` so the
   detach/error path stops emitting a raw `kubectl logs` command. Worth a
   follow-up RFC/ticket.
4. **DS display glosses** — this RFC adopts them (§7); confirm the exact
   wording for the divergent three.
5. **Drop `--label-column` for sidecar-labeled vision** — object detection,
   keypoint, and segmentation carry labels in sidecars; the CLI's current
   plain-string `--label-column` for them is vestigial. Confirm we drop the
   prompt/flag (RFC recommends yes).
6. **`time_series_forecasting` timestamp gap** — the ingestor *requires* a
   `timestamp` column (chronological, not future, `TIMESTAMP` type), but the
   CLI never prompts for or emits it — it rides only in `--schema`. Should
   forecasting get a dedicated timestamp prompt + preflight parity, like
   survival's `--time-column`, so a user can't silently omit/mistype it?
7. **`--target-size` placement divergence** — keypoint emits it top-level;
   image-classification/detection emit it under `file_options`. Normalize,
   or document as an intentional ingestor-contract quirk.
8. **Schema for the regression family** — schema is inferred when `--schema`
   is blank, but the leading-zeros→INT inference gap can mis-type an
   id-like column, which interacts with bucket-hashing the target. Should
   schema be effectively *required* (explicit) for the regression family?

## 13. Non-goals

- Cloud-source datasets (S3/GCS/HTTPS) beyond the 1 GiB cap — v0.2.
- Changing the wire schema field name (`category` stays on the wire; only
  the CLI surface becomes `--task`).
- Server-side `--append` (tracked separately, cli#156).

## Revision history

- **Rev 1 (2026-07-07)** — initial draft from the ingest-flow UX audit,
  DS-vocabulary research, and the flow-inversion discussion with @LukasWodka.
- **Rev 2 (2026-07-07)** — settle the Phase-1 order to **split → name →
  path → task** (name is a deliberate prompt, not path-derived); make the
  **file-or-folder** model explicit ("point at your data in whatever shape
  it has" + the "✔ Found …" echo that removes the confusion, §4); move the
  **label column** fully into the task-specific questions (§5.5, §8). All
  per @LukasWodka.
- **Rev 3 (2026-07-07)** — rewrite §8 as the **label design**: three shapes
  (class / target / absent), precise per-family wording (never "label to
  predict"), and **pick-from-real-headers + exact-match-or-hard-fail**
  validation that closes the data-ingestors#340 silent-NULL-label bug. Add
  §8.1, the **full task-specific input matrix for all 15 tasks**. New open
  questions (§12.5–8) from reconciling the CLI vs. ingestor: drop vestigial
  `--label-column` for sidecar-labeled vision, the forecasting-timestamp
  gap, `--target-size` placement, and schema-required-for-regression.
