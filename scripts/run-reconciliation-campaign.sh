#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p docs/evidence

if [[ $# -eq 0 ]]; then
  # The existing test service mounts this project at /src. The Go test creates
  # and drops its own schemas. The existing payment-integrity DB must be running.
  docker compose -p payment-integrity --profile tools run --rm -T --no-deps \
    -e REQUIRE_DB_TESTS=1 \
    -e RECONCILIATION_CAMPAIGN_REPORT=/src/docs/evidence/reconciliation-campaign.json \
    test go test -race -count=1 -timeout=5m -run '^Test(Campaign|ReconciliationCampaign)' -v ./internal/service
elif [[ $# -eq 1 && "$1" == "--local" ]]; then
  # Optional path for a native Go/PostgreSQL environment or existing CI job.
  : "${TEST_DATABASE_URL:?Set TEST_DATABASE_URL for native PostgreSQL testing}"
  REQUIRE_DB_TESTS=1 \
    RECONCILIATION_CAMPAIGN_REPORT="$PWD/docs/evidence/reconciliation-campaign.json" \
    go test -race -count=1 -timeout=5m -run '^Test(Campaign|ReconciliationCampaign)' -v ./internal/service
else
  echo 'Usage: bash scripts/run-reconciliation-campaign.sh [--local]' >&2
  exit 2
fi

python3 - <<'PY'
import json
from pathlib import Path

path = Path("docs/evidence/reconciliation-campaign.json")
report = json.loads(path.read_text())
score = report["score"]
print("\nSynthetic PostgreSQL reconciliation campaign")
print("Seed:", report["seed"])
print("Dataset SHA-256:", report["dataset_sha256"])
print("Clean baseline issues:", len(report["baseline"]["issues"]))
for row in score["by_failure_type"]:
    print(f"{row['failure_type']}: {row['detected']}/{row['seeded']} detected")
print("Total detected:", score["true_positives"], "/", score["seeded_discrepancies"])
print("False positives:", score["false_positives"])
print("False negatives:", score["false_negatives"])
print("Clean control lifecycles:", report["clean_control_lifecycles"])
print("Clean control postings flagged:", score["clean_control_postings_flagged"])
print("Passed:", report["passed"])
print("Evidence:", path)
if not report["passed"]:
    raise SystemExit(1)
PY
