// Linq test console. A client of /v1 and nothing else: every action is a /v1
// call with the key the person typed. Values from the API are only ever set
// as text (textContent), never parsed as HTML.
"use strict";

(function () {
  const KEY_STORE = "linq.console.key";
  const RECENT_STORE = "linq.console.recent";
  const FINAL = new Set(["settled_in_treasury", "refunded", "expired", "failed", "cancelled"]);
  const EXPLORER = "https://stellar.expert/explorer/testnet/tx/";

  const $ = (id) => document.getElementById(id);
  let apiKey = "";
  let format = "json";
  let pendingKey = null; // the idempotency key the next create will use
  let current = null; // the order being followed
  let pollTimer = null;

  // ---- storage (a tab-only convenience; the page works without it) ----
  function load(name) {
    try { return sessionStorage.getItem(name); } catch (e) { return null; }
  }
  function save(name, value) {
    try {
      if (value === null) sessionStorage.removeItem(name);
      else sessionStorage.setItem(name, value);
    } catch (e) { /* private mode: keep going without it */ }
  }

  // ---- API ----
  async function api(method, path, body, key) {
    const headers = { "X-API-Key": key || apiKey };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: "no-store",
      credentials: "omit",
    });
    let data = null;
    try { data = await res.json(); } catch (e) { data = null; }
    return { status: res.status, ok: res.ok, data, headers: res.headers };
  }

  function messageOf(res) {
    if (res.status === 429) {
      const wait = res.headers.get("Retry-After");
      return "Too many requests for this key. Try again in " + (wait || "a few") + " seconds.";
    }
    if (res.data && typeof res.data.message === "string") return res.data.message;
    return "The request failed (HTTP " + res.status + ").";
  }

  // ---- small UI helpers ----
  function show(el, on) { el.hidden = !on; }
  function setError(id, text) {
    const el = $(id);
    el.textContent = text || "";
    show(el, !!text);
  }
  let toastTimer = null;
  function toast(text) {
    const el = $("toast");
    el.textContent = text;
    show(el, true);
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => show(el, false), 2200);
  }
  function el(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
  }
  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
  // lq_test_<8-character prefix>_<secret>: show the public prefix only.
  function maskKey(key) { return key.length > 16 ? key.slice(0, 16) + "…" : key; }
  function words(status) { return String(status || "").replace(/_/g, " "); }
  function naira(n) {
    const v = Number(n);
    return Number.isFinite(v) ? "₦" + v.toLocaleString("en-NG", { maximumFractionDigits: 2 }) : "";
  }
  function time(iso) {
    const d = new Date(iso);
    return isNaN(d) ? "" : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }
  function newIdempotencyKey() {
    const bytes = new Uint8Array(6);
    crypto.getRandomValues(bytes);
    return "console-" + Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  }

  // ---- sign in ----
  async function signIn(key) {
    // A key is checked by asking for an order that doesn't exist: a good key
    // gets 404, a refused one 401. Nothing is created.
    const res = await api("GET", "/v1/orders/console-key-check", undefined, key);
    if (res.status === 401 || res.status === 403) return "This key was refused. Check it and try again.";
    if (res.status === 429) return messageOf(res);
    if (res.status !== 404 && !res.ok) return messageOf(res);
    apiKey = key;
    save(KEY_STORE, key);
    $("key-chip").textContent = maskKey(key);
    $("key-prefix").textContent = maskKey(key);
    show($("signin"), false);
    show($("app"), true);
    renderRecent();
    renderRequest();
    return "";
  }

  function signOut(reason) {
    apiKey = "";
    save(KEY_STORE, null);
    save(RECENT_STORE, null);
    stopPolling();
    current = null;
    show($("order-card"), false);
    show($("rotated"), false);
    show($("webhook-saved"), false);
    $("key-input").value = "";
    show($("app"), false);
    show($("signin"), true);
    setError("signin-error", reason || "");
    $("key-input").focus();
  }

  // A key refused mid-session (revoked, or rotated out) ends the session.
  // Only 401 means the key: a 403 is a refusal of what was asked (a payout
  // failure this account may not request), shown where it happened.
  function handleAuthFailure(res) {
    if (res.status === 401) {
      signOut("This key is no longer accepted. Sign in with your current key.");
      return true;
    }
    return false;
  }

  // ---- the request preview ----
  function orderBody() {
    const body = {
      idempotencyKey: pendingKey || (pendingKey = newIdempotencyKey()),
      amountNgn: Number($("amount").value),
      bankCode: $("bank-code").value.trim(),
      bankAccount: $("bank-account").value.trim(),
      accountName: $("account-name").value.trim(),
    };
    const outcome = $("outcome").value;
    if (outcome) body.testFailure = outcome;
    return body;
  }

  function renderRequest() {
    const body = orderBody();
    const json = JSON.stringify(body, null, 2);
    const text = format === "json"
      ? json
      : "curl -X POST " + location.origin + "/v1/orders \\\n" +
        "  -H \"X-API-Key: $LINQ_KEY\" \\\n" +
        "  -H \"Content-Type: application/json\" \\\n" +
        "  -d '" + JSON.stringify(body) + "'";
    $("request-preview").textContent = text;
  }

  // ---- create and follow an order ----
  async function createOrder(event) {
    event.preventDefault();
    setError("order-error", "");
    const amount = Number($("amount").value);
    if (!Number.isFinite(amount) || amount <= 0) {
      setError("order-error", "Enter an amount in naira.");
      return;
    }
    const payload = orderBody();
    const button = $("create-button");
    button.disabled = true;
    button.textContent = "Creating…";
    try {
      const res = await api("POST", "/v1/orders", payload);
      if (handleAuthFailure(res)) return;
      if (!res.ok) {
        setError("order-error", messageOf(res));
        return;
      }
      current = { order: res.data, body: payload };
      remember(res.data);
      renderOrder(res.data);
      show($("resend"), true);
      pendingKey = null; // the next order gets a fresh idempotency key
      renderRequest();
      startPolling();
      $("order-card").scrollIntoView({ behavior: "smooth", block: "start" });
    } catch (e) {
      setError("order-error", "Couldn't reach the server. Check your connection and try again.");
    } finally {
      button.disabled = false;
      button.textContent = "Create test payment";
    }
  }

  async function resend() {
    if (!current) return;
    const out = $("resend-result");
    out.textContent = "Sending…";
    const res = await api("POST", "/v1/orders", current.body);
    if (handleAuthFailure(res)) return;
    if (!res.ok) {
      out.textContent = messageOf(res);
      return;
    }
    out.textContent = res.data.id === current.order.id
      ? "Same order returned: " + res.data.id + ". The idempotency key stopped a duplicate."
      : "A different order came back: " + res.data.id + ".";
  }

  function renderOrder(o) {
    show($("order-card"), true);
    $("order-id").textContent = o.id;
    const status = $("order-status");
    status.textContent = words(o.status);
    status.dataset.state = FINAL.has(o.status)
      ? (o.status === "settled_in_treasury" ? "good" : (o.status === "refunded" ? "warn" : "bad"))
      : "live";

    const waiting = o.status === "awaiting_deposit" || o.status === "pending" || o.status === "created";
    show($("pay-box"), waiting || !o.depositTxHash);
    $("pay-amount").textContent = o.quotedUsdc != null ? String(o.quotedUsdc) : "";
    $("pay-ngn").textContent = naira(o.quotedNgn != null ? o.quotedNgn : o.amountNgn);
    $("pay-address").textContent = o.depositAddress || "";
    $("pay-deadline").textContent = o.depositDeadline ? new Date(o.depositDeadline).toLocaleString() : "the deadline";
    const uri = $("pay-uri");
    if (typeof o.paymentUri === "string" && o.paymentUri.startsWith("web+stellar:")) {
      uri.href = o.paymentUri;
      show(uri, true);
    } else {
      show(uri, false);
    }

    // Status timeline
    const tl = $("timeline");
    clear(tl);
    const history = Array.isArray(o.statusHistory) ? o.statusHistory : [];
    if (history.length === 0) {
      const li = el("li", "now");
      li.appendChild(el("span", "dot"));
      li.appendChild(el("strong", "", words(o.status)));
      tl.appendChild(li);
    }
    history.forEach((ev, i) => {
      const li = el("li", i === history.length - 1 ? "now" : "");
      li.appendChild(el("span", "dot"));
      li.appendChild(el("strong", "", words(ev.to)));
      if (ev.at) li.appendChild(el("span", "muted", " " + time(ev.at)));
      if (ev.reason) li.appendChild(el("p", "reason", ev.reason));
      tl.appendChild(li);
    });

    // Payout attempts
    const attempts = Array.isArray(o.payoutAttempts) ? o.payoutAttempts : [];
    show($("attempts"), attempts.length > 0);
    show($("attempts-empty"), attempts.length === 0);
    const tbody = $("attempts").querySelector("tbody");
    clear(tbody);
    attempts.forEach((a) => {
      const tr = el("tr");
      tr.appendChild(el("td", "", String(a.number)));
      tr.appendChild(el("td", "mono", a.reference));
      const outcome = el("td", "");
      outcome.appendChild(el("span", "tag tag-" + (a.outcome === "paid" ? "good" : a.outcome === "failed" ? "bad" : "live"), words(a.outcome)));
      if (a.induced) outcome.appendChild(el("span", "tag tag-note", "induced on purpose"));
      tr.appendChild(outcome);
      tbody.appendChild(tr);
    });

    // Transactions on Stellar
    const links = $("tx-links");
    clear(links);
    const txs = [
      ["Your payment", o.depositTxHash],
      ["Sweep to Linq's treasury", o.sweepTxHash],
      ["Refund to " + (o.refundDestination ? shortAddr(o.refundDestination) : "the payer"), o.refundTxHash],
    ].filter((t) => typeof t[1] === "string" && /^[0-9a-f]{64}$/i.test(t[1]));
    if (txs.length === 0) links.appendChild(el("li", "muted", "Nothing on-chain yet."));
    txs.forEach(([label, hash]) => {
      const li = el("li");
      li.appendChild(el("span", "", label + ": "));
      const a = el("a", "mono", hash.slice(0, 10) + "…" + hash.slice(-6));
      a.href = EXPLORER + hash;
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      li.appendChild(a);
      links.appendChild(li);
    });

    $("order-json").textContent = JSON.stringify(o, null, 2);
    $("resend-result").textContent = "";
  }

  function shortAddr(a) { return String(a).slice(0, 6) + "…" + String(a).slice(-4); }

  function stopPolling() {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
  function startPolling() {
    stopPolling();
    const tick = async () => {
      if (!current || !apiKey) return;
      const res = await api("GET", "/v1/orders/" + encodeURIComponent(current.order.id));
      if (handleAuthFailure(res)) return;
      if (res.ok && res.data) {
        current.order = res.data;
        renderOrder(res.data);
        remember(res.data);
        if (FINAL.has(res.data.status)) return;
      }
      pollTimer = setTimeout(tick, document.hidden ? 10000 : 3000);
    };
    pollTimer = setTimeout(tick, 1500);
  }

  // ---- this session's orders ----
  function recent() {
    try { return JSON.parse(load(RECENT_STORE) || "[]"); } catch (e) { return []; }
  }
  function remember(o) {
    const list = recent().filter((r) => r.id !== o.id);
    list.unshift({ id: o.id, status: o.status, ngn: o.quotedNgn != null ? o.quotedNgn : o.amountNgn, at: o.createdAt });
    save(RECENT_STORE, JSON.stringify(list.slice(0, 10)));
    renderRecent();
  }
  function renderRecent() {
    const list = recent();
    show($("recent-card"), list.length > 0);
    const ul = $("recent");
    clear(ul);
    list.forEach((r) => {
      const li = el("li");
      const b = el("button", "linkish");
      b.type = "button";
      b.appendChild(el("span", "mono", r.id));
      b.appendChild(el("span", "muted", " " + naira(r.ngn) + " · " + words(r.status)));
      b.addEventListener("click", () => openOrder(r.id));
      li.appendChild(b);
      ul.appendChild(li);
    });
  }
  async function openOrder(id) {
    const res = await api("GET", "/v1/orders/" + encodeURIComponent(id));
    if (handleAuthFailure(res)) return;
    if (!res.ok) { toast(messageOf(res)); return; }
    current = { order: res.data, body: null };
    renderOrder(res.data);
    show($("resend"), false);
    if (!FINAL.has(res.data.status)) startPolling();
    $("order-card").scrollIntoView({ behavior: "smooth", block: "start" });
  }

  // ---- developers: rotate, webhook, rate limit ----
  async function rotate() {
    setError("rotate-error", "");
    if (!confirm("Rotate this key? You'll get a new key, shown once. The old one stops working in 24 hours.")) return;
    const button = $("rotate");
    button.disabled = true;
    try {
      const res = await api("POST", "/v1/keys/rotate", {});
      if (handleAuthFailure(res)) return;
      if (!res.ok || !res.data || typeof res.data.key !== "string") {
        setError("rotate-error", messageOf(res));
        return;
      }
      apiKey = res.data.key;
      save(KEY_STORE, apiKey);
      $("new-key").textContent = res.data.key;
      $("key-chip").textContent = maskKey(apiKey);
      $("key-prefix").textContent = maskKey(apiKey);
      const expires = res.data.previousKey && res.data.previousKey.expiresAt;
      $("old-expires").textContent = expires ? new Date(expires).toLocaleString() : "24 hours from now";
      show($("rotated"), true);
      toast("Key rotated");
    } finally {
      button.disabled = false;
    }
  }

  async function saveWebhook(event) {
    event.preventDefault();
    setError("webhook-error", "");
    const url = $("webhook-url").value.trim();
    if (!/^https:\/\//i.test(url)) {
      setError("webhook-error", "Enter an https:// address.");
      return;
    }
    const res = await api("PUT", "/v1/webhook", { url });
    if (handleAuthFailure(res)) return;
    if (!res.ok) {
      setError("webhook-error", messageOf(res));
      return;
    }
    const secret = res.data && res.data.webhookSecret;
    $("webhook-secret").textContent = typeof secret === "string" ? secret : "(no new secret was issued)";
    show($("webhook-saved"), true);
    toast("Webhook URL saved");
  }

  async function testRateLimit() {
    const button = $("ratelimit");
    button.disabled = true;
    button.textContent = "Sending…";
    let allowed = 0, refused = 0, limit = null, retryAfter = null;
    try {
      // Waves of ten at once, until the first refusal or a safety cap.
      for (let wave = 0; wave < 40 && refused === 0; wave++) {
        const replies = await Promise.all(Array.from({ length: 10 }, () =>
          api("GET", "/v1/orders/console-rate-limit-check")));
        for (const res of replies) {
          if (handleAuthFailure(res)) return;
          if (limit === null && res.headers.get("X-RateLimit-Limit")) limit = res.headers.get("X-RateLimit-Limit");
          if (res.status === 429) {
            refused++;
            retryAfter = retryAfter || res.headers.get("Retry-After");
          } else {
            allowed++;
          }
        }
      }
      $("rl-limit").textContent = limit || "?";
      $("rl-allowed").textContent = String(allowed);
      $("rl-refused").textContent = String(refused);
      $("rl-detail").textContent = refused > 0
        ? "Every request past the limit got 429 with Retry-After: " + (retryAfter || "?") + " seconds. The key works again when the minute is up."
        : "No request was refused. This key's limit may be higher than the test sends.";
      show($("ratelimit-result"), true);
    } finally {
      button.disabled = false;
      button.textContent = "Test the rate limit";
    }
  }

  // ---- copying ----
  async function copyFrom(id) {
    const text = $(id).textContent;
    try {
      await navigator.clipboard.writeText(text);
      toast("Copied");
    } catch (e) {
      toast("Couldn't copy. Select the text and copy it instead.");
    }
  }

  // ---- tabs ----
  function selectTab(name) {
    ["payments", "developers"].forEach((t) => {
      const on = t === name;
      $("tab-" + t).setAttribute("aria-selected", String(on));
      show($("panel-" + t), on);
    });
  }

  // ---- wire up ----
  document.addEventListener("DOMContentLoaded", () => {
    $("signin-form").addEventListener("submit", async (e) => {
      e.preventDefault();
      const key = $("key-input").value.trim();
      setError("signin-error", "");
      if (!key) { setError("signin-error", "Paste your test API key."); return; }
      if (!key.startsWith("lq_test_")) { setError("signin-error", "Test keys start with lq_test_. A live key can't be used here."); return; }
      const button = $("signin-button");
      button.disabled = true;
      button.textContent = "Checking…";
      try {
        setError("signin-error", await signIn(key));
      } catch (err) {
        setError("signin-error", "Couldn't reach the server. Check your connection and try again.");
      } finally {
        button.disabled = false;
        button.textContent = "Sign in";
      }
    });
    $("signout").addEventListener("click", () => signOut(""));
    $("tab-payments").addEventListener("click", () => selectTab("payments"));
    $("tab-developers").addEventListener("click", () => selectTab("developers"));
    $("order-form").addEventListener("submit", createOrder);
    ["amount", "bank-code", "bank-account", "account-name", "outcome"].forEach((id) =>
      $(id).addEventListener("input", renderRequest));
    document.querySelectorAll(".seg-btn").forEach((b) => b.addEventListener("click", () => {
      format = b.dataset.format;
      document.querySelectorAll(".seg-btn").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
      renderRequest();
    }));
    $("resend").addEventListener("click", resend);
    $("rotate").addEventListener("click", rotate);
    $("webhook-form").addEventListener("submit", saveWebhook);
    $("ratelimit").addEventListener("click", testRateLimit);
    document.querySelectorAll("[data-copy-from]").forEach((b) =>
      b.addEventListener("click", () => copyFrom(b.dataset.copyFrom)));

    const saved = load(KEY_STORE);
    if (saved) {
      signIn(saved).then((err) => { if (err) signOut(err); }).catch(() => signOut(""));
    } else {
      $("key-input").focus();
    }
  });
})();
