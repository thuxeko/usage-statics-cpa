#!/usr/bin/env python3
"""E2E harness: load usage-statistics.so and drive it exactly like the CPA
host would (C-ABI envelope). Verifies registration, usage ingestion, summary
aggregation, request pagination, and the dashboard resource page."""
import ctypes
import json
import os
import sys

LIB = os.path.join(os.path.dirname(__file__), "usage-statistics.so")


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


lib = ctypes.CDLL(LIB)
lib.cliproxyPluginCall.restype = ctypes.c_int
lib.cliproxyPluginCall.argtypes = [
    ctypes.c_char_p, ctypes.c_char_p, ctypes.c_size_t, ctypes.POINTER(Buffer)]
lib.cliproxyPluginFree.argtypes = [ctypes.c_void_p, ctypes.c_size_t]


def call(method, payload):
    buf = Buffer()
    raw = json.dumps(payload).encode() if payload is not None else b""
    rc = lib.cliproxyPluginCall(method.encode(), raw, len(raw), ctypes.byref(buf))
    if buf.ptr:
        data = ctypes.string_at(buf.ptr, buf.len)
        lib.cliproxyPluginFree(buf.ptr, buf.len)
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


def usage_record(model, alias="", provider="openai-compatible-qwencloud", key="sk-test-key-12345",
                 failed=False, status=0, in_tok=100, out_tok=200, mins_ago=0):
    from datetime import datetime, timedelta, timezone
    ts = datetime.now(timezone.utc) - timedelta(minutes=mins_ago)
    return {
        "Provider": provider, "ExecutorType": "openai", "Model": model, "Alias": alias,
        "APIKey": key, "AuthID": "auth-1", "AuthIndex": "0", "AuthType": "api-key",
        "Source": "test", "ReasoningEffort": "", "ServiceTier": "",
        "RequestedAt": ts.isoformat(), "Latency": 1_500_000_000, "TTFT": 400_000_000,
        "Failed": failed, "Failure": {"StatusCode": status, "Body": "boom" if failed else ""},
        "Detail": {"InputTokens": in_tok, "OutputTokens": out_tok, "ReasoningTokens": 10,
                   "CachedTokens": 5, "CacheReadTokens": 0, "CacheCreationTokens": 0,
                   "TotalTokens": in_tok + out_tok + 10},
    }


def main():
    db_dir = os.path.join(os.path.dirname(__file__), "test-run-data")
    os.makedirs(db_dir, exist_ok=True)
    for f in os.listdir(db_dir):
        os.remove(os.path.join(db_dir, f))

    # 1. register with config
    reg = call("plugin.register", {"config_yaml": f"data_dir: {db_dir}\nretention_days: 30\n"})
    caps = reg["capabilities"]
    assert caps["usage_plugin"] and caps["management_api"], caps
    print("PASS register: metadata=%s caps=%s" % (reg["metadata"]["Name"], caps))

    # 2. management.register → routes + resources
    mreg = call("management.register", None)
    paths = sorted(r["Path"] for r in mreg["routes"])
    print("routes:", paths)
    assert any("usage/summary" in p for p in paths), "summary route missing"
    assert any("usage/requests" in p for p in paths), "requests route missing"
    res = mreg["resources"]
    print("resources:", res)
    assert res and res[0]["Menu"] == "Usage Statistics", res
    print("PASS management.register: routes + resource menu present")

    # 3. push usage records: direct calls, alias (combo) calls, failures
    records = [
        usage_record("qwen3.6-flash", mins_ago=5),
        usage_record("qwen3.6-flash", alias="low-com", mins_ago=4),
        usage_record("glm-5.2", alias="low-com", provider="openai-compatible-ollama", mins_ago=3),
        usage_record("gemini-3-flash", alias="medium-com", provider="gemini", mins_ago=2),
        usage_record("gemini-3-flash", alias="medium-com", provider="gemini",
                     failed=True, status=429, mins_ago=1),
        usage_record("claude-opus-4-6", provider="claude", key="sk-other-key-abcdef",
                     failed=True, status=500, in_tok=50, out_tok=0, mins_ago=1),
    ]
    for r in records:
        out = call("usage.handle", r)
        assert out.get("stored"), out
    print(f"PASS usage.handle: {len(records)} records stored")

    # 4. summary aggregate — CPA forwards the FULL public path to
    # management.handle (matches model-router source); also accept relative.
    code, body = mgmt("GET", "/v0/management/plugins/usage-statistics/usage/summary", {})
    assert code == 200, (code, body[:200])
    code, body = mgmt("GET", "/plugins/usage-statistics/usage/summary", {})
    assert code == 200, code
    s = json.loads(body)
    t = s["totals"]
    print("summary totals:", json.dumps(t))
    assert t["calls"] == 6, t
    assert t["failed"] == 2, t
    assert t["total_tokens"] == sum(
        r["Detail"]["TotalTokens"] for r in records), "token sum mismatch"
    models = {g["name"]: g for g in s["by_model"]}
    assert "qwen3.6-flash" in models and "glm-5.2" in models, list(models)
    aliases = {g["name"]: g for g in s["by_alias"]}
    print("by_alias:", [(g["name"], g["sub"], g["calls"]) for g in s["by_alias"]])
    assert aliases["low-com"]["calls"] == 2 and aliases["low-com"]["sub"] in (
        "qwen3.6-flash", "glm-5.2"), "alias grouping broken"
    keys = {g["name"] for g in s["by_api_key"]}
    assert "sk-test-key-12345" in keys and "sk-other-key-abcdef" in keys, keys
    assert len(s["by_hour"]) >= 1 and s["by_hour"][0]["calls"] >= 1, s["by_hour"]
    codes = {c["status_code"]: c["calls"] for c in s["status_codes"]}
    assert codes.get(429) == 1 and codes.get(500) == 1, codes
    print("PASS summary: totals/hour/model/alias/key/statusCodes all correct")

    # 5. requests pagination + filters
    code, body = mgmt("GET", "/plugins/usage-statistics/usage/requests",
                     {"limit": ["2"], "offset": ["0"]})
    assert code == 200, code
    page = json.loads(body)
    assert page["total"] == 6 and len(page["rows"]) == 2, (page["total"], len(page["rows"]))
    code, body = mgmt("GET", "/plugins/usage-statistics/usage/requests",
                     {"limit": ["10"], "result": ["failed"]})
    page = json.loads(body)
    assert page["total"] == 2 and all(r["failed"] for r in page["rows"]), page["total"]
    code, body = mgmt("GET", "/plugins/usage-statistics/usage/requests",
                      {"limit": ["10"], "model": ["low-com"]})
    page = json.loads(body)
    assert page["total"] == 2, page["total"]  # matches alias OR model
    print("PASS requests: pagination + failed filter + alias filter")

    # 6. dashboard resource page (full public path like CPA dispatches)
    code, body = mgmt("GET", "/v0/resource/plugins/usage-statistics/dashboard", {})
    assert code == 200, code
    html = body.decode()
    assert ("Usage Statistics" in html or "Analytics Dashboard" in html) and "usage/summary" in html, "dashboard html unexpected"
    assert len(html) > 5000, len(html)
    print("PASS dashboard: HTML page served, %d bytes, title + API calls present" % len(html))

    # 6b. dashboard2 resource page (v2 clean, icon-free)
    code, body2 = mgmt("GET", "/v0/resource/plugins/usage-statistics/dashboard2", {})
    assert code == 200, code
    html2 = body2.decode()
    assert "Analytics Dashboard" in html2 and "usage/summary" in html2, "dashboard2 html unexpected"
    assert len(html2) > 5000, len(html2)
    print("PASS dashboard2: HTML page served, %d bytes, title + API calls present" % len(html2))

    # 7. unknown route → 404 (full path form)
    code, _ = mgmt("GET", "/v0/management/plugins/usage-statistics/nope", {})
    assert code == 404, code
    print("PASS unknown route 404")

    print("\nALL E2E TESTS PASSED")


if __name__ == "__main__":
    sys.exit(main())
