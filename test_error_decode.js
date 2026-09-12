#!/usr/bin/env node
/**
 * Verifies the dashboard's error-log decoder against real CPA output:
 *   - HTML entities (&#34; &#39; ...) get decoded
 *   - the {"error":{...}} JSON is extracted and parsed
 *   - trace id and hints (next reset time, suggested model) are found
 */
const fs = require("fs");
const path = require("path");

const html = fs.readFileSync(path.join(__dirname, "dashboard.html"), "utf8");
const m = html.match(/<script>([\s\S]*)<\/script>/);
if (!m) throw new Error("no <script> block");
const src = m[1];

// Only the three pure functions under test: everything between the start of
// decodeHtmlEntities and the start of loadErrorLog. (Brace-counting is unsafe
// here — parseCpaError contains regex literals with braces.)
const sliceA = src.indexOf("function decodeHtmlEntities");
const sliceB = src.indexOf("function loadErrorLog");
if (sliceA < 0 || sliceB < 0 || sliceB <= sliceA) throw new Error("functions not found in expected order");
const sandbox = {};
new Function("window", "navigator", "document", "localStorage", "sessionStorage", "sandbox",
  src.slice(sliceA, sliceB) + `
  sandbox.decodeHtmlEntities = decodeHtmlEntities;
  sandbox.parseCpaError = parseCpaError;
  sandbox.extractHints = extractHints;
`)({ location: { host: "x" } }, { userAgent: "test" }, { getElementById: () => null },
   { getItem: () => null }, { getItem: () => null }, sandbox);

let failures = 0;
const check = (label, got, want) => {
  const ok = got === want;
  if (!ok) failures++;
  console.log(`${ok ? "PASS" : "FAIL"}: ${label}` + (ok ? "" : ` (got ${JSON.stringify(got)}, want ${JSON.stringify(want)})`));
};

// ---- sample exactly as CPA wrote it (user's paste) ----
const sample = `=== API RESPONSE ===
Timestamp: 2026-09-11T19:40:40.499787571+07:00
{&#34;error&#34;:{&#34;message&#34;:&#34;You&#39;ve used this period&#39;s free allowance. Your next rolling 7-day period starts at 2026-09-18T10:24:16.669877+00:00. Use the paid model &#39;deepseek-v4.1-flash&#39; to keep going, or subscribe to a Token Harbor Pass for a recurring included allowance across more models. https://tokenharbor.ai/pricing&#34;,&#34;type&#34;:&#34;free_tier_limit_reached&#34;,&#34;code&#34;:&#34;free_tier_limit_reached&#34;}}


=== RESPONSE ===
Status: 500
Content-Type: application/json
X-Cpa-Trace-Id: 20260911194036-505a61dda40414f2-59a396e3
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, PUT, PATCH, DELETE, OPTIONS
Access-Control-Allow-Headers: *
Access-Control-Expose-Headers: X-CPA-TRACE-ID, X-CPA-VERSION, X-CPA-COMMIT, X-CPA-BUILD-DATE, X-CPA-SUPPORT-PLUGIN, X-CPA-HOME-VERSION, X-CPA-HOME-BUILD-DATE, X-SERVER-VERSION, X-SERVER-BUILD-DATE, Location, Retry-After, X-Request-Id, OpenAI-Request-Id

{&#34;error&#34;:{&#34;message&#34;:&#34;You&#39;ve used this period&#39;s free allowance. Your next rolling 7-day period starts at 2026-09-18T10:24:16.669877+00:00. Use the paid model &#39;deepseek-v4.1-flash&#39; to keep going, or subscribe to a Token Harbor Pass for a recurring included allowance across more models. https://tokenharbor.ai/pricing&#34;,&#34;type&#34;:&#34;free_tier_limit_reached&#34;,&#34;code&#34;:&#34;free_tier_limit_reached&#34;}}`;

const decoded = sandbox.decodeHtmlEntities(sample);
check("decodes &#34; to quote", decoded.includes('"error"'), true);
check("decodes &#39; to apostrophe", decoded.includes("You've used"), true);
check("no entities left", /&#\d+;|&(amp|quot|apos|lt|gt);/.test(decoded), false);

const err = sandbox.parseCpaError(decoded);
check("extracts error.code", err.code, "free_tier_limit_reached");
check("extracts error.type", err.type, "free_tier_limit_reached");
check("extracts message (decoded)", err.message.includes("free allowance"), true);
check("extracts trace id", err.trace, "20260911194036-505a61dda40414f2-59a396e3");
check("raw JSON captured", err.raw.includes('"error"'), true);

const hints = sandbox.extractHints(err.message);
check("hint: next reset time found", hints.some(h => h.label === "Mở lại lúc"), true);
check("hint: suggested model found", hints.some(h => h.label === "Model gợi ý" && h.value === "deepseek-v4.1-flash"), true);

// ---- edge: plain-text error (no JSON at all) ----
const plain = sandbox.parseCpaError("=== API RESPONSE ===\nconnection reset by peer\n=== RESPONSE ===\nStatus: 502");
check("no JSON -> empty code, no crash", plain.code === "" && plain.message === "", true);

// ---- edge: already-decoded JSON log ----
const already = sandbox.parseCpaError('{"error":{"message":"rate limited","code":"429001"}}');
check("plain JSON still parsed", already.code, "429001");

console.log(failures === 0 ? "\nERROR-DECODE TESTS PASSED" : `\n${failures} TEST(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
