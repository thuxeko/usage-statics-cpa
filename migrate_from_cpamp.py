#!/usr/bin/env python3
"""One-time migration: CPAMP usage.sqlite (usage_events) -> plugin usage.db
(usage_records). Idempotent (INSERT OR IGNORE on event_hash id), batched with
busy_timeout so the live plugin can keep writing between batches."""
import sqlite3
import sys
import time

OLD_DB = "/d/usage.sqlite"
NEW_DB = "/d/usage-statistics/usage.db"
BATCH = 500


def fmt_ts(ms):
    """Plugin store format: 2006-01-02T15:04:05.000000000Z (UTC, 9 digits)."""
    sec, msec = divmod(int(ms), 1000)
    t = time.gmtime(sec)
    return "%04d-%02d-%02dT%02d:%02d:%02d.%03d000000Z" % (
        t.tm_year, t.tm_mon, t.tm_mday, t.tm_hour, t.tm_min, t.tm_sec, msec)


def main():
    old = sqlite3.connect("file:%s?immutable=1" % OLD_DB, uri=True)
    old.execute("PRAGMA busy_timeout=30000")
    new = sqlite3.connect(NEW_DB, timeout=30)
    new.execute("PRAGMA busy_timeout=30000")

    total_old = old.execute("SELECT COUNT(*) FROM usage_events").fetchone()[0]
    before_new = new.execute("SELECT COUNT(*) FROM usage_records").fetchone()[0]
    print("old rows: %d | new rows before: %d" % (total_old, before_new))

    insert_sql = """
        INSERT OR IGNORE INTO usage_records (
            id, timestamp, api_key, provider, model, alias, source,
            auth_id, auth_index, auth_type, executor_type,
            reasoning_effort, service_tier, latency_ms, ttft_ms,
            input_tokens, output_tokens, reasoning_tokens, cached_tokens,
            cache_read_tokens, cache_creation_tokens, total_tokens,
            failed, failure_status_code, failure_body
        ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"""

    cursor = old.execute("""
        SELECT event_hash, timestamp_ms,
               COALESCE(api_key_hash,''), COALESCE(provider,''),
               COALESCE(NULLIF(resolved_model,''), model),
               COALESCE(requested_model,''), COALESCE(source,''),
               COALESCE(auth_type,''), COALESCE(auth_index,''),
               COALESCE(executor_type,''),
               COALESCE(reasoning_effort,''), COALESCE(service_tier,''),
               COALESCE(latency_ms,0), COALESCE(ttft_ms,0),
               COALESCE(input_tokens,0), COALESCE(output_tokens,0),
               COALESCE(reasoning_tokens,0), COALESCE(cached_tokens,0),
               COALESCE(cache_read_tokens,0), COALESCE(cache_creation_tokens,0),
               COALESCE(total_tokens,0),
               COALESCE(failed,0), COALESCE(fail_status_code,0),
               SUBSTR(COALESCE(fail_body,''),1,4000)
        FROM usage_events ORDER BY timestamp_ms ASC""")

    inserted = skipped = 0
    batch = []
    for row in cursor:
        (event_hash, ts_ms, api_key, provider, model, alias, source,
         auth_type, auth_index, executor_type, reasoning_effort, service_tier,
         latency_ms, ttft_ms, in_t, out_t, rea_t, cach_t, cr_t, cc_t, tot_t,
         failed, fail_code, fail_body) = row
        ts = fmt_ts(ts_ms) if ts_ms else None
        if not ts:
            skipped += 1
            continue
        if latency_ms < 0:
            latency_ms = 0
        if ttft_ms < 0:
            ttft_ms = 0
        for v in (in_t, out_t, rea_t, cach_t, cr_t, cc_t, tot_t):
            pass
        batch.append((
            event_hash, ts, api_key.strip(), provider.strip(), model.strip(),
            alias.strip(), source.strip(), "", auth_index.strip(),
            auth_type.strip(), executor_type.strip(), reasoning_effort.strip(),
            service_tier.strip(), latency_ms, ttft_ms,
            max(0, in_t), max(0, out_t), max(0, rea_t), max(0, cach_t),
            max(0, cr_t), max(0, cc_t), max(0, tot_t),
            1 if failed else 0, max(0, fail_code), fail_body.strip()))
        if len(batch) >= BATCH:
            new.executemany(insert_sql, batch)
            new.commit()
            inserted += len(batch)
            print("  committed %d / %d" % (inserted, total_old), flush=True)
            batch = []
    if batch:
        new.executemany(insert_sql, batch)
        new.commit()
        inserted += len(batch)

    after_new = new.execute("SELECT COUNT(*) FROM usage_records").fetchone()[0]
    rng = new.execute("SELECT MIN(timestamp), MAX(timestamp) FROM usage_records").fetchone()
    print("inserted(attempted): %d | skipped(no ts): %d" % (inserted, skipped))
    print("new rows after: %d (delta %d)" % (after_new, after_new - before_new))
    print("range now: %s .. %s" % rng)
    models = new.execute(
        "SELECT model, COUNT(*) FROM usage_records GROUP BY model ORDER BY 2 DESC LIMIT 8").fetchall()
    print("top models:", models)
    old.close()
    new.close()
    print("MIGRATION_DONE")


if __name__ == "__main__":
    sys.exit(main())
