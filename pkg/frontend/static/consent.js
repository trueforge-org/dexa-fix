// The choice about the optional analytics and ads (Scarf, Google Analytics, Google AdSense). Nothing is
// loaded from them until the visitor presses Accept, and the choice is kept in a cookie of ours
// (dexafix_consent: "all" or "none", 180 days). The page only contains this notice when an
// administrator turned a service on and the visitor is not signed in; the identifiers are data
// attributes of the notice, so this file has nothing to configure and the policy forbids inline code.
(function () {
  var box = document.getElementById("consent");
  if (!box) { return; }
  var NAME = "dexafix_consent";

  function read() {
    var m = document.cookie.match(new RegExp("(?:^|; )" + NAME + "=([^;]*)"));
    return m ? m[1] : "";
  }
  function write(value) {
    document.cookie = NAME + "=" + value + "; Max-Age=" + 180 * 24 * 3600 + "; Path=/; SameSite=Lax" +
      (location.protocol === "https:" ? "; Secure" : "");
  }
  function addScript(src, crossOrigin) {
    var s = document.createElement("script");
    s.async = true;
    s.src = src;
    if (crossOrigin) { s.crossOrigin = crossOrigin; }
    document.head.appendChild(s);
  }

  var loaded = false;
  function load() {
    if (loaded) { return; }
    loaded = true;
    var scarf = box.getAttribute("data-scarf");
    var ga = box.getAttribute("data-ga");
    var ads = box.getAttribute("data-adsense");
    if (scarf) {
      new Image().src = "https://static.scarf.sh/a.png?x-pxid=" + encodeURIComponent(scarf);
    }
    if (ga) {
      window.dataLayer = window.dataLayer || [];
      window.gtag = function () { window.dataLayer.push(arguments); };
      window.gtag("js", new Date());
      window.gtag("config", ga);
      addScript("https://www.googletagmanager.com/gtag/js?id=" + encodeURIComponent(ga));
    }
    if (ads) {
      addScript("https://pagead2.googlesyndication.com/pagead/js/adsbygoogle.js?client=" + encodeURIComponent(ads), "anonymous");
    }
  }
  // Take back the cookies the services set for this site. Their scripts cannot be unloaded, so the
  // page is reloaded without them.
  function forget() {
    document.cookie.split("; ").forEach(function (c) {
      var n = c.split("=")[0];
      if (n.indexOf("_ga") === 0 || n === "__gads" || n === "__gpi" || n === "__eoi") {
        document.cookie = n + "=; Max-Age=0; Path=/";
      }
    });
  }

  var choice = read();
  if (choice === "all") { load(); } else if (choice === "") { box.hidden = false; }

  box.querySelector("[data-accept]").addEventListener("click", function () {
    write("all");
    choice = "all";
    box.hidden = true;
    load();
  });
  box.querySelector("[data-decline]").addEventListener("click", function () {
    var was = choice;
    write("none");
    choice = "none";
    box.hidden = true;
    if (was === "all" || loaded) { forget(); location.reload(); }
  });
  document.querySelectorAll("[data-consent-open]").forEach(function (b) {
    b.addEventListener("click", function () { box.hidden = false; });
  });
})();
