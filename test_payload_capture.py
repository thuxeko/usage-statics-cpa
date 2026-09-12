import ctypes
import json
import os
import sys
import tempfile
import time

def test_payload_capture():
    so_path = "/mnt/dungchung/cpa-usage-statistics/usage-statistics.so"
    if not os.path.exists(so_path):
        print("so not found")
        sys.exit(1)
    
    lib = ctypes.CDLL(so_path)
    
    class Buffer(ctypes.Structure):
        _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]
    
    class PluginAPI(ctypes.Structure):
        _fields_ = [
            ("abi_version", ctypes.c_uint32),
            ("call", ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.POINTER(ctypes.c_uint8), ctypes.c_size_t, ctypes.POINTER(Buffer))),
            ("free_buffer", ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)),
            ("shutdown", ctypes.CFUNCTYPE(None)),
        ]
        
    plugin = PluginAPI()
    init_res = lib.cliproxy_plugin_init(None, ctypes.byref(plugin))
    assert init_res == 0, f"init failed: {init_res}"
    
    def call(method, payload_dict=None, raw_bytes=None):
        if raw_bytes is None:
            raw_bytes = json.dumps(payload_dict).encode("utf-8") if payload_dict is not None else b""
        req_buf = (ctypes.c_uint8 * len(raw_bytes)).from_buffer_copy(raw_bytes) if len(raw_bytes) > 0 else None
        res_buf = Buffer()
        rc = plugin.call(method.encode("utf-8"), req_buf, len(raw_bytes), ctypes.byref(res_buf))
        assert rc == 0, f"call {method} returned {rc}"
        raw = ctypes.string_at(res_buf.ptr, res_buf.len)
        plugin.free_buffer(res_buf.ptr, res_buf.len)
        return json.loads(raw.decode("utf-8"))

    # Register
    td = tempfile.mkdtemp()
    reg = call("plugin.register", {"config_yaml": f"data_dir: {td}\nretention_days: 7\n"})
    assert reg["ok"], f"reg failed: {reg}"
    
    # 1. Intercept Request
    req_body = {
        "model": "gpt-4o",
        "messages": [
            {"role": "system", "content": "You are an assistant."},
            {"role": "user", "content": "Xin chao AI, hom nay thoi tiet Ha Noi the nao?"}
        ]
    }
    req_rpc = {
        "RequestID": "req-test-12345",
        "TraceID": "trace-test-12345",
        "Model": "gpt-4o",
        "Body": list(json.dumps(req_body).encode("utf-8"))
    }
    res_int = call("request.intercept_before", req_rpc)
    assert res_int["ok"]
    
    # 2. Intercept Response
    resp_body = {
        "choices": [
            {"message": {"role": "assistant", "content": "Chao ban! Hom nay thoi tiet Ha Noi rat dep va mat me."}}
        ]
    }
    resp_rpc = {
        "RequestID": "req-test-12345",
        "Body": list(json.dumps(resp_body).encode("utf-8"))
    }
    res_resp = call("response.intercept_after", resp_rpc)
    assert res_resp["ok"]
    
    # Allow async SQLite write
    time.sleep(0.3)
    
    # 3. Query via management /payload API
    query_rpc = {
        "Method": "GET",
        "Path": "/v0/management/plugins/usage-statistics/payload",
        "Query": {"id": ["req-test-12345"]}
    }
    mgmt_res = call("management.handle", query_rpc)
    assert mgmt_res["ok"]
    res_payload = json.loads(mgmt_res["result"])
    body = res_payload.get("Body", {})
    if isinstance(body, str):
        body = json.loads(body)
    
    assert body.get("found") == True, f"expected payload found, got {body}"
    assert "Xin chao AI" in body.get("prompt_preview", "")
    assert "Chao ban! Hom nay thoi tiet" in body.get("response_preview", "")
    print("PASS: Chat payload capture, SQLite persistence and management query verified!")

if __name__ == "__main__":
    test_payload_capture()
