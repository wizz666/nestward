// Nestward web interface. Plain JavaScript, no build step.
// All text comes from /api/v1/i18n so translations live in one place.
(function () {
  "use strict";

  var state = { lang: null, strings: {}, languages: [], checks: {} };

  function savedLang() {
    try { return localStorage.getItem("nestward.lang"); } catch (e) { return null; }
  }
  function saveLang(l) {
    try { localStorage.setItem("nestward.lang", l); } catch (e) { /* private mode */ }
  }

  function t(key) { return state.strings[key] || key; }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function applyStatic(root) {
    root.querySelectorAll("[data-i18n]").forEach(function (n) {
      n.textContent = t(n.getAttribute("data-i18n"));
    });
  }

  function getJSON(url) {
    return fetch(url, { headers: { Accept: "application/json" } }).then(function (r) {
      if (!r.ok) {
        var err = new Error(r.status + " " + r.statusText);
        err.status = r.status;
        throw err;
      }
      return r.json();
    });
  }

  function loadStrings(lang) {
    var q = lang ? "?lang=" + encodeURIComponent(lang) : "";
    return getJSON("api/v1/i18n" + q).then(function (d) {
      state.lang = d.lang;
      state.strings = d.strings;
      state.languages = d.languages;
      document.documentElement.lang = d.lang;
      document.title = t("ui.title");
      applyStatic(document);
      document.getElementById("json").href = "api/v1/machines?lang=" + d.lang;
    });
  }

  var factRows = [
    ["ui.system", "system"],
    ["ui.cpu", "cpu"],
    ["ui.memory", "memory"],
    ["ui.graphics", "graphics"],
    ["ui.storage", "storage"],
    ["ui.virtualization", "virtualization"]
  ];

  function renderApp(a) {
    var li = el("li", "app v-" + a.verdict);
    var head = el("div", "app-head");
    head.appendChild(el("span", "app-name", a.name));
    head.appendChild(el("span", "badge", a.verdict_text));
    li.appendChild(head);
    li.appendChild(el("p", "note", a.note));

    if (a.suggestion) {
      var s = el("p", "suggestion");
      s.appendChild(el("strong", null, t("ui.suggestion") + ": "));
      s.appendChild(document.createTextNode(a.suggestion));
      li.appendChild(s);
    }

    var det = el("details");
    det.appendChild(el("summary", null, t("ui.details")));
    var ul = el("ul", "reasons");
    (a.reasons || []).forEach(function (r) {
      ul.appendChild(el("li", "r-" + r.effect, r.text));
    });
    det.appendChild(ul);
    if (a.verdict !== "ok") det.open = true;
    li.appendChild(det);
    return li;
  }

  // Fills {name} placeholders in a translated text.
  function fill(text, vals) {
    return text.replace(/\{(\w+)\}/g, function (all, k) { return vals[k] != null ? vals[k] : all; });
  }

  function serviceURL(m, web) {
    var host = web.host || m.address || location.hostname;
    if (host.indexOf(":") >= 0) host = "[" + host + "]"; // IPv6
    return web.scheme + "://" + host + ":" + web.port + (web.path || "");
  }

  // gb shows bytes as gigabytes with one decimal, in the page's language.
  function gb(bytes) {
    var v = (bytes || 0) / 1073741824;
    return v.toLocaleString(document.documentElement.lang || undefined, { maximumFractionDigits: v < 10 ? 1 : 0 }) + " GB";
  }

  function renderService(m, s, helpers) {
    var li = el("li", "svc k-" + s.kind + (s.state === "running" ? "" : " stopped"));
    li.dataset.svc = m.id + "/" + s.name;
    var icon = el("span", "svc-icon c-" + (s.category || s.kind), (s.app_name || s.name).charAt(0).toUpperCase());
    icon.setAttribute("aria-hidden", "true");
    li.appendChild(icon);

    var body = el("div", "svc-body");
    var title = el("div", "svc-title");
    title.appendChild(el("span", "svc-name", s.app_name || s.name));
    var stateText = t("state." + s.state);
    if (s.state === "running" && s.health && s.health !== "healthy") stateText = t("health." + s.health);
    var st = el("span", "svc-state s-" + (s.health === "unhealthy" ? "bad" : s.state), stateText);
    title.appendChild(st);
    body.appendChild(title);

    var sub = [];
    var g = s.guest;
    if (g) {
      // A Proxmox virtual machine or container.
      sub.push(fill(t("ui.guest_" + g.type), { id: g.vmid }));
      if (s.kind !== "guest" && s.category) sub.push(t("category." + s.category));
      if (g.name && g.name.toLowerCase() !== (s.app_name || "").toLowerCase()) sub.push(g.name);
    } else {
      if (s.kind === "own") sub.push(t("ui.own_app"));
      else if (s.kind === "unknown") sub.push(t("ui.unknown_app"));
      else if (s.category) sub.push(t("category." + s.category));
      if ((s.app_name || "").toLowerCase() !== s.name.toLowerCase()) sub.push(s.name);
    }
    body.appendChild(el("div", "svc-sub", sub.join(" · ")));
    if (g && s.state === "running" && g.mem_max) {
      var res = fill(t("ui.guest_res"), { cpus: g.cpus || "?", mem: gb(g.mem_used) + " / " + gb(g.mem_max) });
      if (g.disk_used && g.disk_max) res += " · " + fill(t("ui.guest_disk"), { disk: gb(g.disk_used) + " / " + gb(g.disk_max) });
      body.appendChild(el("div", "svc-res", res));
    }

    if (g && g.backup_checked && s.state === "running") {
      if (!g.last_backup) body.appendChild(el("div", "svc-warn", t("ui.no_backup")));
      else body.appendChild(el("div", "svc-res", fill(t("ui.backup_at"), { date: new Date(g.last_backup).toLocaleDateString(document.documentElement.lang || undefined) })));
    }
    var rt = g && g.restore_test;
    if (rt) {
      var rdate = new Date(rt.tested_at).toLocaleDateString(document.documentElement.lang || undefined);
      var rkey = rt.result === "ok" ? (rt.proof === "restored" ? "ui.restore_only" : "ui.restore_ok")
        : rt.result === "failed" ? "ui.restore_failed" : "ui.restore_skipped";
      var rl = el("div", rt.result === "failed" ? "svc-warn" : "svc-res", fill(t(rkey), { date: rdate }));
      rl.title = rt.stage ? t("restore.stage." + rt.stage) + (rt.detail ? " — " + rt.detail : "") : t("ui.restore_hint");
      body.appendChild(rl);
    }
    if (s.state === "running" && s.autostart === "no") {
      var na = el("div", "svc-warn", t("ui.no_autostart"));
      na.title = t("ui.no_autostart_hint");
      body.appendChild(na);
    }
    if (s.login === "none") {
      var warn = el("div", "svc-warn", t("ui.no_login"));
      warn.title = t("ui.no_login_hint");
      body.appendChild(warn);
    }
    if (helpers.length) {
      body.appendChild(el("div", "svc-helpers", t("ui.helpers") + ": " + helpers.map(function (h) {
        if (h.app_name && h.app_name !== s.app_name) return h.app_name;
        // "nextcloud-aio-talk" in group "nextcloud-aio" -> "talk"
        var prefix = (h.group || "") + "-";
        return h.group && h.name.indexOf(prefix) === 0 ? h.name.slice(prefix.length) : h.name;
      }).join(", ")));
    }
    li.appendChild(body);

    // Monitoring: the container or its web page is not working.
    // When the whole machine is silent its last results are old news: do
    // not paint every service red (the machine itself shows the problem).
    var silent = state.checks["machine/" + m.id] && state.checks["machine/" + m.id].status === "down";
    var bad = silent ? null : [state.checks["container/" + m.id + "/" + s.name], state.checks["web/" + m.id + "/" + s.name]]
      .filter(function (c) { return c && c.status === "down"; })[0];
    if (bad) {
      li.classList.add("svc-down");
      body.appendChild(el("div", "svc-alarm", fill(t("ui.status_down_since"), { time: clock(bad.since) })));
    }
    // A page that never answered is not watched: most likely it lives at
    // another address than Nestward guessed. Say so, quietly.
    var wc = state.checks["web/" + m.id + "/" + s.name];
    if (!silent && !bad && s.state === "running" && wc && wc.status === "stopped") {
      var nc = el("div", "svc-res", t("ui.web_not_checked"));
      nc.title = fill(t("ui.web_not_checked_hint"), { url: wc.target || "" });
      body.appendChild(nc);
    }

    if (s.web && s.state === "running") {
      var actions = el("div", "svc-actions");
      var a = el("a", "svc-open", t("ui.open"));
      a.href = serviceURL(m, s.web);
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      actions.appendChild(a);
      if (s.kind !== "support" && s.app_id !== "nestward" && !s.guest && NW.openPublish) {
        var pb = el("button", "svc-open svc-pub", t("pub.button"));
        pb.type = "button";
        pb.addEventListener("click", function () { NW.openPublish(m, s); });
        actions.appendChild(pb);
      }
      li.appendChild(actions);
    }
    return li;
  }

  function renderServices(m, box) {
    var d = m.discovery;
    if (!d) { box.appendChild(el("p", "muted", t("ui.no_discovery"))); return; }
    if (!d.available) {
      box.appendChild(el("p", "muted", d.error === "docker_not_found" ? t("ui.docker_not_found") :
        fill(t("ui.docker_error"), { error: d.error })));
      return;
    }
    if (!d.services.length) { box.appendChild(el("p", "muted", t("ui.services_none"))); return; }
    var st = d.stats;
    if (st.total) {
      box.appendChild(el("p", "svc-stats", fill(t("ui.services_stats"), {
        recognized: st.recognized, "public": st.total - st.own, own: st.own, running: st.running
      })));
    }
    if (st.guests) {
      box.appendChild(el("p", "svc-stats", fill(t("ui.guests_stats"), { guests: st.guests, running: st.guests_running || 0 })));
    }
    var helpersOf = {};
    d.services.forEach(function (s) {
      if (s.kind === "support" && s.part_of) (helpersOf[s.part_of] = helpersOf[s.part_of] || []).push(s);
    });
    var ul = el("ul", "svc-grid");
    d.services.forEach(function (s) {
      if (s.kind === "support" && s.part_of) return; // shown under its app
      var helpers = helpersOf[s.app_name] || [];
      delete helpersOf[s.app_name]; // list each group's helpers once
      ul.appendChild(renderService(m, s, helpers));
    });
    box.appendChild(ul);
  }

  function renderMachine(m) {
    var tpl = document.getElementById("machine-tpl");
    var node = tpl.content.cloneNode(true);
    applyStatic(node);
    var r = m.report;
    node.querySelector(".machine").dataset.machine = m.id;
    node.querySelector(".m-title").textContent = r.machine.title;
    var src = m.source === "local" ? t("ui.this_machine") : t("ui.last_seen") + ": " +
      new Date(m.last_seen).toLocaleString(state.lang);
    node.querySelector(".m-source").textContent = src;
    var mc = state.checks["machine/" + m.id];
    if (mc && mc.status === "down") {
      var head = node.querySelector(".machine-head");
      head.appendChild(el("span", "svc-alarm", fill(t("ui.status_down_since"), { time: clock(mc.since) })));
    }
    node.querySelector(".summary").textContent = r.summary;

    var dl = node.querySelector(".facts");
    factRows.forEach(function (row) {
      var wrap = el("div");
      wrap.appendChild(el("dt", null, t(row[0])));
      wrap.appendChild(el("dd", null, r.machine[row[1]]));
      dl.appendChild(wrap);
    });

    renderServices(m, node.querySelector(".services"));

    var apps = node.querySelector(".apps");
    r.apps.forEach(function (a) { apps.appendChild(renderApp(a)); });
    return node;
  }

  function clock(iso) {
    var d = new Date(iso);
    if (isNaN(d) || d.getFullYear() < 2000) return "–";
    var sameDay = d.toDateString() === new Date().toDateString();
    var time = d.toLocaleTimeString(state.lang, { hour: "2-digit", minute: "2-digit" });
    return sameDay ? time : d.toLocaleDateString(state.lang) + " " + time;
  }

  function checkName(c) {
    var n = c.app || c.service || c.machine;
    return c.kind === "machine" || !c.machine ? n : n + " (" + c.machine + ")";
  }

  // Scrolls to something on the page and makes it blink once.
  function spotlight(node) {
    if (!node) return;
    node.scrollIntoView({ behavior: "smooth", block: "center" });
    node.classList.remove("spot");
    void node.offsetWidth;
    node.classList.add("spot");
  }

  function healthAction(a) {
    if (a.kind === "alerts_setup" || a.kind === "heartbeat_setup") {
      if (NW.openSettings) NW.openSettings(a.kind);
      return;
    }
    if (a.kind === "show_service") spotlight(document.querySelector('.svc[data-svc="' + CSS.escape(a.target) + '"]'));
    if (a.kind === "show_machine") spotlight(document.querySelector('.machine[data-machine="' + CSS.escape(a.target) + '"]'));
    if (a.kind === "show_publications") spotlight(document.getElementById("publications"));
    if (a.kind === "show_exposure" && NW.showExposure) NW.showExposure(a.target);
  }

  function renderHealth(h) {
    var box = document.getElementById("health");
    if (!h) { box.hidden = true; return; }
    box.hidden = false;
    box.className = "panel health g-" + h.grade;
    var head = el("div", "health-head");
    var ring = el("div", "health-score");
    ring.style.setProperty("--p", h.score);
    ring.appendChild(el("span", "health-num", String(h.score)));
    ring.setAttribute("role", "img");
    ring.setAttribute("aria-label", h.score + " / 100");
    var words = el("div");
    words.appendChild(el("h2", null, t("ui.health_heading")));
    words.appendChild(el("p", "health-grade", h.grade_text));
    head.append(ring, words);
    box.replaceChildren(head);

    var ul = el("ul", "health-items");
    if (!h.items.length) ul.appendChild(el("li", "muted", t("ui.health_none")));
    h.items.forEach(function (it) {
      var li = el("li", "health-item");
      li.appendChild(el("span", "health-pts", fill(t("ui.health_points"), { points: it.points })));
      var txt = el("div", "health-text");
      txt.appendChild(el("div", null, it.text));
      txt.appendChild(el("div", "muted small", it.fix));
      li.appendChild(txt);
      if (it.action) {
        var b = el("button", "ghost small", t("health.btn." + it.action.kind));
        b.type = "button";
        b.addEventListener("click", function () { healthAction(it.action); });
        li.appendChild(b);
      }
      ul.appendChild(li);
    });
    box.appendChild(ul);
    if (h.good.length) {
      var gl = el("ul", "health-good");
      h.good.forEach(function (g) { gl.appendChild(el("li", null, "✓ " + g)); });
      box.appendChild(gl);
    }
  }

  function renderOverview(st) {
    var box = document.getElementById("overview");
    if (!st || !st.summary.total) { box.hidden = true; return; }
    var s = st.summary;
    box.hidden = false;
    box.className = "overview " + (s.down ? "bad" : "good");
    var down = st.checks.filter(function (c) { return c.status === "down"; });
    var title = s.down ? fill(t("ui.status_problems"), { count: s.down }) : t("ui.status_all_ok");
    var detail = fill(t("ui.status_checks"), { up: s.up, total: s.up + s.down });
    box.replaceChildren(el("strong", null, title), el("span", null, " · " + detail));
    if (down.length) {
      var seen = {};
      box.appendChild(el("div", "overview-list", down.map(function (c) {
        return checkName(c);
      }).filter(function (n) { return seen[n] ? false : (seen[n] = true); }).join(", ")));
    }
  }

  function eventText(e) {
    var name = e.kind === "machine" ? e.machine : checkName(e);
    var key = e.kind === "machine" ? (e.to === "down" ? "ui.event_machine_down" : "ui.event_machine_up")
      : (e.to === "down" ? "ui.event_down" : "ui.event_up");
    return fill(t(key), { name: name });
  }

  function renderAlerts(st, events) {
    var panel = document.getElementById("alerts");
    if (!st) { panel.hidden = true; return; }
    panel.hidden = false;
    var a = st.alerts || {};
    document.getElementById("alerts-target").textContent = a.configured ?
      fill(t("ui.alerts_to"), { target: a.target }) : t("ui.alerts_not_set");
    var err = document.getElementById("alerts-error");
    err.hidden = !a.last_error;
    err.textContent = a.last_error ? fill(t("ui.alerts_last_error"), { error: a.last_error }) : "";
    document.getElementById("test-alert").disabled = !a.configured;

    var hb = st.heartbeat || {};
    var hbText = !hb.configured ? t("ui.heartbeat_not_set") :
      hb.last_error ? fill(t("ui.heartbeat_error"), { error: hb.last_error }) :
      fill(t("ui.heartbeat_ok"), { time: hb.last_ok ? clock(hb.last_ok) : "–" });
    var hbEl = document.getElementById("heartbeat");
    hbEl.textContent = hbText;
    hbEl.className = hb.configured && !hb.last_error ? "" : "warn-text";

    var ul = document.getElementById("events");
    ul.replaceChildren();
    if (!events.length) { ul.appendChild(el("li", "muted", t("ui.events_none"))); return; }
    events.slice(0, 12).forEach(function (e) {
      var li = el("li", "ev ev-" + e.to);
      li.appendChild(el("span", "ev-time", clock(e.at)));
      li.appendChild(el("span", null, eventText(e)));
      ul.appendChild(li);
    });
  }

  function loadMachines() {
    var main = document.getElementById("main");
    if (!main.querySelector(".machine")) main.replaceChildren(el("p", "status", t("ui.loading")));
    var status = getJSON("api/v1/status").catch(function () { return null; });
    var events = getJSON("api/v1/events?limit=12").catch(function () { return []; });
    var pubs = getJSON("api/v1/publications").catch(function () { return null; });
    var hl = getJSON("api/v1/health?lang=" + encodeURIComponent(state.lang)).catch(function () { return null; });
    var agents = getJSON("api/v1/agents").catch(function () { return null; });
    var exposure = getJSON("api/v1/exposure").catch(function () { return null; });
    var machines = getJSON("api/v1/machines?lang=" + encodeURIComponent(state.lang));
    return Promise.all([machines, status, events, pubs, hl, agents, exposure]).then(function (res) {
      renderHealth(res[4]);
      if (NW.renderAgents) NW.renderAgents(res[5]);
      var st = res[1];
      state.checks = {};
      if (st) st.checks.forEach(function (c) { state.checks[c.id] = c; });
      renderOverview(st);
      renderAlerts(st, res[2] || []);
      if (NW.renderPublications) NW.renderPublications(res[3]);
      if (NW.renderExposure) NW.renderExposure(res[6]);
      var frag = document.createDocumentFragment();
      res[0].forEach(function (m) { frag.appendChild(renderMachine(m)); });
      main.replaceChildren(frag);
    }).catch(function (err) {
      if (err && err.status === 401) { checkAuth(); return; }
      main.replaceChildren(el("p", "status error", t("ui.error")));
    });
  }

  document.getElementById("test-alert").addEventListener("click", function () {
    var btn = this, out = document.getElementById("test-result");
    btn.disabled = true;
    out.textContent = "";
    fetch("api/v1/alerts/test", { method: "POST", headers: { "X-Nestward": "1" } }).then(function (r) {
      return r.json().then(function (body) {
        out.textContent = r.ok ? t("ui.alerts_test_sent") : fill(t("ui.alerts_test_failed"), { error: body.error || r.status });
      });
    }).catch(function (e) {
      out.textContent = fill(t("ui.alerts_test_failed"), { error: e.message });
    }).then(function () {
      setTimeout(function () { btn.disabled = false; }, 20000);
    });
  });

  // Keep the page fresh while it is open.
  setInterval(function () {
    var dlg = document.getElementById("pub-dialog");
    if (!document.hidden && state.lang && !state.locked && !(dlg && dlg.open)) loadMachines();
  }, 30000);

  // Shows the login or setup page when needed, the overview otherwise.
  function checkAuth() {
    return getJSON("api/v1/auth/state").then(function (st) {
      if (st.setup_required || !st.logged_in) {
        state.locked = true;
        NW.showAuth(st);
        return;
      }
      state.locked = false;
      var authBox = document.getElementById("auth");
      authBox.hidden = true;
      authBox.replaceChildren(); // do not keep password fields around
      document.getElementById("logout").hidden = !st.enabled;
      document.getElementById("refresh").hidden = false;
      return loadMachines();
    });
  }

  var NW = window.NW = window.NW || {};
  NW.t = t;
  NW.el = el;
  NW.fill = fill;
  NW.getJSON = getJSON;
  NW.clock = clock;
  NW.spotlight = spotlight;
  NW.reload = function () { return loadMachines(); };
  NW.afterLogin = function () { return checkAuth(); };

  function start(lang) {
    return loadStrings(lang).then(checkAuth).catch(function () {
      document.getElementById("main").replaceChildren(el("p", "status error",
        "Could not load Nestward. Is it running? / Det gick inte att nå Hemvakten."));
    });
  }

  // "Check again" runs every check now, then shows the fresh result.
  document.getElementById("refresh").addEventListener("click", function () {
    var btn = this;
    btn.disabled = true;
    btn.textContent = t("ui.checking");
    var done = t("ui.refresh");
    fetch("api/v1/check", { method: "POST", headers: { "X-Nestward": "1" } })
      .catch(function () { return null; })
      .then(function () { return loadMachines(); })
      .then(function () { done = fill(t("ui.checked_at"), { time: clock(new Date().toISOString()) }); })
      .catch(function () { done = t("ui.error"); })
      .then(function () {
        btn.textContent = done;
        setTimeout(function () { btn.disabled = false; btn.textContent = t("ui.refresh"); }, 4000);
      });
  });
  document.getElementById("lang").addEventListener("click", function () {
    var langs = state.languages.length ? state.languages : ["en", "sv"];
    var next = langs[(langs.indexOf(state.lang) + 1) % langs.length];
    saveLang(next);
    start(next);
  });

  getJSON("healthz").then(function (h) {
    document.getElementById("version").textContent = "v" + h.version;
  }).catch(function () {});

  start(savedLang());
})();
