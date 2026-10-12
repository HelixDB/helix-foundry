# Sensitive column values reach hosted AI providers through query error messages

**Weakness:** CWE-209, Generation of Error Message Containing Sensitive Information
**Affected:** helix-foundry `main` at 9780d74 (12 October 2026); earlier releases with the same code
**Suggested severity:** Moderate

## Summary

Sensitive columns are protected from hosted AI providers by masking their sample values (`[masked]`) in the dataset context. Their values can still reach the provider through DuckDB error messages. When a model-written query fails, the error text goes to the provider for one repair attempt. That happens with any provider and regardless of `allowRawData`. DuckDB's conversion errors quote the offending row value word for word. `redact()` does not catch these values, because they are not credential-shaped.

## Details

- **Which columns are sensitive.** `services/executor/src/engine.ts:341-344` marks a column sensitive when its name matches `/email|phone|ssn|password|secret|token|address|birth|credit/i`.
- **How they are protected.** `apps/api/src/assistant.ts:541-573` (`mask`) replaces their sample values with `[masked]` when the provider is hosted and `allowRawData` is off. That is the only protection. The executor loads every column (`SELECT *`, `engine.ts:215-217`) and does not know about sensitivity.
- **The leak.** `apps/api/src/assistant.ts:897-906` catches a failed Analyst query and sets `context.repair = { sql, error: redact(message).slice(0, 800) }`. It then sends that context to the provider (`ask("analysis-repair", …)`). Nothing checks the provider or `allowRawData` here. By contrast, the result-summary step that sends rows is gated on `settings.provider === "local" || settings.allowRawData` (`assistant.ts:933`).
- **Same pattern elsewhere.** Validation-failure details go to the provider for repair at `assistant.ts:1356-1360`. They carry the same DuckDB text.
- **Voice path.** When a run's repair also fails, its raw error is stored (`assistant.ts:1533`). The live voice task then reads it back to the OpenAI realtime model (`live.ts:749`, first 200 characters).

## Reproduction

The table below has the shape the executor builds; the query runs through the executor's own wrapper. This uses the repo's `@duckdb/node-api` 1.5.5.

```sql
CREATE TABLE d_users AS
  SELECT * FROM (VALUES (1, 'alice@example.com', '123-45-6789', 'hunter2')) t(id, email, ssn, password);
```

| Model-written SQL | Error sent to the provider (after `redact()`) |
| --- | --- |
| `SELECT count(*) FROM d_users WHERE ssn = 123456789` | `Conversion Error: Could not convert string '123-45-6789' to INT32 when casting from source column ssn` |
| `SELECT CAST(password AS INTEGER) FROM d_users` | `Conversion Error: Could not convert string 'hunter2' to INT32 when casting from source column password` |
| `SELECT sum(email::INTEGER) FROM d_users` | `Conversion Error: Could not convert string 'alice@example.com' to INT32 when casting from source column email` |

The first query is an ordinary mistake: comparing a text column with a number. No malicious model is needed.

In the app:
1. Choose Claude, OpenAI or a custom provider, and leave raw data off.
2. Import a dataset with an `ssn`, `email` or `password` column stored as text.
3. Ask a question that leads the model to compare or cast that column as a number.
4. The repair request sent to the provider contains the value.

## Impact

Values the user's settings promise to keep from a hosted provider (emails, SSNs, passwords, tokens, addresses) can be sent to it. That provider is one the user chose, not an arbitrary third party, and only the first offending value per error is quoted. A model influenced by prompt injection in non-sensitive columns could also write such queries on purpose (`CAST(secret AS INT)` or `error(secret)`) to read protected values one at a time.

## Suggested fixes

1. **Gate engine error text** for hosted providers without `allowRawData`. Either drop quoted literals from DuckDB value errors before they leave the API, or send only the error class and the first line with its quoted strings replaced. The column name and target type are kept, because the repair needs them. The forms to cover are `Could not convert string '…'`, `Could not parse string "…"` and the line it echoes, and `value out of range: "…"`.
2. **Or keep sensitive columns out of the sandbox** when raw data is not allowed, or replace them with hashes there. That also stops deliberate exfiltration.
3. **Apply the same gate** to validation-failure details (`assistant.ts:1360`) and to the stored run error that the voice model reads (`live.ts:749`).

## Possibly related

`live.ts:756-772` (`resultText`) sends the first result row's values (up to 8 columns) to the OpenAI realtime model without checking `allowRawData` or sensitive columns. The text Analyst's summary step is gated for the same rows. This may be intentional for voice answers; I'm listing it so it can be confirmed.
