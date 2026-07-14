#!/usr/bin/env python3
"""Regenerate internal/push/testdata/parity/goldens.json by running the REAL
data-ingestors validators over the parity fixture cases.

This is the Python half of the validator-parity harness (backend#828 P3):
the CLI's local preflight (internal/push/preflight.go) previews the
in-cluster validators, and parity_golden_test.go pins that the two sides
agree case-by-case. When the ingestor's rules change, re-run this and
commit the diff — a changed verdict then fails the Go test until the
preview is consciously updated.

Usage:
    DATA_INGESTORS_DIR=~/path/to/data-ingestors python3 scripts/gen-validator-goldens.py

DATA_INGESTORS_DIR must point at a checkout of tracebloc/data-ingestors
with its dependencies importable (its own .venv works:
    ~/repos/data-ingestors/.venv/bin/python scripts/gen-validator-goldens.py
also does the trick — the script only needs pandas + Pillow, no MySQL).

Verdicts are recorded at the accept/reject level, not message-text level —
error copy may drift harmlessly; verdicts may not. TableNameValidator and
DuplicateValidator are skipped (they check cluster-side state — the table
name is validated separately by both sides, and destination-duplicate
handling is the cli#70 guard's territory, not a data-hygiene rule).

For cases the manifest flags ``value_parity`` (and the ingestor accepts),
this also records a VALUE-level golden — the label column the ingestor read
path RESOLVES to, the row count, and the class set it stores — by driving the
REAL read path (CSVIngestor.read_data + the #340 label resolution +
RecordProcessor). parity_golden_test.go then pins that the Go preview reads
exactly the same values, catching accept/accept-with-divergent-label — the
#340 class a verdict alone is blind to (backend#1009). This requires the #340
fix in the target ingestor; the generator fails loudly without it rather than
pin the bug.
"""

import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
PARITY = os.path.join(HERE, "..", "internal", "push", "testdata", "parity")

di = os.environ.get("DATA_INGESTORS_DIR", "")
if di:
    sys.path.insert(0, os.path.abspath(di))

try:
    from tracebloc_ingestor.utils.validators_mapping import map_validators
    from tracebloc_ingestor.config import Config
    from tracebloc_ingestor.ingestors import preflight
except ImportError as exc:  # pragma: no cover
    sys.exit(
        f"can't import the data-ingestors package ({exc}).\n"
        "Set DATA_INGESTORS_DIR to a checkout, or run with its venv python."
    )

SKIP = {"TableNameValidator", "DuplicateValidator"}
ANSI = re.compile(r"\x1b\[[0-9;]*m")


def infer_schema(csv_path):
    """Mirror the CLI's inference shape: every trimmed header column typed —
    the schema content doesn't matter for these validators, membership does.
    Read with pandas (BOM-stripping) exactly like the CLI's header reader."""
    import pandas as pd

    cols = list(pd.read_csv(csv_path, nrows=0, encoding="utf-8").columns)
    return {str(c).strip(): "VARCHAR(255)" for c in cols}


def read_label_values(case, csv_path, cfg, options):
    """Drive the REAL ingestor read path — CSVIngestor.read_data + the #340
    label-column resolution + RecordProcessor — to capture the value-level view
    the parity harness pins: the resolved label header, the row count, and the
    sorted distinct classes the ingestor actually stores. This is the only
    thing that catches accept/accept-with-divergent-label (the #340 class):
    verdicts stay 'accept' while the stored labels silently go null.

    Requires the #340 fix (BaseIngestor._resolve_label_column) in the target
    ingestor — without it a case-/whitespace-mismatched label would read null
    and this generator would pin the BUG. Fails loudly if it's absent.
    """
    from unittest.mock import MagicMock

    from tracebloc_ingestor.ingestors.csv_ingestor import CSVIngestor

    db = MagicMock()
    db.config = cfg
    file_opts = {k: v for k, v in options.items() if k != "schema"}
    ing = CSVIngestor(
        database=db,
        api_client=MagicMock(),
        table_name="parity_t",
        schema=options.get("schema", {}) or {},
        label_column=case.get("label_column", "label"),
        intent="train",
        category=case["category"],
        file_options=file_opts,
    )
    if not hasattr(ing, "_resolve_label_column"):
        sys.exit(
            "the target ingestor predates the #340 label-resolution fix; "
            "value-level parity requires it. Point DATA_INGESTORS_DIR at a "
            "checkout that includes BaseIngestor._resolve_label_column."
        )
    records = list(ing.read_data(csv_path))
    # Pin the label column on the first record that CONTAINS it (mirrors the
    # ingest loop; sparse-record-safe), then read every row's stored label.
    for rec in records:
        if ing._resolve_label_column(rec.keys()):
            break
    labels = []
    for rec in records:
        cleaned = ing.process_record(rec)
        labels.append(cleaned.get("label") if cleaned else None)
    classes = sorted({str(v) for v in labels if v is not None})
    return {
        "resolved_label": ing.label_column,
        "row_count": len(records),
        "classes": classes,
    }


def run_case(case):
    case_dir = os.path.join(PARITY, "cases", case["name"])
    csv_path = os.path.join(case_dir, case["csv"])

    if True:  # (kept indented for a small diff; no tempdir needed — DuplicateValidator is skipped)
        cfg = Config(SRC_PATH=case_dir, TABLE_NAME="parity_t")
        options = {"label_column": case.get("label_column", "label")}
        if case.get("extension"):
            options["extension"] = case["extension"]
        if case.get("target_size"):
            options["target_size"] = case["target_size"]
        if case.get("min_size"):
            # The --min-size floor override (#348), cross-checked end-to-end:
            # the image factory reads options["min_size"] into
            # ImageResolutionValidator, so a per-case override drives the REAL
            # validator the same way the Go preview drives SpecArgs.MinSize.
            options["min_size"] = case["min_size"]
        if case["category"].startswith(("tabular", "time_")):
            # An explicit per-case schema (mirroring --schema) wins; else
            # infer — BOTH sides of the harness use the same source so
            # dtype-sensitive cases (label diversity) stay comparable.
            schema = case.get("schema")
            if not schema:
                try:
                    schema = infer_schema(csv_path)
                except Exception:
                    schema = {}
            options["schema"] = schema
            options["full_schema"] = schema
        elif case["category"] == "semantic_segmentation":
            # semseg declares the masks sidecar's mask_id link column in the
            # schema it sends to the ingestor (spec.go: buildSpec sets
            # schema={"mask_id": "VARCHAR(255)"}). data-ingestors #358's
            # MaskIdColumnValidator REQUIRES that declaration — an undeclared
            # mask_id is dropped at ingest, so the stored table lacks it and the
            # training client raises FileNotFoundError. The harness must drive
            # the validators with the SAME schema the CLI sends, or every semseg
            # case is spuriously rejected on the mask_id contract. An explicit
            # per-case schema still wins (e.g. a case that deliberately omits
            # mask_id to exercise the reject path).
            options["schema"] = case.get("schema", {"mask_id": "VARCHAR(255)"})
            options["full_schema"] = options["schema"]

        errors = []
        try:
            preflight.check_csv_encoding(csv_path)
        except Exception as exc:
            errors.append(f"csv-encoding: {exc}")

        for v in map_validators(case["category"], options, cfg):
            if type(v).__name__ in SKIP:
                continue
            try:
                res = v.validate(csv_path)
                if not res.is_valid:
                    errors.extend(
                        f"{type(v).__name__}: {ANSI.sub('', str(e)).replace(case_dir + os.sep, '')}"
                        for e in res.errors
                    )
            except Exception as exc:  # a raising validator is a rejection too
                errors.append(f"{type(v).__name__}: raised {exc}")

    result = {
        "verdict": "reject" if errors else "accept",
        "errors": errors[:6],
    }
    # Value-level golden (data-ingestors #340 class): for cases the manifest
    # flags value_parity AND the ingestor accepts, pin the resolved label,
    # row count, and class set the REAL read path produces — parity_golden_test
    # asserts the Go preview reads exactly these. Only meaningful when accepted
    # (a rejected run never reaches the read path).
    if case.get("value_parity") and not errors:
        result["values"] = read_label_values(case, csv_path, cfg, options)
    return result


def main():
    with open(os.path.join(PARITY, "cases.json")) as f:
        manifest = json.load(f)

    goldens = {}
    for case in manifest["cases"]:
        goldens[case["name"]] = run_case(case)
        print(f"  {case['name']:24s} → {goldens[case['name']]['verdict']}")

    out = os.path.join(PARITY, "goldens.json")
    with open(out, "w") as f:
        json.dump(
            {
                "_comment": "GENERATED by scripts/gen-validator-goldens.py from the REAL "
                "data-ingestors validators — do not hand-edit. Regenerate when the "
                "ingestor's rules change; parity_golden_test.go pins these.",
                "verdicts": goldens,
            },
            f,
            indent=2,
            sort_keys=True,
        )
        f.write("\n")
    print(f"wrote {out}")


if __name__ == "__main__":
    main()
