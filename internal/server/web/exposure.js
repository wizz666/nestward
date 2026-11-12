// "Seen from the internet": every public address and what a stranger
// meets there. Uses helpers from app.js and publish.js (window.NW).
(function () {
  "use strict";
  var NW = window.NW = window.NW || {};
  function t(k) { return NW.t(k); }
  function el(tag, cls, text) { return NW.el(tag, cls, text); }
  function fill(s, v) { return NW.fill(s, v); }

  var showAll = false;
  var last = null;
  var poll = null;

  function cls(f) {
    if (f.accepted) return "x-accepted";
    return "x-" + f.severity;
  }

  function sourceName(s) {
    if (!s) return "";
    if (s.indexOf("tunnel:") === 0) return "Cloudflare Tunnel " + s.slice(7);
    return s === "manual" ? t("exp.extra_label").toLowerCase() : s;
  }

  function item(f) {
    var li = el("li", "xp " + cls(f));
    li.dataset.host = f.host;
    var head = el("div", "pub-head");
    var a = el("a", "pub-host", f.host);
    a.href = "https://" + f.host; a.target = "_blank"; a.rel = "noopener noreferrer";
    head.appendChild(a);
    head.appendChild(el("span", "badge", t("exp.verdict." + f.verdict)));
    li.appendChild(head);
    var bits = [];
    if (f.app) bits.push(f.app);
    if (f.target) bits.push("→ " + f.target);
    bits.push(fill(t("exp.via"), { source: sourceName(f.source) }));
    if (f.accepted) bits.push(t("exp.accepted"));
    li.appendChild(el("div", "muted small", bits.join(" · ")));
    if (!f.accepted && (f.severity === "critical" || f.severity === "warn")) {
      var key = f.verdict === "open" ? "health.exposed_open.fix" :
        f.verdict === "admin_login" ? "health.exposed_admin.fix" : "exp.alert.dead.body";
      li.appendChild(el("p", "small", fill(t(key), { host: f.host, app: f.app || f.host })));
    }
    if (f.severity !== "ok") {
      var row = el("div", "row");
      var b = el("button", "ghost small", t(f.accepted ? "exp.unaccept" : "exp.accept"));
      b.type = "button";
      b.addEventListener("click", function () {
        b.disabled = true;
        NW.send("POST", "api/v1/exposure/accept", { host: f.host, accepted: !f.accepted }).then(function (r) {
          b.disabled = false;
          if (r.ok) NW.reload();
        });
      });
      row.appendChild(b);
      li.appendChild(row);
    }
    return li;
  }

  function extraForm(box) {
    var f = el("form", "form xp-extra");
    var inp = document.createElement("input");
    inp.type = "text"; inp.name = "hosts"; inp.spellcheck = false;
    inp.placeholder = "vault.example.com, cloud.example.com";
    var lab = el("label", "field");
    lab.append(el("span", "field-label", t("exp.extra_label")), inp, el("span", "field-help", t("exp.extra_help")));
    var msg = el("p", "form-msg small");
    var save = el("button", "ghost small", t("exp.save"));
    f.append(lab, save, msg);
    NW.getJSON("api/v1/settings").then(function (list) {
      (list || []).forEach(function (s) { if (s.name === "exposure.hosts" && s.is_set) inp.value = s.value; });
    }).catch(function () {});
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      NW.send("POST", "api/v1/settings", { name: "exposure.hosts", value: inp.value }).then(function (r) {
        msg.className = "form-msg small" + (r.ok ? "" : " error-text");
        msg.textContent = r.ok ? t("exp.saved") : NW.errText(r.data);
      });
    });
    var d = el("details", "xp-more");
    d.appendChild(el("summary", null, t("exp.extra_label")));
    d.appendChild(f);
    box.appendChild(d);
  }

  function startPolling() {
    if (poll) return;
    poll = setInterval(function () {
      NW.getJSON("api/v1/exposure").then(function (r) {
        if (!r.running) { clearInterval(poll); poll = null; NW.reload(); }
      }).catch(function () { clearInterval(poll); poll = null; });
    }, 3000);
  }

  NW.renderExposure = function (r) {
    var panel = document.getElementById("exposure");
    if (!r) { panel.hidden = true; return; }
    last = r;
    panel.hidden = false;
    var box = panel.querySelector(".xp-body");
    box.replaceChildren();

    var top = el("div", "row");
    var check = el("button", "small", t(r.running ? "exp.checking" : "exp.check_now"));
    check.type = "button";
    check.disabled = !!r.running;
    check.addEventListener("click", function () {
      check.disabled = true;
      check.textContent = t("exp.checking");
      NW.send("POST", "api/v1/exposure/check").then(startPolling);
    });
    top.appendChild(check);
    var when = r.findings.length ? fill(t("exp.last_checked"), { time: NW.clock(r.checked_at) }) : t("exp.never");
    top.appendChild(el("span", "muted small", when));
    box.appendChild(top);
    if (r.running) startPolling();

    if (r.error) box.appendChild(el("p", "error-text small", fill(t("exp.error"), { error: r.error })));
    if (!r.findings.length && !r.running && !(r.sources || []).length) box.appendChild(el("p", "muted", t("exp.no_source")));

    if (r.findings.length) {
      box.appendChild(el("p", "xp-counts", fill(t("exp.counts"),
        { critical: r.counts.critical, warn: r.counts.warn, ok: r.counts.ok + r.counts.info })));
      var needs = r.findings.filter(function (f) { return !f.accepted && (f.severity === "critical" || f.severity === "warn"); });
      var list = showAll ? r.findings : needs;
      if (!showAll && !needs.length) box.appendChild(el("p", "ok-text", t("exp.all_good")));
      var ul = el("ul", "pubs xp-list");
      list.forEach(function (f) { ul.appendChild(item(f)); });
      box.appendChild(ul);
      var tog = el("button", "ghost small", t(showAll ? "exp.hide_ok" : "exp.show_all"));
      tog.type = "button";
      tog.addEventListener("click", function () { showAll = !showAll; NW.renderExposure(last); });
      box.appendChild(tog);
    }
    extraForm(box);
  };

  // Health button: open the list and point at the address.
  NW.showExposure = function (host) {
    var panel = document.getElementById("exposure");
    if (!panel) return;
    if (host && !panel.querySelector('.xp[data-host="' + CSS.escape(host) + '"]') && last) {
      showAll = true;
      NW.renderExposure(last);
    }
    var node = host && panel.querySelector('.xp[data-host="' + CSS.escape(host) + '"]');
    NW.spotlight(node || panel);
  };
})();
