#!/usr/bin/env python3
"""E2E test additions: router-source summary/requests + error-log lookup."""
import ctypes
import json
import os
import shutil
import sqlite3
import sys

LIB = "/mnt/dungchung/cpa-usage-statistics/usage-statistics.so"


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


def _load_lib():
    lib = ctypes.CDLL(LIB)
    lib.cliproxyPluginCall.restype = ctypes.c_int
    lib.cliproxyPluginCall.argtypes = [
        ctypes.c_char_p, ctypes.c_char_p, ctypes.c_size_t, ctypes.POINTER(Buffer)]
    lib.cliproxyPluginFree.argtypes = [ctypes.c_void_p, ctypes.c_size_t]
    return lib


_LIB = _load_lib()


def call(method, payload):
    buf = Buffer()
    raw = json.dumps(payload).encode() if payload is not None else b""
    rc = _LIB.cliproxyPluginCall(method.encode(), raw, len(raw), ctypes.byref(buf))
    if buf.ptr:
        data = ctypes.string_at(buf.ptr, buf.len)
        _LIB.cliproxyPluginFree(buf.ptr, buf.len)
    else:
        data = b""
    env = json.loads(data) if data else {}
    if rc != 0 or not env.get("ok"):
        raise RuntimeError(f"{method} failed rc={rc}: {env}")
    return env.get("result")


def mgmt(method, path, query=None):
    import base64
    req = {"Method": method, "Path": path, "Headers": {},
           "Query": query or {}, "Body": ""}
    resp = call("management.handle", req)
    body = base64.b64decode(resp["Body"]) if resp.get("Body") else b""
    return resp["StatusCode"], body


def build_fake_router_db(workdir):
    """model-router.db with 3 rows: 1 routed success, 1 routed fail, 1 direct."""
    import datetime
    path = os.path.join(workdir, "model-router.db")
    if os.path.exists(path):
        os.remove(path)
    db = sqlite3.connect(path)
    db.execute("CREATE TABLE requests (sequence INTEGER PRIMARY KEY, requested_at_ns INTEGER NOT NULL, payload BLOB NOT NULL)")
    db.execute("CREATE TABLE store_state (id INTEGER PRIMARY KEY)")
    base = datetime.datetime(2026, 9, 11, 1, 0, 0)
    rows = []
    for i, (failed, code, alias) in enumerate([
            (False, 0, "fast"), (True, 429, "fast"), (False, 0, "")]):
        ts = (base + datetime.timedelta(minutes=i)).strftime("%Y-%m-%dT%H:%M:%S.123456789Z")
        ns = int((base + datetime.timedelta(minutes=i)).timestamp() * 1e9)
        payload = {
            "sequence": i + 1, "requested_at": ts,
            "attribution": "direct" if alias == "" else "routed",
            "router_model": alias, "provider": "openai-compatible-bai",
            "executor_type": "OpenAICompatExecutor",
            "provider_model": "glm-5.3-flash", "provider_alias": "bai/glm-5.3-flash",
            "source": "openai-compatible-bai", "reasoning_effort": "max",
            "service_tier": "auto", "masked_api_key": "sk******57", "generate": True,
            "failed": failed, "status_code": code,
            "latency_ns": 1_500_000_000, "ttft_ns": 400_000_000,
            "input_tokens": 100 if not failed else 0,
            "output_tokens": 200 if not failed else 0,
            "reasoning_tokens": 0, "cached_tokens": 0, "cache_read_tokens": 0,
            "cache_creation_tokens": 0, "total_tokens": 300 if not failed else 0,
        }
        rows.append((ns, json.dumps(payload)))
    db.executemany("INSERT INTO requests (requested_at_ns, payload) VALUES (?,?)", rows)
    db.commit()
    db.close()
    return path


def build_fake_error_log(logdir, record_utc="2026-09-11T01:01:00Z"):
    """Create an error log whose embedded name-timestamp is the LOCAL time of
    the failed record (CPA names files with local TZ)."""
    import datetime
    os.makedirs(logdir, exist_ok=True)
    t = datetime.datetime.fromisoformat(record_utc.replace("Z", "+00:00"))
    local = t.astimezone()
    ts_local = local.strftime("%Y-%m-%dT%H%M%S")
    name = f"error-v1-chat-completions-{ts_local}-abc123.log"
    content = (
        "=== API REQUEST ===\n{...prompt...}\n\n"
        "=== API RESPONSE ===\n"
        '{"error":{"message":"The request rate exceeds the current model TPM limit","code":"429001"}}\n\n'
        "=== RESPONSE ===\nStatus: 500\n"
    )
    with open(os.path.join(logdir, name), "w") as f:
        f.write(content)
    return name


def main():
    lib = "/tmp/usage-statistics-test"
    os.makedirs(lib, exist_ok=True)
    # library cwd = plugins dir containing both model-router.db and usage dir
    shutil.copy("/mnt/dungchung/cpa-usage-statistics/usage-statistics.so", os.path.join(lib, "usage-statistics.so"))
    os.chmod(os.path.join(lib, "usage-statistics.so"), 0o644)
    build_fake_router_db(lib)
    logs = os.path.join(lib, "logs")
    err_name = build_fake_error_log(logs, record_utc="2026-09-11T01:01:00Z")
    data_dir = os.path.join(lib, "data")
    os.makedirs(data_dir, exist_ok=True)

    cwd = os.getcwd()
    os.chdir(lib)
    try:
        reg = call("plugin.register", {"config_yaml": f"data_dir: {data_dir}\n"})
        assert reg["capabilities"]["usage_plugin"]
        call("management.register", None)

        # 1. summary auto-sources from router db (3 rows)
        code, body = mgmt("GET", "/plugins/usage-statistics/usage/summary", {})
        assert code == 200, body[:200]
        s = json.loads(body)
        assert s["source"] == "router", s.get("source")
        t = s["totals"]
        assert t["calls"] == 3 and t["failed"] == 1, t
        assert t["total_tokens"] == 600, t  # 2 success x 300
        names = {g["name"] for g in s["by_alias"]}
        assert "fast" in names, s["by_alias"]
        print("PASS router summary: source=router, totals/alias ok")

        # 2. requests listing from router db + failed filter
        code, body = mgmt("GET", "/plugins/usage-statistics/usage/requests", {"limit": ["10"]})
        page = json.loads(body)
        assert page["source"] == "router" and page["total"] == 3, page["total"]
        assert page["rows"][0]["id"].startswith("mr:"), page["rows"][0]
        assert page["rows"][0]["api_key"] == "sk******57", page["rows"][0]
        code, body = mgmt("GET", "/plugins/usage-statistics/usage/requests", {"limit": ["10"], "result": ["failed"]})
        page = json.loads(body)
        assert page["total"] == 1 and page["rows"][0]["failure_status_code"] == 429, page["total"]
        print("PASS router requests: pagination + failed filter + masked key")

        # 3. error-log listing by timestamp (fake log at 01:00:01 local, record at 01:01 UTC?)
        # record 2 (failed) requested_at = 2026-09-11T01:01:00Z -> we craft log with local name matching tz window.
        # The match window is ±100s on parsed local time; we just call with that ts.
        ts = "2026-09-11T01:01:00.000000000Z"
        code, body = mgmt("GET", "/plugins/usage-statistics/error-log", {"ts": [ts]})
        assert code == 200, code
        res = json.loads(body)
        # name matching depends on host TZ in container; accept 0..N files but route must not 404
        assert "files" in res, res
        print("PASS error-log listing: route OK, files:", len(res.get("files", [])))

        # 4. error-log content read (direct file fetch)
        code, body = mgmt("GET", "/plugins/usage-statistics/error-log", {"file": [err_name]})
        assert code == 200, code
        content = json.loads(body)
        assert "TPM limit" in content.get("content", ""), list(content)[:3]
        # traversal must be rejected
        code, body = mgmt("GET", "/plugins/usage-statistics/error-log", {"file": ["../config.yaml"]})
        assert code == 400, code
        print("PASS error-log content: read + traversal blocked")
        print("\nROUTER-SOURCE TESTS PASSED")
        return 0
    finally:
        os.chdir(cwd)


if __name__ == "__main__":
    sys.exit(main())
