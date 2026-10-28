CREATE TABLE IF NOT EXISTS inbox (
  event_id text PRIMARY KEY,
  payload bytea NOT NULL,
  state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','processing','done','dead')),
  attempts integer NOT NULL DEFAULT 0,
  available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  lease_until timestamptz,
  lease_token text,
  last_error text,
  received_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS inbox_ready ON inbox(state, available_at, lease_until);

-- Mutable projection rows are locks and refund-limit counters, not the ledger.
CREATE TABLE IF NOT EXISTS captures (
  charge_id text PRIMARY KEY,
  amount bigint NOT NULL CHECK (amount > 0 AND amount <= 99999999),
  currency text NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
  refunded bigint NOT NULL DEFAULT 0 CHECK (refunded >= 0 AND refunded <= amount)
);

-- Each journal row represents one balanced, two-leg financial posting.
CREATE TABLE IF NOT EXISTS journal (
  posting_key text PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('capture','refund')),
  object_id text NOT NULL,
  charge_id text NOT NULL REFERENCES captures(charge_id),
  amount bigint NOT NULL CHECK (amount > 0 AND amount <= 99999999),
  currency text NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
  provider_created bigint NOT NULL CHECK (provider_created > 0),
  debit_account text NOT NULL,
  credit_account text NOT NULL,
  source_event text NOT NULL REFERENCES inbox(event_id),
  posted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE(kind, object_id),
  CHECK (posting_key = kind || ':' || object_id),
  CHECK ((kind='capture' AND debit_account='stripe_clearing' AND credit_account='merchant_payable')
      OR (kind='refund' AND debit_account='merchant_payable' AND credit_account='stripe_clearing'))
);

CREATE OR REPLACE VIEW ledger_entries AS
  SELECT posting_key, charge_id, currency, debit_account AS account, amount AS signed_amount FROM journal
  UNION ALL
  SELECT posting_key, charge_id, currency, credit_account, -amount FROM journal;

CREATE OR REPLACE FUNCTION forbid_journal_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'journal is append-only'; END $$;
DROP TRIGGER IF EXISTS journal_no_change ON journal;
CREATE TRIGGER journal_no_change BEFORE UPDATE OR DELETE ON journal
  FOR EACH ROW EXECUTE FUNCTION forbid_journal_mutation();
DROP TRIGGER IF EXISTS journal_no_truncate ON journal;
CREATE TRIGGER journal_no_truncate BEFORE TRUNCATE ON journal
  FOR EACH STATEMENT EXECUTE FUNCTION forbid_journal_mutation();

CREATE TABLE IF NOT EXISTS reconciliation_runs (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  cutoff timestamptz NOT NULL,
  report jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
