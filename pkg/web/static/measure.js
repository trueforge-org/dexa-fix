// Suggests related body-composition values while a measurement form is filled in. A suggestion is
// the gray placeholder of an empty field, calculated from what was typed; it is used on submit
// unless a value is typed instead. Without JavaScript the forms work the same way, without
// suggestions.
(function () {
  // Each rule is [target, inputs, calculation]. Rules are tried in order until nothing new follows.
  var RULES = [
    ["fat_mass_kg", ["weight_kg", "fat_ratio_pct"], function (w, p) { return w * p / 100; }],
    ["fat_mass_kg", ["weight_kg", "fat_free_mass_kg"], function (w, ffm) { return w - ffm; }],
    ["fat_free_mass_kg", ["weight_kg", "fat_mass_kg"], function (w, fm) { return w - fm; }],
    ["fat_free_mass_kg", ["lean_mass_kg", "bone_mass_kg"], function (l, b) { return l + b; }],
    ["weight_kg", ["fat_mass_kg", "fat_free_mass_kg"], function (fm, ffm) { return fm + ffm; }],
    ["weight_kg", ["fat_mass_kg", "fat_ratio_pct"], function (fm, p) { return fm / (p / 100); }],
    ["weight_kg", ["fat_free_mass_kg", "fat_ratio_pct"], function (ffm, p) { return ffm / (1 - p / 100); }],
    ["fat_ratio_pct", ["fat_mass_kg", "weight_kg"], function (fm, w) { return fm / w * 100; }],
    ["lean_mass_kg", ["fat_free_mass_kg", "bone_mass_kg"], function (ffm, b) { return ffm - b; }],
    ["bone_mass_kg", ["fat_free_mass_kg", "lean_mass_kg"], function (ffm, l) { return ffm - l; }],
  ];

  var FIELDS = Array.from(new Set(RULES.map(function (r) { return r[0]; })));

  function plausible(field, value) {
    return isFinite(value) && value >= 0 && (field !== "fat_ratio_pct" || value <= 100);
  }

  function calculate(known) {
    var changed = true;
    while (changed) {
      changed = false;
      RULES.forEach(function (rule) {
        if (rule[0] in known || !rule[1].every(function (k) { return k in known; })) {
          return;
        }
        var value = rule[2].apply(null, rule[1].map(function (k) { return known[k]; }));
        if (plausible(rule[0], value)) {
          known[rule[0]] = value;
          changed = true;
        }
      });
    }
    return known;
  }

  function inputs(form) {
    return FIELDS.filter(function (k) { return form.elements[k]; }).map(function (k) { return form.elements[k]; });
  }

  // suggestions returns, per empty field, the value that follows from what was typed.
  function suggestions(form) {
    var known = {};
    inputs(form).forEach(function (input) {
      var value = parseFloat(input.value);
      if (!isNaN(value)) {
        known[input.name] = value;
      }
    });
    calculate(known);
    var out = {};
    inputs(form).forEach(function (input) {
      if (input.value === "" && input.name in known) {
        out[input.name] = String(Math.round(known[input.name] * 100) / 100);
      }
    });
    return out;
  }

  function refresh(form) {
    var suggested = suggestions(form);
    inputs(form).forEach(function (input) {
      input.placeholder = suggested[input.name] || "";
    });
  }

  // Delegated on the document, because htmx replaces the forms after each submit.
  document.addEventListener("input", function (e) {
    if (e.target.form && e.target.form.hasAttribute("data-calc")) {
      refresh(e.target.form);
    }
  });

  // Capture phase: htmx reads the form values in its own submit handler on the form.
  document.addEventListener("submit", function (e) {
    if (!e.target.hasAttribute("data-calc")) {
      return;
    }
    var suggested = suggestions(e.target);
    inputs(e.target).forEach(function (input) {
      if (suggested[input.name]) {
        input.value = suggested[input.name];
      }
    });
  }, true);
})();
