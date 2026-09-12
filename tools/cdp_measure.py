#!/usr/bin/env python3
"""Minimal CDP client over websockets — measure page layout at mobile size."""
import json
import sys
import urllib.request

import websockets.sync.client as ws_client


def tabs(port=9222):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/json") as r:
        return json.load(r)


class CDP:
    def __init__(self, ws_url):
        self.ws = ws_client.connect(ws_url, max_size=32 * 1024 * 1024)
        self.id = 0

    def send(self, method, **params):
        self.id += 1
        self.ws.send(json.dumps({"id": self.id, "method": method, "params": params}))
        while True:
            msg = json.loads(self.ws.recv())
            if msg.get("id") == self.id:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error']}")
                return msg.get("result", {})

    def js(self, expr):
        r = self.send("Runtime.evaluate", expression=expr, returnByValue=True, awaitPromise=True)
        if "exceptionDetails" in r:
            raise RuntimeError(r["exceptionDetails"].get("text", "js error"))
        return r.get("result", {}).get("value")

    def close(self):
        self.ws.close()


def open_page(url, width=412, height=915, mobile=True, wait=2.5):
    t = None
    for tab in tabs():
        if tab["type"] == "page":
            t = tab
            break
    if t is None:
        raise RuntimeError("no page tab")
    c = CDP(t["webSocketDebuggerUrl"])
    c.send("Page.enable")
    c.send("Emulation.setDeviceMetricsOverride", width=width, height=height,
           deviceScaleFactor=2.6, mobile=mobile)
    c.send("Page.navigate", url=url)
    import time
    time.sleep(wait)
    return c


if __name__ == "__main__":
    url = sys.argv[1] if len(sys.argv) > 1 else "about:blank"
    c = open_page(url)
    print("title:", c.js("document.title"))
    print("readyState:", c.js("document.readyState"))
    c.close()
