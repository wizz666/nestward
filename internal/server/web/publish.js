// Login, first-time setup and publishing. Uses helpers that app.js puts
// on window.NW before it calls anything here.
(function () {
  "use strict";
  var NW = window.NW = window.NW || {};

  function t(k) { return NW.t(k); }
  function el(tag, cls, text) { return NW.el(tag, cls, text); }
  function fill(s, v) { return NW.fill(s, v); }

  // POST/DELETE with the header that proves the request comes from this page.
  function send(method, url, body) {
    return fetch(url, {
      method: method,
      headers: { "Content-Type": "application/json", "X-Nestward": "1" },
      body: body ? JSON.stringify(body) : undefined
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (data) {
        return { ok: r.ok, status: r.status, data: data };
      });
    });
  }
  NW.send = send;

  function errText(data) {
    var key = "err." + (data && data.error || "internal");
    var s = t(key);
    if (s === key) s = t("err.internal");
    return fill(s, { detail: (data && data.detail) || "" });
  }
  NW.errText = errText;

  function field(label, input, help) {
    var wrap = el("label", "field");
    wrap.appendChild(el("span", "field-label", label));
    wrap.appendChild(input);
    if (help) wrap.appendChild(el("span", "field-help", help));
    return wrap;
  }

  function input(type, name, value, attrs) {
    var i = document.createElement("input");
    i.type = type; i.name = name;
    if (value != null) i.value = value;
    Object.keys(attrs || {}).forEach(function (k) { i.setAttribute(k, attrs[k]); });
    return i;
  }

  // ---------- Login and setup ----------

  NW.showAuth = function (st) {
    var box = document.getElementById("auth");
    document.getElementById("main").replaceChildren();
    document.getElementById("overview").hidden = true;
    document.getElementById("health").hidden = true;
    document.getElementById("agents").hidden = true;
    document.getElementById("alerts").hidden = true;
    document.getElementById("publications").hidden = true;
    document.getElementById("exposure").hidden = true;
    document.getElementById("logout").hidden = true;
    document.getElementById("refresh").hidden = true;
    box.hidden = false;
    box.replaceChildren();
    if (st.setup_required) setupStep1(box); else loginForm(box);
  };

  function msgLine() { return el("p", "form-msg error-text"); }

  function setupStep1(box) {
    box.appendChild(el("h2", null, t("auth.setup_title")));
    box.appendChild(el("p", null, t("auth.setup_intro")));
    var f = el("form", "form");
    var code = input("text", "code", "", { autocomplete: "off", required: "", spellcheck: "false" });
    var user = input("text", "username", "", { autocomplete: "username", required: "", maxlength: "64" });
    var msg = msgLine();
    f.append(field(t("auth.setup_code"), code), field(t("auth.username"), user), el("button", null, t("auth.next")), msg);
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      send("POST", "api/v1/auth/setup/start", { code: code.value, username: user.value }).then(function (r) {
        if (!r.ok) { msg.textContent = errText(r.data); return; }
        box.replaceChildren();
        setupStep2(box, code.value, user.value, r.data);
      });
    });
    box.appendChild(f);
    code.focus();
  }

  function setupStep2(box, code, username, d) {
    box.appendChild(el("h2", null, t("auth.setup_title")));
    box.appendChild(el("p", null, t("auth.scan")));
    var img = document.createElement("img");
    img.src = d.qr; img.alt = "QR"; img.className = "qr"; img.width = 200; img.height = 200;
    box.appendChild(img);
    box.appendChild(el("p", "secret", d.secret.replace(/(.{4})/g, "$1 ").trim()));
    var f = el("form", "form");
    var pw = input("password", "password", "", { autocomplete: "new-password", required: "", minlength: "10" });
    var pw2 = input("password", "password2", "", { autocomplete: "new-password", required: "" });
    var totp = input("text", "totp", "", { inputmode: "numeric", autocomplete: "one-time-code", required: "", maxlength: "6" });
    var msg = msgLine();
    f.append(field(t("auth.password"), pw), field(t("auth.password_again"), pw2), field(t("auth.totp"), totp),
      el("button", null, t("auth.create")), msg);
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      if (pw.value !== pw2.value) { msg.textContent = t("auth.mismatch"); return; }
      send("POST", "api/v1/auth/setup/finish", { code: code, username: username, password: pw.value, totp: totp.value })
        .then(function (r) {
          if (!r.ok) { msg.textContent = errText(r.data); return; }
          NW.afterLogin();
        });
    });
    box.appendChild(f);
    pw.focus();
  }

  function loginForm(box) {
    box.appendChild(el("h2", null, t("auth.login_title")));
    var f = el("form", "form");
    var user = input("text", "username", "", { autocomplete: "username", required: "" });
    var pw = input("password", "password", "", { autocomplete: "current-password", required: "" });
    var totp = input("text", "totp", "", { inputmode: "numeric", autocomplete: "one-time-code", required: "", maxlength: "6" });
    var msg = msgLine();
    f.append(field(t("auth.username"), user), field(t("auth.password").replace(/ \(.*\)/, ""), pw),
      field(t("auth.totp"), totp), el("button", null, t("auth.login")), msg);
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      send("POST", "api/v1/auth/login", { username: user.value, password: pw.value, totp: totp.value }).then(function (r) {
        if (!r.ok) { msg.textContent = errText(r.data); totp.value = ""; return; }
        NW.afterLogin();
      });
    });
    box.appendChild(f);
    box.appendChild(el("p", "muted small", t("auth.lost")));
    user.focus();
  }

  document.getElementById("logout").addEventListener("click", function () {
    send("POST", "api/v1/auth/logout").then(function () { location.reload(); });
  });

  // ---------- Publishing ----------

  var videoApps = { plex: true, jellyfin: true, emby: true };
  var dialog = document.getElementById("pub-dialog");

  function slug(s) {
    return (s || "").toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 63) || "app";
  }

  function defaultTarget(m, s, opts) {
    var host = s.web.host || m.address || opts.lan_address || location.hostname;
    if (host.indexOf(":") >= 0) host = "[" + host + "]";
    return s.web.scheme + "://" + host + ":" + s.web.port;
  }

  function pubURL(p) { return p.url || ("https://" + p.hostname); }

  function stepList(p) {
    var ul = el("ul", "steps");
    (p.steps || []).forEach(function (s) {
      var key = "pub.step." + s.action;
      var text = t(key) === key ? s.action : t(key);
      ul.appendChild(el("li", s.ok ? "ok" : "bad", (s.ok ? "✓ " : "✗ ") + text + (s.detail ? " – " + s.detail : "")));
    });
    return ul;
  }

  function checkText(ch) {
    return fill(t("pub.check." + ch.result), { detail: ch.detail || ("HTTP " + ch.status) });
  }

  function checkButton(p, out) {
    var b = el("button", "ghost small", t("pub.check"));
    b.type = "button";
    b.addEventListener("click", function () {
      b.disabled = true;
      out.textContent = t("pub.checking");
      send("POST", "api/v1/publications/" + p.id + "/check").then(function (r) {
        b.disabled = false;
        out.textContent = r.ok ? checkText(r.data) : errText(r.data);
        out.className = "check-result " + (r.ok && r.data.result !== "unreachable" ? "ok-text" : "warn-text");
      });
    });
    return b;
  }

  function setupForm(body, opts, then) {
    body.appendChild(el("h2", null, t("pub.setup_title")));
    body.appendChild(el("p", "small", t("pub.setup_intro")));
    var f = el("form", "form");
    var tok = input("password", "token", "", { autocomplete: "off", required: "" });
    var mails = input("text", "emails", (opts.emails || []).join(", "), { required: "" });
    var msg = msgLine();
    var actions = el("div", "row");
    var cancel = el("button", "ghost", t("pub.cancel"));
    cancel.type = "button";
    cancel.addEventListener("click", function () { dialog.close(); });
    actions.append(el("button", null, t("pub.save")), cancel);
    f.append(field(t("pub.token"), tok, t("pub.token_help")), field(t("pub.emails"), mails), actions, msg);
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      send("POST", "api/v1/settings", { name: "cloudflare.token", value: tok.value }).then(function (r) {
        if (!r.ok) { msg.textContent = errText(r.data); return; }
        return send("POST", "api/v1/settings", { name: "publish.emails", value: mails.value }).then(function (r2) {
          if (!r2.ok) { msg.textContent = errText(r2.data); return; }
          then();
        });
      });
    });
    body.appendChild(f);
  }

  NW.openPublish = function (m, s) {
    var body = dialog.querySelector(".dialog-body");
    body.replaceChildren(el("p", "muted", t("ui.loading")));
    dialog.showModal();
    NW.getJSON("api/v1/publish/options").then(function (opts) {
      body.replaceChildren();
      if (!opts.tailscale) { cloudflareFlow(body, m, s, opts); return; }
      chooseProvider(body, m, s, opts);
    }).catch(function () {
      body.replaceChildren(el("p", "error-text", t("ui.error")));
    });
  };

  function cloudflareFlow(body, m, s, opts) {
    // A token that does not work can be replaced right here.
    if (!opts.configured || opts.error) {
      if (opts.error) body.appendChild(el("p", "error-text", fill(t("pub.cf_error"), { error: opts.error })));
      setupForm(body, opts, function () { NW.openPublish(m, s); });
      return;
    }
    publishForm(body, m, s, opts);
  }

  // First question: who should reach the app?
  function chooseProvider(body, m, s, opts) {
    var app = s.app_name || s.name;
    body.appendChild(el("h2", null, fill(t("pub.title_any"), { app: app })));
    body.appendChild(el("p", "small", t("pub.choose_how")));
    var video = videoApps[s.app_id];
    var choices = el("div", "choices");
    function choice(key, recommended, go) {
      var b = el("button", "choice");
      b.type = "button";
      b.appendChild(el("strong", null, t("pub.via." + key)));
      b.appendChild(el("span", "small", t("pub.via." + key + "_help")));
      if (recommended) b.appendChild(el("span", "badge b-lock", t("pub.recommended")));
      b.addEventListener("click", function () { body.replaceChildren(); go(); });
      choices.appendChild(b);
    }
    choice("tailscale", video, function () { tailscaleFlow(body, m, s, opts); });
    choice("cloudflare", false, function () { cloudflareFlow(body, m, s, opts); });
    body.appendChild(choices);
    var close = el("button", "ghost", t("pub.cancel"));
    close.type = "button";
    close.addEventListener("click", function () { dialog.close(); });
    body.appendChild(close);
  }

  function tailscaleFlow(body, m, s, opts) {
    if (!opts.tailscale_configured) {
      body.appendChild(el("h2", null, t("pub.ts_setup_title")));
      body.appendChild(el("p", "small", t("pub.ts_setup_intro")));
      var f = el("form", "form");
      var key = input("password", "tskey", "", { autocomplete: "off", required: "", spellcheck: "false" });
      var msg = msgLine();
      var row = el("div", "row");
      var cancel = el("button", "ghost", t("pub.cancel"));
      cancel.type = "button";
      cancel.addEventListener("click", function () { dialog.close(); });
      row.append(el("button", null, t("pub.save")), cancel);
      var api = input("password", "tsapi", "", { autocomplete: "off", spellcheck: "false" });
      f.append(field(t("pub.ts_key"), key, t("pub.ts_key_help")), field(t("pub.ts_api"), api, t("pub.ts_api_help")), row, msg);
      f.addEventListener("submit", function (e) {
        e.preventDefault();
        saveAll([["tailscale.authkey", key.value.trim()], ["tailscale.apikey", api.value.trim()]]).then(function (r) {
          if (!r.ok) { msg.textContent = errText(r.data); return; }
          opts.tailscale_configured = true;
          body.replaceChildren();
          tailscaleForm(body, m, s, opts);
        });
      });
      body.appendChild(f);
      key.focus();
      return;
    }
    tailscaleForm(body, m, s, opts);
  }

  function tailscaleForm(body, m, s, opts) {
    var app = s.app_name || s.name;
    body.appendChild(el("h2", null, fill(t("pub.ts_title"), { app: app })));
    body.appendChild(el("p", "small", t("pub.ts_intro")));
    var f = el("form", "form");
    var name = input("text", "name", slug(s.app_id || s.name), { required: "", pattern: "[a-z0-9]([a-z0-9-]*[a-z0-9])?", maxlength: "63" });
    var target = input("url", "target", defaultTarget(m, s, opts), { required: "" });
    var submit = el("button", null, t("pub.submit"));
    var cancel = el("button", "ghost", t("pub.cancel"));
    cancel.type = "button";
    cancel.addEventListener("click", function () { dialog.close(); });
    var row = el("div", "row");
    row.append(submit, cancel);
    var msg = msgLine();
    f.append(field(t("pub.ts_name"), name, t("pub.ts_name_help")),
      field(t("pub.target"), target, t("pub.ts_target_help")), row, msg);
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      submit.disabled = true;
      msg.className = "form-msg muted";
      msg.textContent = t("pub.ts_working");
      send("POST", "api/v1/publications", {
        provider: "tailscale", name: name.value, target: target.value,
        machine_id: m.id, service: s.name, app: s.app_name, app_id: s.app_id, login: s.login || ""
      }).then(function (r) {
        submit.disabled = false;
        if (!r.ok) {
          msg.className = "form-msg error-text";
          msg.textContent = errText(r.data);
          return;
        }
        showDone(body, r.data);
        NW.reload();
      });
    });
    body.appendChild(f);
    name.focus();
  }

  function publishForm(body, m, s, opts) {
    var app = s.app_name || s.name;
    body.appendChild(el("h2", null, fill(t("pub.title"), { app: app })));
    body.appendChild(el("p", "small", t("pub.intro")));
    var f = el("form", "form");

    var name = input("text", "name", slug(s.app_id || s.name), { required: "", pattern: "[a-z0-9]([a-z0-9-]*[a-z0-9])?", maxlength: "63" });
    // Domain and tunnel are never chosen for the user: publishing on the
    // wrong domain is easy to do and hard to notice.
    function choose(sel) {
      var o = new Option(t("pub.choose"), "");
      o.disabled = true;
      o.selected = true;
      sel.appendChild(o);
      sel.required = true;
    }
    var zone = document.createElement("select");
    choose(zone);
    opts.zones.forEach(function (z) { zone.appendChild(new Option(z.name, z.id)); });
    var nameRow = el("div", "row nowrap");
    nameRow.append(name, el("span", "muted", "."), zone);

    var tunnel = document.createElement("select");
    choose(tunnel);
    opts.tunnels.forEach(function (tn) {
      var label = tn.name;
      if (tn.config_src !== "cloudflare") label = fill(t("pub.tunnel_local"), { name: tn.name });
      else if (tn.status !== "healthy" && tn.status !== "degraded") label = fill(t("pub.tunnel_down"), { name: tn.name });
      var o = new Option(label, tn.id);
      o.disabled = tn.config_src !== "cloudflare";
      tunnel.appendChild(o);
    });

    var target = input("url", "target", defaultTarget(m, s, opts), { required: "" });

    var mustLogin = s.login !== "own";
    var login = input("checkbox", "access", null, {});
    login.checked = mustLogin;
    login.disabled = mustLogin;
    var loginWrap = el("label", "check");
    loginWrap.append(login, el("span", null, t("pub.login")));
    var loginNote = el("p", "small " + (mustLogin ? "muted" : "warn-text"),
      fill(t(mustLogin ? "pub.login_forced" : "pub.login_own"), { app: app }));
    var mails = input("text", "emails", (opts.emails || []).join(", "), {});
    var mailsField = field(t("pub.emails"), mails);
    function syncLogin() { mailsField.hidden = !login.checked; }
    login.addEventListener("change", syncLogin);
    syncLogin();

    var video = null;
    var parts = [field(t("pub.name"), nameRow), field(t("pub.tunnel"), tunnel),
      field(t("pub.target"), target, t("pub.target_help")), loginWrap, loginNote, mailsField];
    if (videoApps[s.app_id]) {
      video = input("checkbox", "video", null, {});
      var vw = el("label", "check warn-text");
      vw.append(video, el("span", null, fill(t("pub.video"), { app: app })));
      parts.push(vw);
    }
    var submit = el("button", null, t("pub.submit"));
    var cancel = el("button", "ghost", t("pub.cancel"));
    cancel.type = "button";
    cancel.addEventListener("click", function () { dialog.close(); });
    var actions = el("div", "row");
    actions.append(submit, cancel);
    var msg = msgLine();
    parts.push(actions, msg);
    parts.forEach(function (p) { f.appendChild(p); });

    f.addEventListener("submit", function (e) {
      e.preventDefault();
      submit.disabled = true;
      msg.className = "form-msg muted";
      msg.textContent = t("pub.working");
      var req = {
        name: name.value, zone_id: zone.value, tunnel_id: tunnel.value, target: target.value,
        access: login.checked, emails: mails.value.split(",").map(function (x) { return x.trim(); }).filter(Boolean),
        machine_id: m.id, service: s.name, app: s.app_name, app_id: s.app_id, login: s.login || "",
        accept_video_warning: video ? video.checked : false
      };
      send("POST", "api/v1/publications", req).then(function (r) {
        submit.disabled = false;
        if (!r.ok) {
          msg.className = "form-msg error-text";
          msg.textContent = errText(r.data);
          if (r.data.publication) f.appendChild(stepList(r.data.publication));
          return;
        }
        showDone(body, r.data);
        NW.reload();
      });
    });
    body.appendChild(f);
    name.focus();
  }

  function showDone(body, p) {
    body.replaceChildren();
    body.appendChild(el("h2", null, fill(t("pub.done"), { host: p.hostname })));
    var a = el("a", null, pubURL(p));
    a.href = pubURL(p); a.target = "_blank"; a.rel = "noopener noreferrer";
    if (p.provider === "tailscale") body.appendChild(el("p", "small", t("pub.ts_done_help")));
    body.appendChild(el("p")).appendChild(a);
    body.appendChild(el("h3", null, t("pub.steps")));
    body.appendChild(stepList(p));
    var out = el("p", "check-result");
    var row = el("div", "row");
    var close = el("button", "ghost", t("pub.cancel"));
    close.type = "button";
    close.addEventListener("click", function () { dialog.close(); });
    var check = checkButton(p, out);
    row.append(check, close);
    body.append(row, out);
    // DNS needs a moment; check once by itself after a short wait.
    setTimeout(function () { if (dialog.open) check.click(); }, 8000);
  }

  // ---------- Settings: alerts and outside watch ----------

  function closeButton() {
    var b = el("button", "ghost", t("pub.cancel"));
    b.type = "button";
    b.addEventListener("click", function () { dialog.close(); });
    return b;
  }

  // Saves the non-empty fields one by one; secrets left empty keep their value.
  function saveAll(pairs) {
    var chain = Promise.resolve({ ok: true });
    pairs.forEach(function (p) {
      chain = chain.then(function (r) {
        if (!r.ok || p[1] === "") return r;
        return send("POST", "api/v1/settings", { name: p[0], value: p[1] });
      });
    });
    return chain;
  }

  NW.openSettings = function (kind) {
    var body = dialog.querySelector(".dialog-body");
    body.replaceChildren(el("p", "muted", t("ui.loading")));
    dialog.showModal();
    NW.getJSON("api/v1/settings").then(function (list) {
      var cur = {};
      list.forEach(function (e) { cur[e.name] = e; });
      body.replaceChildren();
      var f = el("form", "form");
      var msg = msgLine();
      var row = el("div", "row");
      if (kind === "heartbeat_setup") {
        body.appendChild(el("h2", null, t("set.heartbeat_title")));
        body.appendChild(el("p", "small", t("set.heartbeat_intro")));
        var hb = input("url", "heartbeat", "", { placeholder: cur["heartbeat.url"].is_set ? "••••••" : "https://hc-ping.com/…" });
        row.append(el("button", null, t("pub.save")), closeButton());
        f.append(field(t("set.heartbeat_url"), hb, cur["heartbeat.url"].is_set ? t("set.keep_secret") : ""), row, msg);
        f.addEventListener("submit", function (e) {
          e.preventDefault();
          saveAll([["heartbeat.url", hb.value.trim()]]).then(function (r) {
            msg.className = "form-msg " + (r.ok ? "ok-text" : "error-text");
            msg.textContent = r.ok ? t("set.saved") : errText(r.data);
            if (r.ok) NW.reload();
          });
        });
      } else {
        body.appendChild(el("h2", null, t("set.alerts_title")));
        body.appendChild(el("p", "small", t("set.alerts_intro")));
        var url = input("url", "ntfy_url", cur["ntfy.url"].value, { required: "" });
        var topic = input("text", "ntfy_topic", cur["ntfy.topic"].value, { required: "", maxlength: "64", spellcheck: "false" });
        var tok = input("password", "ntfy_token", "", { autocomplete: "off", placeholder: cur["ntfy.token"].is_set ? "••••••" : "" });
        var lang = document.createElement("select");
        [["sv", "Svenska"], ["en", "English"]].forEach(function (o) { lang.appendChild(new Option(o[1], o[0])); });
        // Not chosen yet: use the language the page is shown in.
        lang.value = cur["alerts.lang"].is_set ? cur["alerts.lang"].value : state_lang();
        row.append(el("button", null, t("set.save_test")), closeButton());
        f.append(field(t("set.ntfy_url"), url), field(t("set.ntfy_topic"), topic),
          field(t("set.ntfy_token"), tok, cur["ntfy.token"].is_set ? t("set.keep_secret") : ""),
          field(t("set.lang"), lang), row, msg);
        f.addEventListener("submit", function (e) {
          e.preventDefault();
          saveAll([["ntfy.url", url.value.trim()], ["ntfy.topic", topic.value.trim()],
            ["ntfy.token", tok.value.trim()], ["alerts.lang", lang.value]]).then(function (r) {
            if (!r.ok) { msg.className = "form-msg error-text"; msg.textContent = errText(r.data); return; }
            return send("POST", "api/v1/alerts/test").then(function (r2) {
              msg.className = "form-msg " + (r2.ok ? "ok-text" : "error-text");
              msg.textContent = r2.ok ? t("ui.alerts_test_sent") : fill(t("ui.alerts_test_failed"), { error: r2.data.error || r2.status });
              NW.reload();
            });
          });
        });
      }
      body.appendChild(f);
    }).catch(function () {
      body.replaceChildren(el("p", "error-text", t("ui.error")));
    });
  };

  function state_lang() { return document.documentElement.lang === "sv" ? "sv" : "en"; }

  document.getElementById("alerts-setup").addEventListener("click", function () { NW.openSettings("alerts_setup"); });
  document.getElementById("heartbeat-setup").addEventListener("click", function () { NW.openSettings("heartbeat_setup"); });

  // ---------- Machines (agents) ----------

  function copyBox(text) {
    var wrap = el("div", "copy");
    var pre = el("pre", "cmd", text);
    var b = el("button", "ghost small", t("ag.copy"));
    b.type = "button";
    b.addEventListener("click", function () {
      var done = function () { b.textContent = t("ag.copied"); setTimeout(function () { b.textContent = t("ag.copy"); }, 2000); };
      if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done);
      else {
        var r = document.createRange(); r.selectNodeContents(pre);
        var sel = getSelection(); sel.removeAllRanges(); sel.addRange(r);
        try { document.execCommand("copy"); done(); } catch (e) { /* the text stays selected */ }
      }
    });
    wrap.append(pre, b);
    return wrap;
  }

  document.getElementById("add-machine").addEventListener("click", function () {
    var body = dialog.querySelector(".dialog-body");
    body.replaceChildren(el("p", "muted", t("ui.loading")));
    dialog.showModal();
    send("POST", "api/v1/agents/code").then(function (r) {
      body.replaceChildren();
      if (!r.ok) { body.appendChild(el("p", "error-text", errText(r.data))); body.appendChild(closeButton()); return; }
      var d = r.data;
      body.appendChild(el("h2", null, t("ag.add_title")));
      body.appendChild(el("p", "small", t("ag.add_intro")));
      body.appendChild(el("h3", null, t("ag.docker")));
      body.appendChild(copyBox(d.docker));
      body.appendChild(el("h3", null, t("ag.binary")));
      body.appendChild(copyBox(d.command));
      body.appendChild(el("p", "small muted", fill(t("ag.expires"), { time: new Date(d.expires).toLocaleTimeString(document.documentElement.lang, { hour: "2-digit", minute: "2-digit" }) })));
      body.appendChild(el("p", "small muted", fill(t("ag.fingerprint"), { fp: d.fingerprint.slice(0, 23) + "…" })));
      body.appendChild(closeButton());
    });
  });

  NW.renderAgents = function (list) {
    var panel = document.getElementById("agents");
    if (!list) { panel.hidden = true; return; }
    panel.hidden = false;
    var ul = panel.querySelector(".agent-list");
    ul.replaceChildren();
    list.forEach(function (a) {
      var li = el("li", "agent");
      li.appendChild(el("span", null, a.hostname || a.machine_id));
      li.appendChild(el("span", "muted small", fill(t("ag.added"), { date: new Date(a.created).toLocaleDateString(document.documentElement.lang) })));
      var rm = el("button", "ghost small danger", t("ag.remove"));
      rm.type = "button";
      rm.addEventListener("click", function () {
        if (!confirm(fill(t("ag.remove_confirm"), { name: a.hostname || a.machine_id }))) return;
        rm.disabled = true;
        send("DELETE", "api/v1/agents/" + encodeURIComponent(a.machine_id)).then(function (r) {
          if (r.ok) NW.reload(); else { rm.disabled = false; alert(errText(r.data)); }
        });
      });
      li.appendChild(rm);
      ul.appendChild(li);
    });
  };

  // ---------- Published list ----------

  // Tailscale keys can be changed later (both optional to refill).
  NW.openTailscaleSettings = function () {
    var body = dialog.querySelector(".dialog-body");
    body.replaceChildren();
    dialog.showModal();
    body.appendChild(el("h2", null, t("pub.ts_setup_title")));
    body.appendChild(el("p", "small", t("pub.ts_setup_intro")));
    var f = el("form", "form");
    var key = input("password", "tskey", "", { autocomplete: "off", placeholder: "••••••" });
    var api = input("password", "tsapi", "", { autocomplete: "off", placeholder: "tskey-api-…" });
    var msg = msgLine();
    var row = el("div", "row");
    row.append(el("button", null, t("pub.save")), closeButton());
    f.append(field(t("pub.ts_key"), key, t("set.keep_secret")), field(t("pub.ts_api"), api, t("pub.ts_api_help")), row, msg);
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      saveAll([["tailscale.authkey", key.value.trim()], ["tailscale.apikey", api.value.trim()]]).then(function (r) {
        msg.className = "form-msg " + (r.ok ? "ok-text" : "error-text");
        msg.textContent = r.ok ? t("set.saved") : errText(r.data);
      });
    });
    body.appendChild(f);
  };

  NW.renderPublications = function (list) {
    var panel = document.getElementById("publications");
    if (!list) { panel.hidden = true; return; }
    panel.hidden = false;
    var box = panel.querySelector(".pub-list");
    box.replaceChildren();
    if (list.some(function (p) { return p.provider === "tailscale"; })) {
      var tsb = el("button", "ghost small", t("pub.ts_settings"));
      tsb.type = "button";
      tsb.addEventListener("click", NW.openTailscaleSettings);
      box.appendChild(tsb);
    }
    var active = list.filter(function (p) { return p.status === "active"; });
    var old = list.filter(function (p) { return p.status !== "active"; }).slice(0, 8);
    if (!active.length) box.appendChild(el("p", "muted", t("pub.none")));
    var ul = el("ul", "pubs");
    active.forEach(function (p) {
      var li = el("li", "pub");
      var head = el("div", "pub-head");
      var a = el("a", "pub-host", p.hostname);
      a.href = pubURL(p); a.target = "_blank"; a.rel = "noopener noreferrer";
      head.appendChild(a);
      var ts = p.provider === "tailscale";
      var badge = ts ? "pub.only_devices" : (p.access ? "pub.with_login" : "pub.without_login");
      head.appendChild(el("span", "badge " + (ts || p.access ? "b-lock" : "b-own"), t(badge)));
      li.appendChild(head);
      li.appendChild(el("div", "muted small", (p.app ? p.app + " · " : "") + fill(t("pub.target_short"), { target: p.target }) +
        " · " + (ts ? "Tailscale" : p.tunnel_name)));
      var out = el("p", "check-result small", p.last_check ? checkText(p.last_check) : "");
      var row = el("div", "row");
      var undo = el("button", "ghost small danger", t("pub.undo"));
      undo.type = "button";
      undo.addEventListener("click", function () {
        if (!confirm(fill(t("pub.undo_confirm"), { host: p.hostname }))) return;
        undo.disabled = true;
        send("DELETE", "api/v1/publications/" + p.id).then(function (r) {
          if (!r.ok) { out.textContent = errText(r.data); undo.disabled = false; return; }
          var steps = (r.data && r.data.steps) || [];
          var last = steps.length ? steps[steps.length - 1].action : "";
          if (last === "tailscale_node_logged_out") {
            // Without an API key Tailscale keeps the device listed; say so plainly.
            alert(fill(t("pub.ts_undone"), { name: p.tunnel_name }));
          }
          NW.reload();
        });
      });
      row.append(checkButton(p, out), undo);
      li.append(row, out);
      ul.appendChild(li);
    });
    box.appendChild(ul);
    if (old.length) {
      var det = el("details");
      det.appendChild(el("summary", null, t("pub.history")));
      var ol = el("ul", "pubs old");
      old.forEach(function (p) {
        var li = el("li", "muted small", p.status === "failed" ? p.hostname + " – " + t("pub.failed") :
          fill(t("pub.undone"), { host: p.hostname }));
        ol.appendChild(li);
      });
      det.appendChild(ol);
      box.appendChild(det);
    }
  };
})();
