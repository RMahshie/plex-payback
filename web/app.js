"use strict";

// What everything watched on Plex would have cost on streaming services. The server
// prices every play (30-day subscriptions for shows, rentals for movies) and keeps a
// ledger; this page draws it and edits costs and prices.

const $ = (id) => document.getElementById(id);

// el builds DOM nodes. Text always goes in via text nodes (never innerHTML).
function el(tag, attrs = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    n.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return n;
}

function svg(tag, attrs = {}, text) {
  const n = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  if (text !== undefined) n.textContent = text;
  return n;
}

async function api(method, path, body) {
  const opts = { method, headers: { "X-Requested-With": "payback" } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || "HTTP " + r.status);
  return data;
}

let toastTimer;
function toast(msg, bad = false) {
  const t = $("toast");
  t.textContent = msg;
  t.className = "toast" + (bad ? " bad" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 4000);
}

const st = { r: null, tab: "Recent", buckets: [], editing: null };
const MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const money = (v, d = 0) => v.toLocaleString(undefined, { style: "currency", currency: st.r?.settings.currency || "USD", minimumFractionDigits: d, maximumFractionDigits: d });
const plain = (v) => v.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
const DEC = (1.5).toLocaleString().charAt(1); // "." or "," in this browser's locale
const parseAmount = (s) => parseFloat(s.replace(new RegExp(`[^0-9${DEC}]`, "g"), "").replace(DEC, "."));
const monthName = (m) => MON[+m.slice(5) - 1] + " " + m.slice(0, 4);
const monthOf = (t) => { const d = new Date(t); return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}`; };
const dayOf = (t) => { const d = new Date(t); return `${MON[d.getMonth()]} ${d.getDate()}`; };
const bucketOf = (c) => (c.type === "plan" ? c.via : c.type === "rent" ? "Rentals" : "Buys");

let pollTimer;
async function load() {
  clearTimeout(pollTimer);
  try { st.r = await api("GET", "/api/result"); } catch (e) { toast(e.message, true); return; }
  render();
  if (st.r.error === "Working it out") pollTimer = setTimeout(load, 3000);
}

async function save(patch) {
  try { st.r = await api("PUT", "/api/settings", patch); render(); } catch (err) { toast(err.message, true); }
}

function render() {
  const r = st.r;
  const empty = !r.months?.length;
  $("demo").classList.toggle("hidden", !r.demo);
  $("updated").textContent = r.updated && !r.updated.startsWith("0001") ? "Updated " + new Date(r.updated).toLocaleString(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" }) : "";
  const note = r.error || (r.unpriced?.length ? `${r.unpriced.length} service${r.unpriced.length > 1 ? "s" : ""} your titles are on ${r.unpriced.length > 1 ? "have" : "has"} no price yet, so those titles are priced as rentals or purchases. Add prices on the right.` : "");
  $("sv-err").textContent = note;
  $("sv-err").classList.toggle("hidden", !note);
  $("sv-total").textContent = empty ? "–" : money(r.total);
  $("sv-pct").replaceChildren(empty ? "" : `${Math.round(r.paidPct)}%`, empty ? "" : el("span", {}, "paid back"));
  $("sv-fill").style.width = (empty ? 0 : r.paidPct) + "%";
  $("sv-when").textContent = r.payback === "paid" ? "Paid off" : r.payback ? monthName(r.payback) : "–";
  renderPanel();
  if (empty) {
    for (const id of ["sv-svc", "sv-tabs", "sv-box", "sv-sum", "sv-svg"]) $(id).replaceChildren();
    return;
  }
  renderChart();
  const sums = {};
  for (const c of r.ledger) { const k = bucketOf(c); sums[k] ||= { c: 0, n: 0 }; sums[k].c += c.cost; sums[k].n++; }
  st.buckets = Object.entries(sums).map(([n, v]) => ({ n, c: v.c, mo: r.settings.plans[n] !== undefined ? v.n : 0 })).sort((a, b) => b.c - a.c);
  const most = st.buckets[0].c || 1;
  const open = (n) => () => { showTab(n); $("sv-tabs").scrollIntoView({ behavior: "smooth", block: "start" }); };
  $("sv-svc").replaceChildren(...st.buckets.flatMap((b) => {
    const bar = el("i");
    bar.style.width = (b.c / most) * 100 + "%"; // CSSOM: the CSP blocks inline style attributes
    return [
      el("div", { class: "n", onclick: open(b.n) }, b.n),
      el("div", { class: "b", onclick: open(b.n) }, bar),
      el("div", { class: "c", onclick: open(b.n) }, money(b.c)),
      el("div", { class: "m", onclick: open(b.n) }, b.mo ? b.mo + " mo" : ""),
    ];
  }));
  showTab(sums[st.tab] ? st.tab : "Recent");
}

// One column per month, peak labelled; hover shows the month's subscriptions, click opens it below.
function renderChart() {
  const months = st.r.months, box = $("sv-chart"), s = $("sv-svg"), tip = $("sv-tip");
  const W = box.clientWidth, H = 240, top = 24, bot = 40, left = 48;
  if (!W) return;
  const peak = months.reduce((a, b) => (b.cost > a.cost ? b : a));
  const step = peak.cost > 400 ? 100 : peak.cost > 200 ? 50 : 25;
  const max = Math.max(step, Math.ceil(peak.cost / step) * step), ph = H - top - bot;
  const slot = (W - left) / months.length, bw = Math.min(24, slot * 0.5), y = (v) => top + ph - (v / max) * ph;
  const grid = svg("g", { class: "grid" });
  for (let t = 0; t <= max; t += step) {
    grid.append(svg("line", { x1: left, x2: W, y1: y(t), y2: y(t) }), svg("text", { class: "tick", x: left - 8, y: y(t) + 4, "text-anchor": "end" }, money(t)));
  }
  const nodes = [grid];
  months.forEach((m, i) => {
    const last = i === months.length - 1;
    const cx = left + slot * i + slot / 2, h = Math.max(2, (m.cost / max) * ph), x = cx - bw / 2, yy = top + ph - h, r = Math.min(4, h / 2);
    const d = `M${x},${top + ph} V${yy + r} Q${x},${yy} ${x + r},${yy} H${x + bw - r} Q${x + bw},${yy} ${x + bw},${yy + r} V${top + ph} Z`;
    const col = svg("path", { class: "col" + (last ? " part" : ""), d });
    const g = svg("g");
    g.append(svg("rect", { class: "hit", x: cx - slot / 2, y: top, width: slot, height: ph, "aria-label": `${monthName(m.m)} ${money(m.cost, 2)}` }), col);
    g.addEventListener("mouseenter", () => {
      const extra = [m.rentals && `${m.rentals} rental${m.rentals > 1 ? "s" : ""}`, m.buys && `${m.buys} buy${m.buys > 1 ? "s" : ""}`].filter(Boolean);
      const what = [[...new Set(m.subs)].join(", "), ...extra].filter(Boolean).join(" + ") || "Nothing";
      tip.replaceChildren(el("b", {}, `${monthName(m.m)} | ${money(m.cost, 2)}`), what, ...(last ? [el("br"), "So far"] : []));
      const b = col.getBBox(), right = b.x + b.width + 12 + tip.offsetWidth < W;
      tip.style.left = (right ? b.x + b.width + 12 : b.x - 12 - tip.offsetWidth) + "px";
      tip.style.top = Math.max(b.y + 24, 40) + "px";
      tip.style.opacity = 1;
      g.classList.add("on");
    });
    g.addEventListener("mouseleave", () => { tip.style.opacity = 0; g.classList.remove("on"); });
    g.addEventListener("click", () => { showTab("Recent", m.m); $("sv-tabs").scrollIntoView({ behavior: "smooth", block: "start" }); });
    nodes.push(g);
    if (m === peak && m.cost > 0) nodes.push(svg("text", { class: "vl", x: cx, y: yy - 8, "text-anchor": "middle" }, money(m.cost)));
    nodes.push(svg("text", { class: "xl", x: cx, y: top + ph + 20, "text-anchor": "middle" }, MON[+m.m.slice(5) - 1]));
    if (i === 0 || m.m.endsWith("-01")) nodes.push(svg("text", { class: "yr", x: cx, y: top + ph + 36, "text-anchor": "middle" }, m.m.slice(0, 4)));
  });
  s.setAttribute("viewBox", `0 0 ${W} ${H}`);
  s.setAttribute("height", H);
  s.replaceChildren(...nodes);
}

// The ledger: every charge once, newest first. Subscriptions list what was watched on
// them; a month's rentals sit under one Rentals entry.
function showTab(name, month) {
  st.tab = name;
  const r = st.r;
  const tabs = ["Recent", ...st.buckets.map((b) => b.n)];
  $("sv-tabs").replaceChildren(...tabs.map((t) => el("button", { class: t === name ? "on" : "", onclick: () => showTab(t) }, t)));
  const list = name === "Recent" ? r.ledger : r.ledger.filter((c) => bucketOf(c) === name);
  const isPlan = r.settings.plans[name] !== undefined; // a service's periods carry their own dates
  const rows = [];
  let cur = "", head = null, headSum = 0;
  const closeHead = () => head && head.append(el("b", {}, money(headSum, 2)));
  const rentals = {}; // month -> its "Rentals" entry
  for (const c of list) {
    const m = monthOf(c.start);
    if (!isPlan && m !== cur) {
      closeHead();
      cur = m;
      headSum = 0;
      head = el("div", { class: "sv-mhead", "data-m": m }, el("span", {}, monthName(m)));
      rows.push(head);
    }
    headSum += c.cost;
    const when = c.manual ? monthName(m) : c.type === "plan" ? `${dayOf(c.start)} – ${dayOf(c.end)}` : dayOf(c.start);
    const top = (what) => el("div", { class: "top" }, what, el("span", { class: "when" }, when), el("span", { class: "cost" }, money(c.cost, 2)));
    if (c.type === "rent") {
      let g = rentals[m];
      if (!g) {
        g = rentals[m] = { n: 0, sum: 0, when: el("span", { class: "when" }), cost: el("span", { class: "cost" }), ul: el("ul", { class: "rentals" }) };
        rows.push(el("div", { class: "sv-entry" }, el("div", { class: "top" }, el("span", { class: "what" }, "Rentals"), g.when, g.cost), g.ul));
      }
      g.n++;
      g.sum += c.cost;
      g.ul.append(el("li", {}, el("span", {}, c.titles[0], el("span", { class: "kind" }, c.via)), el("span", { class: "when" }, when), el("span", { class: "cost" }, money(c.cost, 2))));
      continue;
    }
    rows.push(c.type === "plan"
      ? el("div", { class: "sv-entry" }, top(el("span", { class: "what" }, c.via)), el("ul", {}, c.titles.map((t) => el("li", {}, t))))
      : el("div", { class: "sv-entry rent" }, top(el("span", { class: "what" }, c.titles[0], el("span", { class: "kind" }, c.via)))));
  }
  closeHead();
  for (const g of Object.values(rentals)) {
    g.when.textContent = `${g.n} movie${g.n > 1 ? "s" : ""}`;
    g.cost.textContent = money(g.sum, 2);
  }
  const box = $("sv-box");
  box.replaceChildren(...rows);
  const target = month && box.querySelector(`[data-m="${month}"]`);
  box.scrollTop = target ? target.offsetTop - box.offsetTop : 0;
  const what = name === "Recent" ? "charges" : name === "Rentals" ? "rentals" : name === "Buys" ? "buys" : "months";
  $("sv-sum").replaceChildren(`${list.length} ${what} `, el("span", { class: "sep" }, "|"), " ", el("b", {}, money(list.reduce((a, c) => a + c.cost, 0), 2)));
}

// ---------- costs and prices: every change is saved and everything repriced ----------

function amountInput(value, onSet, placeholder) {
  return el("input", { value: value === undefined ? "" : plain(value), placeholder, inputmode: "decimal", "aria-label": "amount", onchange: (e) => {
    const v = parseAmount(e.target.value);
    if (!(v >= 0)) { e.target.value = value === undefined ? "" : plain(value); return; }
    onSet(v);
  } });
}

const EVERY = { once: "once", month: "per month", year: "per year" };
const span = (c) => {
  const a = c.from && monthName(c.from), b = c.to && monthName(c.to);
  return a && b ? `${a}–${b}` : a ? `since ${a}` : b ? `until ${b}` : "";
};

function renderPanel() {
  if (document.activeElement?.closest(".sv-kv")) return; // don't redraw under the cursor
  const set = st.r.settings;
  const row = (label, note, input, cls) => [el("div", { class: cls }, label, note ? el("small", {}, note) : ""), el("div", { class: "p" }, input)];
  $("sv-spent").textContent = st.r.months?.length ? `${money(st.r.costs)} so far` : "";

  const costs = set.costs || [];
  const put = (list) => { st.editing = null; save({ costs: list }); };
  const nodes = [];
  costs.forEach((c, i) => {
    const note = [c.every === "once" ? "" : EVERY[c.every], span(c)].filter(Boolean).join(", ");
    nodes.push(el("div", { class: "cname", title: "Edit", onclick: () => { st.editing = st.editing === i ? null : i; renderPanel(); } }, c.name, note ? el("small", {}, note) : ""),
      el("div", { class: "p" }, amountInput(c.amount, (v) => put(costs.map((x, k) => (k === i ? { ...x, amount: v } : x))))));
    if (st.editing === i) nodes.push(costEditor(c, (n) => put(costs.map((x, k) => (k === i ? n : x))), () => put(costs.filter((_, k) => k !== i))));
  });
  if (st.editing === "new") nodes.push(costEditor({ name: "", amount: 0, every: "month" }, (n) => put([...costs, n]), () => { st.editing = null; renderPanel(); }));
  $("sv-costs").replaceChildren(...nodes);

  $("sv-asof").textContent = set.pricesAsOf ? monthName(set.pricesAsOf) : st.r.region;
  const plan = (n) => (v) => save({ plans: { [n]: v } });
  $("sv-prices").replaceChildren(
    el("div", { class: "hd" }, "Per month, the plan you'd pick"), el("div", { class: "hd" }),
    ...Object.entries(set.plans).sort((a, b) => b[1] - a[1]).flatMap(([n, v]) => row(n, "", amountInput(v, plan(n)))),
    ...(st.r.unpriced?.length ? [el("div", { class: "hd" }, "Not priced yet"), el("div", { class: "hd" }),
      ...st.r.unpriced.flatMap((n) => row(n, "", amountInput(undefined, plan(n), "–"), "unpriced"))] : []),
    el("div", { class: "hd" }, "Rent or buy"), el("div", { class: "hd" }),
    ...row("Movie, older", "", amountInput(set.rentOld, (v) => save({ rentOld: v }))),
    ...row("Movie, last 2 years", "", amountInput(set.rentRecent, (v) => save({ rentRecent: v }))),
    ...row("Movie, new release", "", amountInput(set.rentNew, (v) => save({ rentNew: v }))),
    ...row("Episode", "", amountInput(set.episode, (v) => save({ episode: v }))),
    ...row("Season", "", amountInput(set.season, (v) => save({ season: v }))));
}

// costEditor edits a cost's name, how often it's paid and the months it ran.
function costEditor(c, onSave, onRemove) {
  const name = el("input", { value: c.name, placeholder: "e.g. Plex Pass" });
  const amount = el("input", { value: plain(c.amount || 0), inputmode: "decimal" });
  const every = el("select", {}, ...Object.entries(EVERY).map(([k, v]) => el("option", { value: k, selected: k === c.every ? "" : null }, v)));
  const from = el("input", { type: "month", value: c.from || "" });
  const to = el("input", { type: "month", value: c.to || "" });
  return el("div", { class: "editor" },
    el("label", { class: "wide" }, "Name", name),
    el("label", {}, "Amount", amount), el("label", {}, "Paid", every),
    el("label", {}, "From (optional)", from), el("label", {}, "Until (optional)", to),
    el("div", { class: "acts" },
      el("button", { onclick: onRemove }, c.name ? "Remove" : "Cancel"),
      el("button", { class: "save", onclick: () => {
        const n = { name: name.value.trim(), amount: parseAmount(amount.value) || 0, every: every.value };
        if (from.value) n.from = from.value;
        if (to.value) n.to = to.value;
        if (!n.name) { toast("Give the cost a name", true); return; }
        onSave(n);
      } }, "Save")));
}

// ---------- wiring ----------

$("cost-add").addEventListener("click", () => { st.editing = "new"; renderPanel(); });
$("sv-reset").addEventListener("click", async () => {
  try { st.r = await api("POST", "/api/prices/reset"); render(); toast("Prices reset"); } catch (err) { toast(err.message, true); }
});
$("refresh").addEventListener("click", async () => {
  const before = st.r?.updated, b = $("refresh");
  if (b.classList.contains("spin")) return;
  b.classList.add("spin");
  try { await api("POST", "/api/refresh"); } catch (err) { toast(err.message, true); b.classList.remove("spin"); return; }
  const wait = async (n) => {
    const r = await api("GET", "/api/result").catch(() => null);
    if (r && r.updated !== before) { st.r = r; render(); b.classList.remove("spin"); return; }
    if (n > 0) setTimeout(() => wait(n - 1), 3000); else b.classList.remove("spin");
  };
  wait(400);
});
addEventListener("resize", () => { if (st.r?.months?.length) renderChart(); });
load();
