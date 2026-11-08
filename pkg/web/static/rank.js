// Drag-to-reorder for the method accuracy list. Without JavaScript the rank number inputs stay
// visible and the form works the same way: this script only keeps those inputs up to date.
(function () {
  document.querySelectorAll("[data-rank-list]").forEach(function (list) {
    var status = list.parentElement.querySelector("[data-rank-status]");
    var items = function () { return Array.from(list.querySelectorAll("[data-rank-item]")); };

    function renumber() {
      items().forEach(function (item, i) {
        item.querySelector("input").value = i + 1;
        item.querySelector("[data-rank-pos]").textContent = i + 1;
      });
    }

    function announce(item) {
      if (status) {
        status.textContent = item.dataset.label + " moved to position " + (items().indexOf(item) + 1) + ".";
      }
    }

    function move(item, delta) {
      var all = items();
      var target = all[all.indexOf(item) + delta];
      if (!target) return;
      list.insertBefore(item, delta < 0 ? target : target.nextSibling);
      renumber();
      announce(item);
    }

    list.classList.add("rank-enhanced");
    renumber();

    list.addEventListener("click", function (e) {
      var button = e.target.closest("[data-rank-move]");
      if (!button) return;
      move(button.closest("[data-rank-item]"), Number(button.dataset.rankMove));
      button.focus();
    });

    // Pointer events cover both mouse and touch, which native drag and drop does not.
    list.addEventListener("pointerdown", function (e) {
      var handle = e.target.closest("[data-rank-handle]");
      if (!handle) return;
      e.preventDefault();
      var item = handle.closest("[data-rank-item]");
      item.classList.add("dragging");

      function onMove(ev) {
        var next = items().find(function (other) {
          if (other === item) return false;
          var r = other.getBoundingClientRect();
          return ev.clientY < r.top + r.height / 2;
        }) || null;
        if (next !== item.nextElementSibling) {
          list.insertBefore(item, next);
          renumber();
        }
      }

      function onEnd() {
        document.removeEventListener("pointermove", onMove);
        document.removeEventListener("pointerup", onEnd);
        document.removeEventListener("pointercancel", onEnd);
        item.classList.remove("dragging");
        announce(item);
      }

      // On the document, because moving the row in the page would end a pointer capture on it.
      document.addEventListener("pointermove", onMove);
      document.addEventListener("pointerup", onEnd);
      document.addEventListener("pointercancel", onEnd);
    });
  });
})();
