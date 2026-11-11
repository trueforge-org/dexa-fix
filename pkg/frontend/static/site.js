// Small things for every page. No framework, no network: it only remembers, in a cookie, that a
// visitor dismissed a note, so that it does not come back for 90 days.
(function () {
  function has(name) {
    return document.cookie.split("; ").some(function (c) { return c.indexOf(name + "=") === 0; });
  }
  document.querySelectorAll("[data-dismiss-cookie]").forEach(function (note) {
    var name = note.getAttribute("data-dismiss-cookie");
    if (has(name)) { note.hidden = true; return; }
    var button = note.querySelector("[data-dismiss]");
    if (!button) { return; }
    button.addEventListener("click", function () {
      document.cookie = name + "=1; Max-Age=" + 90 * 24 * 3600 + "; Path=/; SameSite=Lax" + (location.protocol === "https:" ? "; Secure" : "");
      note.hidden = true;
    });
  });
})();
