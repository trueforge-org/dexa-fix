// Runs static/consent.js against a tiny fake DOM and checks the promises the privacy policy makes:
// nothing is loaded before Accept, Accept loads what the administrator turned on, a remembered
// choice is respected, and Decline takes the cookies back. Usage: node consent_harness.js consent.js
"use strict";
const fs = require("fs");
const vm = require("vm");

const source = fs.readFileSync(process.argv[2], "utf8");
let failures = 0;
function check(cond, what) {
  if (!cond) { failures++; console.log("FAIL: " + what); }
}

function page(opts) {
  const jar = {};
  (opts.cookies || []).forEach((c) => { const [k, v] = c.split("="); jar[k] = v; });
  const log = { images: [], scripts: [], reloads: 0 };
  const listeners = {};
  const accept = { addEventListener: (e, f) => { listeners.accept = f; } };
  const decline = { addEventListener: (e, f) => { listeners.decline = f; } };
  const opener = { addEventListener: (e, f) => { listeners.open = f; } };
  const box = {
    hidden: true,
    getAttribute: (n) => ({ "data-scarf": opts.scarf || "", "data-ga": opts.ga || "", "data-adsense": opts.ads || "" })[n] || null,
    querySelector: (s) => (s === "[data-accept]" ? accept : decline),
  };
  const document = {
    getElementById: (id) => (id === "consent" && !opts.noBox ? box : null),
    querySelectorAll: () => [opener],
    createElement: () => ({}),
    head: { appendChild: (s) => log.scripts.push(s.src + (s.crossOrigin ? " [" + s.crossOrigin + "]" : "")) },
    get cookie() { return Object.keys(jar).map((k) => k + "=" + jar[k]).join("; "); },
    set cookie(v) {
      const parts = v.split(";").map((x) => x.trim());
      const [k, val] = parts[0].split("=");
      if (parts.some((p) => p === "Max-Age=0")) { delete jar[k]; } else { jar[k] = val; log.lastCookie = v; }
    },
  };
  const window = {};
  function Image() { const img = {}; log.images.push(img); return img; }
  const ctx = vm.createContext({ document, window, location: { protocol: "https:", reload: () => { log.reloads++; } }, Image, encodeURIComponent, RegExp, Date, Object });
  vm.runInContext(source, ctx);
  return { box, listeners, log, jar, window };
}

const ALL = { scarf: "123e4567-e89b-12d3-a456-426614174000", ga: "G-ABC123XYZ9", ads: "ca-pub-1234567890123456" };

// 1. First visit: the notice is shown and nothing is loaded.
let p = page(ALL);
check(p.box.hidden === false, "the notice must be shown when there is no choice yet");
check(p.log.images.length === 0 && p.log.scripts.length === 0, "nothing may load before Accept");
check(!p.window.dataLayer, "no Google data layer before Accept");

// 2. Accept loads exactly what is turned on and remembers the choice.
p.listeners.accept();
check(p.box.hidden === true, "the notice must close on Accept");
check(p.jar.dexafix_consent === "all", "Accept must be remembered");
check(/Max-Age=15552000/.test(p.log.lastCookie) && /SameSite=Lax/.test(p.log.lastCookie) && /Secure/.test(p.log.lastCookie), "the cookie must last 180 days, be SameSite=Lax and Secure on https: " + p.log.lastCookie);
check(p.log.images.length === 1 && p.log.images[0].src === "https://static.scarf.sh/a.png?x-pxid=" + ALL.scarf, "the Scarf pixel: " + JSON.stringify(p.log.images));
check(p.log.scripts.includes("https://www.googletagmanager.com/gtag/js?id=G-ABC123XYZ9"), "Google Analytics script: " + p.log.scripts);
check(p.log.scripts.includes("https://pagead2.googlesyndication.com/pagead/js/adsbygoogle.js?client=ca-pub-1234567890123456 [anonymous]"), "AdSense script: " + p.log.scripts);
check(p.log.scripts.length === 2, "only the two scripts: " + p.log.scripts);
check(p.window.dataLayer && p.window.dataLayer.length === 2, "gtag js + config were queued");
p.listeners.accept(); // pressing it twice must not load twice
check(p.log.images.length === 1 && p.log.scripts.length === 2, "a second Accept must not load again");

// 3. Only what is configured is loaded.
p = page({ scarf: ALL.scarf });
p.listeners.accept();
check(p.log.images.length === 1 && p.log.scripts.length === 0 && !p.window.dataLayer, "with only Scarf on, nothing from Google may load");
p = page({ ga: ALL.ga });
p.listeners.accept();
check(p.log.images.length === 0 && p.log.scripts.length === 1, "with only Google Analytics on, one script and no pixel");

// 4. A remembered choice is respected without asking again.
p = page(Object.assign({ cookies: ["dexafix_consent=all"] }, ALL));
check(p.box.hidden === true, "no notice when the choice is known");
check(p.log.images.length === 1 && p.log.scripts.length === 2, "a remembered Accept loads at once");
p = page(Object.assign({ cookies: ["dexafix_consent=none"] }, ALL));
check(p.box.hidden === true, "no notice after Decline either");
check(p.log.images.length === 0 && p.log.scripts.length === 0, "a remembered Decline loads nothing");

// 5. Decline on the first visit loads nothing and does not reload.
p = page(ALL);
p.listeners.decline();
check(p.jar.dexafix_consent === "none" && p.box.hidden === true, "Decline is remembered and closes the notice");
check(p.log.images.length === 0 && p.log.scripts.length === 0 && p.log.reloads === 0, "Decline on a first visit changes nothing else");

// 6. Changing your mind after Accept removes Google's cookies and reloads without the scripts.
p = page(Object.assign({ cookies: ["dexafix_consent=all", "_ga=GA1.1.1", "_ga_ABC123=GS1", "__gads=x", "session_unrelated=keep"] }, ALL));
p.listeners.open(); // "Privacy choices" in the footer
check(p.box.hidden === false, "the footer link must reopen the notice");
p.listeners.decline();
check(p.jar.dexafix_consent === "none", "the new choice is remembered");
check(!("_ga" in p.jar) && !("_ga_ABC123" in p.jar) && !("__gads" in p.jar), "Google's cookies must be removed: " + JSON.stringify(p.jar));
check(p.jar.session_unrelated === "keep", "other cookies must be left alone");
check(p.log.reloads === 1, "the page must reload so the loaded scripts go away");

// 7. A page without the notice (signed in, or nothing turned on) does nothing and does not fail.
p = page({ noBox: true });
check(p.log.images.length === 0 && p.log.scripts.length === 0, "no notice, no loading");

if (failures) { console.log(failures + " check(s) failed"); process.exit(1); }
console.log("consent.js: all checks passed");
