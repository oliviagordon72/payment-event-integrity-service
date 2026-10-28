#!/usr/bin/env python3
"""Signed, shuffled synthetic deliveries. Never creates payments at Stripe."""
import argparse
import concurrent.futures
import hashlib
import hmac
import json
import os
import random
import time
import urllib.request
import uuid
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lifecycles", type=int, default=30)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--omit-refunds", type=int, default=0)
    parser.add_argument("--timeout", type=int, default=90)
    args = parser.parse_args()
    if not 0 <= args.omit_refunds <= args.lifecycles or args.lifecycles < 1:
        parser.error("require lifecycles >= 1 and 0 <= omit-refunds <= lifecycles")
    values = {}
    if Path(".env").exists():
        for line in Path(".env").read_text().splitlines():
            if line.strip() and not line.lstrip().startswith("#") and "=" in line:
                key, value = line.split("=", 1)
                values[key.strip()] = value.strip().strip("\"'")
    values.update(os.environ)
    secret = values.get("STRIPE_WEBHOOK_SECRET", "")
    token = values.get("ADMIN_TOKEN", "")
    if not secret or not token:
        parser.error("copy .env.example to .env or set STRIPE_WEBHOOK_SECRET and ADMIN_TOKEN")
    base = values.get("SERVICE_URL", "http://127.0.0.1:8080")
    version = values.get("STRIPE_API_VERSION", "2026-09-30.endive")

    def get(path):
        request = urllib.request.Request(base + path, headers={"Authorization": "Bearer " + token})
        with urllib.request.urlopen(request, timeout=15) as response:
            return json.load(response)

    def clearing():
        return next((row["debit_minus_credit"] for row in get("/v1/balances")
                     if row["account"] == "stripe_clearing" and row["currency"] == "usd"), 0)

    before = clearing()
    prefix = uuid.uuid4().hex[:12]
    created = int(time.time())
    provider = {"charges": [], "refunds": []}
    events = []

    def event(name, kind, obj):
        events.append({"id": "evt_demo_" + prefix + "_" + name, "object": "event", "type": kind,
                       "api_version": version, "livemode": False, "data": {"object": obj}})

    for i in range(args.lifecycles):
        charge_id = f"ch_demo_{prefix}_{i}"
        charge = {"id": charge_id, "object": "charge", "amount_captured": 1000,
                  "currency": "usd", "paid": True, "captured": True, "status": "succeeded",
                  "created": created, "livemode": False, "payment_method_details": {"type": "card"}}
        refund = {"id": f"re_demo_{prefix}_{i}", "object": "refund", "charge": charge_id,
                  "amount": 250, "currency": "usd", "status": "succeeded", "created": created}
        provider["charges"].append(charge)
        provider["refunds"].append(refund)
        event(f"c_{i}_a", "charge.succeeded", charge)
        event(f"c_{i}_b", "charge.captured", charge)
        if i >= args.omit_refunds:
            event(f"r_{i}_a", "refund.created", refund)
            event(f"r_{i}_b", "refund.updated", refund)

    Path("demo-provider.json").write_text(json.dumps(provider, indent=2) + "\n")
    deliveries = events * 2
    random.Random(args.seed).shuffle(deliveries)

    def deliver(evt):
        raw = json.dumps(evt, separators=(",", ":")).encode()
        timestamp = str(int(time.time()))
        digest = hmac.new(secret.encode(), timestamp.encode() + b"." + raw, hashlib.sha256).hexdigest()
        request = urllib.request.Request(base + "/webhooks/stripe", data=raw, method="POST",
                                         headers={"Content-Type": "application/json",
                                                  "Stripe-Signature": f"t={timestamp},v1={digest}"})
        with urllib.request.urlopen(request, timeout=20) as response:
            if response.status != 202:
                raise RuntimeError(f"unexpected HTTP {response.status}")
            return json.load(response)

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
        receipts = list(executor.map(deliver, deliveries))
    pending = {evt["id"] for evt in events}
    deadline = time.monotonic() + args.timeout
    while pending and time.monotonic() < deadline:
        for event_id in list(pending):
            state = get("/v1/events/" + event_id)
            if state["state"] == "dead":
                raise RuntimeError(f"{event_id}: {state}")
            if state["state"] == "done":
                pending.remove(event_id)
        if pending:
            time.sleep(0.2)
    if pending:
        raise RuntimeError(f"{len(pending)} events did not complete before timeout")
    expected = 1000 * args.lifecycles - 250 * (args.lifecycles - args.omit_refunds)
    actual = clearing() - before
    report = {"source": "synthetic local workload", "seed": args.seed, "lifecycles": args.lifecycles,
              "deliveries": len(deliveries), "unique_events": len(events),
              "duplicate_receipts": sum(receipt["duplicate"] for receipt in receipts),
              "intentionally_omitted_refunds": args.omit_refunds,
              "expected_net_minor_units": expected, "actual_net_minor_units": actual,
              "passed": actual == expected}
    Path("demo-report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    if actual != expected:
        raise SystemExit("ledger balance differs from expected workload delta")


if __name__ == "__main__":
    main()
