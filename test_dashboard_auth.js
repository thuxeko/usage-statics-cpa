#!/usr/bin/env node
/**
 * Verifies the dashboard's CPAMC key resolution without a browser.
 *
 * CPAMC obfuscates the value it writes to localStorage (zustand persist):
 *   "enc::v2::" + base64(XOR(utf8(json), utf8(SALT + "|" + host)))            current (since 2026-10-31)
 *   "enc::v1::" + base64(XOR(utf8(json), utf8(SALT + "|" + host + "|" + UA))) older
 * Older CPAMC builds wrote plain JSON. The dashboard must handle all of these,
 * prefer the current v2 entry, and must never brute-force storage (a wrong key
 * counts as a failed auth at CPA's management API and triggers an IP ban).
 *
 * Run:  node test_dashboard_auth.js
 */
const fs = require("fs");
const path = require("path");

const SALT = "cli-proxy-api-webui::secure-storage";
const PREFIX_V1 = "enc::v1::";
const PREFIX_V2 = "enc::v2::";
const HOST = "192.168.5.230:8892";
const UA = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 Chrome/120 Mobile";

Object.defineProperty(globalThis.navigator, "userAgent", { value: UA, configurable: true, writable: true });

function obfuscate(plain, version, host) {
  const saltBase = `${SALT}|${host || HOST}` + (version === 1 ? `|${UA}` : "");
  const k = [...Buffer.from(saltBase, "utf8")];
  const d = [...Buffer.from(plain, "utf8")];
  for (let i = 0; i < d.length; i++) d[i] ^= k[i % k.length];
  return (version === 1 ? PREFIX_V1 : PREFIX_V2) + Buffer.from(d).toString("base64");
}

// Extract the <script> body and keep only the auth helpers (before the theme
// section) so no DOM/network code runs in the test.
//
// This reads dashboard2.html — the page CPA actually serves. The marker must
// EXIST: an absent marker makes indexOf return -1, slice(0, -1) keeps the whole
// script, and the DOM code then runs headless and throws. Assert instead.
const PAGE = "dashboard2.html";
const THEME_MARKER = "// ---- host theme sync";
const html = fs.readFileSync(path.join(__dirname, PAGE), "utf8");
const m = html.match(/<script>([\s\S]*)<\/script>/);
if (!m) throw new Error(`no <script> block found in ${PAGE}`);
const cut = m[1].indexOf(THEME_MARKER);
if (cut < 0) throw new Error(`marker ${JSON.stringify(THEME_MARKER)} not found in ${PAGE}: cannot isolate the auth helpers`);
const authSrc = m[1].slice(0, cut);

// The auth helpers only need: window.location.host, navigator.userAgent,
// localStorage, sessionStorage, document.getElementById.
function runAuth(localEntries, sessionEntries) {
  const mk = (obj) => ({
    getItem: (k) => (k in obj ? obj[k] : null),
    setItem: (k, v) => { obj[k] = v; },
    removeItem: (k) => { delete obj[k]; },
  });
  const localStorage = mk(localEntries);
  const sessionStorage = mk(sessionEntries);
  // The shared helpers reach storage via window[name], like a real browser.
  const window = { location: { host: HOST }, localStorage, sessionStorage };
  const document = { getElementById: () => null };
  const factory = new Function(
    "window", "navigator", "document", "localStorage", "sessionStorage",
    authSrc + "\nreturn { key: cpaStoredManagementKey(), cands: managementKeyCandidates() };"
  );
  return factory(window, globalThis.navigator, document, localStorage, sessionStorage);
}

let failures = 0;
function check(label, got, want) {
  const ok = JSON.stringify(got) === JSON.stringify(want);
  if (!ok) failures++;
  console.log(`${ok ? "PASS" : "FAIL"}: ${label}` + (ok ? "" : ` (got ${JSON.stringify(got)}, want ${JSON.stringify(want)})`));
}

// 1. Current CPAMC (v2): obfuscated zustand-persist payload -> state.managementKey
let obfBlob = obfuscate(JSON.stringify({
  state: { apiBase: "http://192.168.5.230:8892/v0/management", managementKey: "OBF-KEY-123", rememberPassword: true },
  version: 3,
}), 2);
let r = runAuth({ "cli-proxy-auth": obfBlob }, {});
check("decode v2 cli-proxy-auth (state.managementKey)", r.key, "OBF-KEY-123");
check("first candidate is the CPAMC key", r.cands[0] && r.cands[0].key, "OBF-KEY-123");

// 2. Legacy CPAMC (v1): same shape, salt includes the user agent.
obfBlob = obfuscate(JSON.stringify({
  state: { apiBase: "http://192.168.5.230:8892/v0/management", managementKey: "OBF-KEY-123", rememberPassword: true },
  version: 3,
}), 1);
r = runAuth({ "cli-proxy-auth": obfBlob }, {});
check("decode v1 cli-proxy-auth (state.managementKey)", r.key, "OBF-KEY-123");

// 3. v2 and v1 both present: v2 wins (the panel deletes the v1 entry anyway).
const v2Blob = obfuscate(JSON.stringify({ state: { managementKey: "V2-WINS" }, version: 3 }), 2);
const v1Blob = obfuscate(JSON.stringify({ managementKey: "V1-LOSES" }), 1);
r = runAuth({ "cli-proxy-auth": v2Blob, "managementKey": v1Blob }, {});
check("v2 entry wins over v1", r.key, "V2-WINS");

// 4. v2 blob from a DIFFERENT host (e.g. old bookmark with :8891): no key.
const badHost = obfuscate(JSON.stringify({ state: { managementKey: "WRONG-HOST" }, version: 3 }), 2, "10.0.0.9:8892");
r = runAuth({ "cli-proxy-auth": badHost }, {});
check("v2 blob from another host -> no key", r.key, "");
check("... and no candidates either", r.cands.length, 0);

// 5. Legacy CPAMC: plain JSON, key at the top level.
r = runAuth({ "cli-proxy-auth": JSON.stringify({ managementKey: "PLAIN-KEY-9", apiBase: "x" }) }, {});
check("decode plain-JSON cli-proxy-auth (top-level managementKey)", r.key, "PLAIN-KEY-9");

// 6. Separate encrypted "managementKey" storage entry (sessionStorage, v1).
r = runAuth({}, { managementKey: obfuscate("SESSION-KEY-7", 1) });
check("decode separate sessionStorage managementKey entry", r.key, "SESSION-KEY-7");

// 7. No CPAMC session -> empty candidate list (never brute-forces storage).
r = runAuth({ "some-unrelated-key": obfuscate("NOPE", 2) }, { other: "x" });
check("no session -> no candidates (never brute-forces storage)", r.cands.length, 0);

// 8. The key-handling block is duplicated in dashboard.html and dashboard2.html
// (each page is a self-contained resource). Fail as soon as the copies drift.
const SHARED_BEGIN = "// ---- cpa management key: shared block (begin)";
const SHARED_END = "// ---- cpa management key: shared block (end)";
function sharedBlock(page) {
  const text = fs.readFileSync(path.join(__dirname, page), "utf8");
  const a = text.indexOf(SHARED_BEGIN);
  const b = text.indexOf(SHARED_END);
  if (a < 0 || b < a) throw new Error(`shared key block markers not found in ${page}`);
  return text.slice(a, b + SHARED_END.length);
}
check("dashboard.html and dashboard2.html carry the same shared key block",
  sharedBlock("dashboard.html") === sharedBlock("dashboard2.html"), true);

console.log(failures === 0 ? "\nDASHBOARD AUTH TESTS PASSED" : `\n${failures} TEST(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);