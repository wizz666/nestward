// Travel mode: the check before leaving home, and the daily message while
// away. Uses helpers from app.js and publish.js (window.NW).
(function () {
  "use strict";
  var NW = window.NW = window.NW || {};
  function t(k) { return NW.t(k); }
  function el(tag, cls, text) { return NW.el(tag, cls, text); }
  function fill(s, v) { return NW.fill(s, v); }

  var mark = { ok: "✓", warn: "!", bad: "✗", manual: "?" };
  var cls = { ok: "v-ok", warn: "v-slow", bad: "v-no", manual: "v-slow" };

  function check(c) {
    var li = el("li", "tv " + cls[c.status]);
    var head = el("div", "pub-head");
    head.appendChild(el("span", "badge", mark[c.status]));
    head.appendChild(el("strong", "", c.title));
    li.appendChild(head);
    if (c.status !== "ok") li.appendChild(el("div", "muted small", c.why));
    (c.issues || []).forEach(function (s) { li.appendChild(el("div", "small", s)); });
    (c.fixes || []).forEach(function (s) { li.appendChild(el("div", "small muted", "→ " + s)); });
    (c.machines || []).forEach(function (m) {
      var lab = el("label", "tv-bios small");
      var box = document.createElement("input");
      box.type = "checkbox"; box.checked = !!m.confirmed;
      box.addEventListener("change", function () {
        box.disabled = true;
        NW.send("POST", "api/v1/travel/bios", { machine: m.id, done: box.checked }).then(function () { load(); });
      });
      lab.append(box, document.createTextNode(" " + m.name + (m.confirmed ? " — " + fill(t("travel.bios.done"), { date: m.confirmed }) : "")));
      li.appendChild(lab);
    });
    return li;
  }

  function form(r) {
    var f = el("form", "form tv-form");
    var on = document.createElement("input"); on.type = "checkbox"; on.checked = r.on;
    var until = document.createElement("input"); until.type = "date"; until.value = r.until || "";
    var hour = document.createElement("input"); hour.type = "number"; hour.min = 0; hour.max = 23; hour.value = r.hour;
    var l1 = el("label", "tv-bios"); l1.append(on, document.createTextNode(" " + t("travel.on")));
    var l2 = el("label", "field"); l2.append(el("span", "field-label", t("travel.until")), until);
    var l3 = el("label", "field"); l3.append(el("span", "field-label", t("travel.hour")), hour);
    var save = el("button", "", t("travel.save"));
    var test = el("button", "ghost small", t("travel.test")); test.type = "button";
    var msg = el("p", "form-msg small");
    var row = el("div", "row"); row.append(save, test);
    f.append(l1, l2, l3, row, msg);
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      NW.send("POST", "api/v1/travel", { on: on.checked, until: until.value, hour: parseInt(hour.value, 10) || 0 }).then(function (res) {
        msg.textContent = res.ok ? t("travel.saved") : NW.errText(res.data);
        if (res.ok) load();
      });
    });
    test.addEventListener("click", function () {
      test.disabled = true;
      NW.send("POST", "api/v1/travel/test", {}).then(function (res) {
        msg.textContent = res.ok ? t("travel.sent") : NW.errText(res.data);
        setTimeout(function () { test.disabled = false; }, 20000);
      });
    });
    return f;
  }

  function render(r) {
    var sec = document.getElementById("travel");
    var box = sec && sec.querySelector(".tv-body");
    if (!box || !r) return;
    sec.hidden = false;
    var frag = document.createDocumentFragment();
    var verdict = el("p", "tv-verdict " + (r.ready ? "v-ok" : "v-slow"));
    verdict.appendChild(el("span", "badge", t(r.ready ? "travel.ready" : "travel.not_ready")));
    frag.appendChild(verdict);
    if (r.on) {
      frag.appendChild(el("p", "small", fill(t("travel.away_since"), {
        hour: r.hour, until: r.until ? fill(t("travel.away_until"), { date: r.until }) : "" })));
    }
    var ul = el("ul", "tv-list");
    r.checks.forEach(function (c) { ul.appendChild(check(c)); });
    frag.appendChild(ul);
    frag.appendChild(form(r));
    box.replaceChildren(frag);
  }

  function load() {
    return NW.getJSON("api/v1/travel?lang=" + encodeURIComponent(document.documentElement.lang || "")).then(render).catch(function () {});
  }
  NW.loadTravel = load;
})();
