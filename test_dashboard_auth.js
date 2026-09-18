#!/usr/bin/env node
/**
 * Verifies the dashboard's CPAMC key resolution without a browser.
 *
 * CPAMC obfuscates the value it writes to localStorage:
 *   "enc::v1::" + base64(XOR(utf8(json), utf8(SALT + "|" + host + "|" + UA)))
 * Older CPAMC builds wrote plain JSON. The dashboard must handle both, and
 * must never brute-force storage (a wrong key counts as a failed auth at CPA's
 * management API and triggers an IP ban).
 *
 * Run:  node test_dashboard_auth.js
 */
const fs = require("fs");
const path = require("path");

const SALT = "cli-proxy-api-webui::secure-storage";
const PREFIX = "enc::v1::";
const HOST = "192.168.5.230:8892";
const UA = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 Chrome/120 Mobile";

Object.defineProperty(globalThis.navigator, "userAgent", { value: UA, configurable: true, writable: true });

function obfuscate(plain) {
  const k = [...Buffer.from(`${SALT}|${HOST}|${UA}`, "utf8")];
  const d = [...Buffer.from(plain, "utf8")];
  for (let i = 0; i < d.length; i++) d[i] ^= k[i % k.length];
  return PREFIX + Buffer.from(d).toString("base64");
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
const fakeWindow = { location: { host: HOST } };

function runAuth(localEntries, sessionEntries) {
  const mk = (obj) => ({
    getItem: (k) => (k in obj ? obj[k] : null),
    setItem: (k, v) => { obj[k] = v; },
    removeItem: (k) => { delete obj[k]; },
  });
  const localStorage = mk(localEntries);
  const sessionStorage = mk(sessionEntries);
  const document = { getElementById: () => null };
  const factory = new Function(
    "window", "navigator", "document", "localStorage", "sessionStorage",
    authSrc + "\nreturn { key: cpaStoredManagementKey(), cands: mgmtKeyCandidates() };"
  );
  return factory(fakeWindow, globalThis.navigator, document, localStorage, sessionStorage);
}

let failures = 0;
function check(label, got, want) {
  const ok = got === want;
  if (!ok) failures++;
  console.log(`${ok ? "PASS" : "FAIL"}: ${label}` + (ok ? "" : ` (got ${JSON.stringify(got)}, want ${JSON.stringify(want)})`));
}

// 1. Current CPAMC: obfuscated zustand-persist payload -> state.managementKey
const obfBlob = obfuscate(JSON.stringify({
  state: { apiBase: "http://192.168.5.230:8892/v0/management", managementKey: "OBF-KEY-123", rememberPassword: true },
  version: 3,
}));
let r = runAuth({ "cli-proxy-auth": obfBlob }, {});
check("decode obfuscated cli-proxy-auth (state.managementKey)", r.key, "OBF-KEY-123");
check("first candidate is the CPAMC key", r.cands[0], "OBF-KEY-123");

// 2. Legacy CPAMC: plain JSON, key at the top level.
r = runAuth({ "cli-proxy-auth": JSON.stringify({ managementKey: "PLAIN-KEY-9", apiBase: "x" }) }, {});
check("decode plain-JSON cli-proxy-auth (top-level managementKey)", r.key, "PLAIN-KEY-9");

// 3. Separate encrypted "managementKey" storage entry (sessionStorage).
r = runAuth({}, { managementKey: obfuscate("SESSION-KEY-7") });
check("decode separate sessionStorage managementKey entry", r.key, "SESSION-KEY-7");

// 4. No CPAMC session -> empty candidate list (never brute-forces storage).
r = runAuth({ "some-unrelated-key": obfuscate("NOPE") }, { other: "x" });
check("no session -> no candidates (never brute-forces storage)", r.cands.length, 0);

console.log(failures === 0 ? "\nDASHBOARD AUTH TESTS PASSED" : `\n${failures} TEST(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
