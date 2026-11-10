// DotaStats page behaviour: local times, tabs, Chart.js charts, the match
// map and timeline filters. Chart and map data come from
// <script type="application/json"> blocks rendered by the server; canvases
// name their builder (data-chart) and data block (data-source). No build
// step. The interface is in Russian.
(() => {
  "use strict";

  const LOCALE = "ru-RU";
  const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

  // Dota 2's player colors, slots 0-4 then 128-132.
  const slotColors = ["#3375ff", "#66ffbf", "#bf00bf", "#f3f00b", "#ff6b00",
    "#fe86c2", "#a1b447", "#65d9f7", "#008321", "#a46900"];
  const slotColor = (slot) => slotColors[slot < 128 ? slot : 5 + slot - 128] || "#888";
  const teamOf = (slot) => (slot < 128 ? "radiant" : "dire");
  const opposite = (team) => (team === "radiant" ? "dire" : "radiant");
  const TEAM = { radiant: "Силы Света", dire: "Силы Тьмы" };
  const TEAM_GEN = { radiant: "Сил Света", dire: "Сил Тьмы" };
  const clock = (sec) => {
    const s = Math.abs(Math.round(sec));
    return (sec < 0 ? "-" : "") + Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0");
  };
  const num = (v) => Math.round(v).toLocaleString(LOCALE);
  const fmtK = (v) => (Math.abs(v) >= 1000 ? (v / 1000).toLocaleString(LOCALE, { maximumFractionDigits: 1 }) + " тыс." : num(v));

  // Dates in the current year leave the year out.
  function localTimes(root) {
    const thisYear = new Date().getFullYear();
    root.querySelectorAll("time[data-local]").forEach((el) => {
      const d = new Date(el.dateTime);
      if (isNaN(d)) return;
      const day = { day: "numeric", month: "short" };
      if (d.getFullYear() !== thisYear) day.year = "numeric";
      el.textContent = el.dataset.local === "date"
        ? d.toLocaleDateString(LOCALE, day)
        : d.toLocaleString(LOCALE, { ...day, hour: "2-digit", minute: "2-digit" });
      el.title = d.toLocaleString(LOCALE);
    });
  }

  const dataCache = new Map();
  function data(id) {
    if (!dataCache.has(id)) {
      const el = document.getElementById(id);
      dataCache.set(id, el ? JSON.parse(el.textContent) : null);
    }
    return dataCache.get(id);
  }

  function baseOptions() {
    const text = css("--muted"), grid = css("--border");
    return {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      locale: LOCALE,
      interaction: { mode: "index", intersect: false },
      plugins: { legend: { labels: { color: text, boxWidth: 12 } }, tooltip: { callbacks: {} } },
      scales: {
        x: { ticks: { color: text, maxTicksLimit: 12 }, grid: { color: grid } },
        y: { ticks: { color: text }, grid: { color: grid } },
      },
    };
  }

  // Tooltips list the series by their value at the hovered point, highest
  // first, so the order follows the curves.
  const byValue = (a, b) => (b.parsed.y ?? -Infinity) - (a.parsed.y ?? -Infinity);
  const minutes = (n) => Array.from({ length: n }, (_, i) => i + " мин");

  // markerLines draws labeled vertical lines at given minutes.
  const markerLines = (markers) => ({
    id: "markers",
    afterDatasetsDraw(chart) {
      const { ctx, chartArea, scales } = chart;
      ctx.save();
      ctx.strokeStyle = css("--warn");
      ctx.fillStyle = css("--warn");
      ctx.setLineDash([4, 4]);
      ctx.font = "11px system-ui, sans-serif";
      // Labels that would overlap the previous one move down a row.
      const rowEnds = [];
      [...(markers || [])].sort((a, b) => a.minute - b.minute).forEach((m) => {
        const x = scales.x.getPixelForValue(m.minute);
        if (x < chartArea.left || x > chartArea.right) return;
        ctx.beginPath();
        ctx.moveTo(x, chartArea.top);
        ctx.lineTo(x, chartArea.bottom);
        ctx.stroke();
        const text = `${m.label}, ${m.minute} мин`;
        const w = ctx.measureText(text).width;
        const left = x + w + 8 > chartArea.right ? x - w - 4 : x + 4;
        let row = rowEnds.findIndex((end) => left > end + 6);
        if (row < 0) row = rowEnds.length;
        rowEnds[row] = left + w;
        ctx.fillText(text, left, chartArea.top + 12 + row * 14);
      });
      ctx.restore();
    },
  });

  function hexAlpha(color, alpha) {
    const m = /^#([0-9a-f]{6})$/i.exec(color);
    if (!m) return color;
    const n = parseInt(m[1], 16);
    return `rgba(${n >> 16}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
  }

  const builders = {
    // Team gold and XP advantage per minute; positive means Radiant ahead.
    advantage(canvas, d) {
      const a = d.advantage;
      const opts = baseOptions();
      opts.scales.y.ticks.callback = fmtK;
      opts.plugins.tooltip.callbacks.label = (c) =>
        ` ${c.dataset.label}: ${c.parsed.y === 0 ? "поровну" : `${TEAM[c.parsed.y > 0 ? "radiant" : "dire"]} +${num(Math.abs(c.parsed.y))}`}`;
      return new Chart(canvas, {
        type: "line",
        data: {
          labels: minutes(Math.max(a.gold?.length || 0, a.xp?.length || 0)),
          datasets: [
            { label: "Золото", data: a.gold, borderColor: css("--warn"), pointRadius: 0, borderWidth: 2,
              fill: { target: "origin", above: hexAlpha(css("--radiant"), 0.18), below: hexAlpha(css("--dire"), 0.18) } },
            { label: "Опыт", data: a.xp, borderColor: css("--link"), pointRadius: 0, borderWidth: 2, borderDash: [5, 4] },
          ],
        },
        options: opts,
      });
    },

    // Gold or last hits per minute for all ten players. Controls switch the
    // metric and the team; tracked players are drawn thicker.
    players(canvas, d) {
      const series = d.players || [];
      const card = canvas.closest(".card");
      let metric = "gold", team = "all";
      const make = () => series
        .filter((p) => team === "all" || teamOf(p.slot) === team)
        .map((p) => ({
          label: p.name, data: p[metric], borderColor: slotColor(p.slot), backgroundColor: slotColor(p.slot),
          pointRadius: 0, pointHoverRadius: 4, borderWidth: p.tracked ? 3.5 : 1.6,
          borderDash: teamOf(p.slot) === "dire" ? [6, 3] : [],
        }));
      const opts = baseOptions();
      opts.plugins.legend.position = "bottom";
      opts.plugins.tooltip.itemSort = byValue;
      opts.plugins.tooltip.callbacks.label = (c) => ` ${c.dataset.label}: ${num(c.parsed.y)}`;
      opts.scales.y.ticks.callback = fmtK;
      const chart = new Chart(canvas, {
        type: "line",
        data: { labels: minutes(Math.max(0, ...series.map((p) => p.gold?.length || 0))), datasets: make() },
        options: opts,
      });
      const group = (sel, onPick) => {
        const buttons = card?.querySelectorAll(`${sel} button`) || [];
        buttons.forEach((btn) => btn.addEventListener("click", () => {
          buttons.forEach((b) => b.setAttribute("aria-pressed", String(b === btn)));
          onPick(btn);
          chart.data.datasets = make();
          chart.update();
        }));
      };
      group("[data-curve-metric]", (b) => { metric = b.dataset.metric; });
      group("[data-curve-team]", (b) => { team = b.dataset.team; });
      return chart;
    },

    // Approximate Radiant win chance per minute with key moments marked.
    winchance(canvas, d) {
      const p = (d.win_chance || []).map((v) => Math.round(v * 1000) / 10);
      const opts = baseOptions();
      opts.scales.y.min = 0;
      opts.scales.y.max = 100;
      opts.scales.y.ticks.callback = (v) => v + "%";
      opts.plugins.legend.display = false;
      opts.plugins.tooltip.callbacks.label = (c) =>
        ` Силы Света ${c.parsed.y.toLocaleString(LOCALE)}% · Силы Тьмы ${(Math.round((100 - c.parsed.y) * 10) / 10).toLocaleString(LOCALE)}%`;
      return new Chart(canvas, {
        type: "line",
        data: {
          labels: minutes(p.length),
          datasets: [{ label: "Шанс победы Сил Света", data: p, borderColor: css("--link"), pointRadius: 0, borderWidth: 2,
            fill: { target: { value: 50 }, above: hexAlpha(css("--radiant"), 0.2), below: hexAlpha(css("--dire"), 0.2) } }],
        },
        options: opts,
        plugins: [markerLines(d.markers)],
      });
    },

    // GPM per game with the win rate over the last 10 games.
    trend(canvas, d) {
      const games = d.trend || [];
      const wr = games.map((_, i) => {
        const window = games.slice(Math.max(0, i - 9), i + 1);
        return Math.round((100 * window.filter((g) => g.win).length) / window.length);
      });
      const opts = baseOptions();
      opts.scales.y.title = { display: true, text: "GPM", color: css("--muted") };
      opts.scales.y2 = { position: "right", min: 0, max: 100, ticks: { color: css("--muted"), callback: (v) => v + "%" }, grid: { drawOnChartArea: false } };
      opts.plugins.tooltip.callbacks.label = (c) => c.datasetIndex === 0
        ? ` GPM: ${c.parsed.y == null ? "—" : num(c.parsed.y)} (${games[c.dataIndex].win ? "победа" : "поражение"})`
        : ` Винрейт за 10 игр: ${c.parsed.y}%`;
      return new Chart(canvas, {
        type: "line",
        data: {
          labels: games.map((g) => new Date(g.time).toLocaleDateString(LOCALE)),
          datasets: [
            { label: "GPM", data: games.map((g) => g.gpm), borderColor: css("--warn"), showLine: false,
              pointBackgroundColor: games.map((g) => (g.win ? css("--win") : css("--loss"))), pointBorderWidth: 0, pointRadius: 3 },
            { label: "Винрейт за 10 игр", data: wr, yAxisID: "y2", borderColor: css("--link"), pointRadius: 0, borderWidth: 2, tension: 0.3 },
          ],
        },
        options: opts,
      });
    },

    // Benchmark percentiles per game.
    benchmarks(canvas, d) {
      const b = d.benchmarks;
      const names = { gold_per_min: "GPM", xp_per_min: "XPM", last_hits_per_min: "Добивания в минуту", hero_damage_per_min: "Урон по героям в минуту" };
      const colors = [css("--warn"), css("--link"), css("--win"), css("--accent")];
      const opts = baseOptions();
      opts.scales.y.min = 0;
      opts.scales.y.max = 100;
      opts.scales.y.ticks.callback = (v) => v + "%";
      opts.plugins.tooltip.itemSort = byValue;
      opts.plugins.tooltip.callbacks.label = (c) => ` ${c.dataset.label}: лучше, чем у ${c.parsed.y}%`;
      return new Chart(canvas, {
        type: "line",
        data: {
          labels: b.points.map((p) => new Date(p.time).toLocaleDateString(LOCALE)),
          datasets: b.metrics.map((m, i) => ({
            label: names[m] || m,
            data: b.points.map((p) => (p.pct[m] == null ? null : Math.round(p.pct[m] * 100))),
            borderColor: colors[i % colors.length], pointRadius: 2, borderWidth: 2, tension: 0.3, spanGaps: true,
          })),
        },
        options: opts,
      });
    },
  };

  // charts builds the charts that are visible; hidden tabs get theirs when
  // shown, so Chart.js measures a real size.
  function charts(root) {
    if (typeof Chart === "undefined") return;
    root.querySelectorAll("canvas[data-chart]").forEach((canvas) => {
      if (canvas.dataset.ready || canvas.offsetParent === null) return;
      const build = builders[canvas.dataset.chart];
      const d = data(canvas.dataset.source);
      if (!build || !d) return;
      canvas.dataset.ready = "1";
      build(canvas, d);
    });
  }

  // tabs shows one data-panel at a time and remembers it in the URL hash.
  // Without JavaScript every panel stays visible.
  let showTab = () => {};
  function tabs(root) {
    root.querySelectorAll("[data-tabs]").forEach((nav) => {
      const buttons = [...nav.querySelectorAll("button[data-tab]")];
      const panels = [...document.querySelectorAll("[data-panel]")];
      const show = (name) => {
        if (!buttons.some((b) => b.dataset.tab === name)) name = buttons[0].dataset.tab;
        panels.forEach((p) => { p.hidden = p.dataset.panel !== name; });
        buttons.forEach((b) => b.setAttribute("aria-selected", String(b.dataset.tab === name)));
        if (location.hash.slice(1) !== name) history.replaceState(null, "", "#" + name);
        charts(document);
        if (name === "map") initMap()?.render();
        else mapPlayer?.pause();
      };
      showTab = show;
      nav.addEventListener("click", (e) => {
        const b = e.target.closest("button[data-tab]");
        if (b) show(b.dataset.tab);
      });
      show(location.hash.slice(1));
    });
  }

  // Timeline: filter by player and by events/findings.
  function timeline(root) {
    const list = root.querySelector(".timeline");
    if (!list) return;
    const player = root.querySelector("[data-timeline-player]");
    const kinds = [...root.querySelectorAll("[data-timeline-kind]")];
    const apply = () => {
      const slot = player.value;
      const allowed = new Set(kinds.filter((k) => k.checked).map((k) => k.dataset.timelineKind));
      list.querySelectorAll("li").forEach((li) => {
        const slots = li.dataset.slots ? li.dataset.slots.split(" ") : [];
        li.hidden = !allowed.has(li.dataset.kind) || (slot !== "" && !slots.includes(slot));
      });
    };
    player.addEventListener("change", apply);
    kinds.forEach((k) => k.addEventListener("change", apply));
  }

  // Whole table rows open their match.
  function rowLinks(root) {
    root.querySelectorAll("tr.row-link").forEach((tr) => {
      tr.addEventListener("click", (e) => {
        if (e.target.closest("a, button, form")) return;
        location.href = tr.dataset.href;
      });
    });
  }

  // Match map playback. The terrain and buildings come from the server;
  // this places wards and deaths, then moves everything to the chosen
  // second: standing buildings, live wards, fresh deaths, who is dead and
  // for how long, gold and the latest events. Map cells are 64..192, drawn
  // as x-64 and 192-y.
  const SVG = "http://www.w3.org/2000/svg";
  const TEAM_INS = { radiant: "Силами Света", dire: "Силами Тьмы" };
  const ROSHAN_MIN = 480, ROSHAN_MAX = 660, AEGIS = 300; // game rules, seconds
  let mapPlayer = null;

  function initMap() {
    if (!mapPlayer) {
      const root = document.querySelector("[data-map]");
      const d = data("chart-data");
      if (root && d && d.map) mapPlayer = createMapPlayer(root.closest(".card"), d);
    }
    return mapPlayer;
  }

  function createMapPlayer(card, d) {
    // The Ancient's fall is logged a second after the match's duration.
    const m = d.map, dur = Math.max(1, m.duration, ...(m.buildings || []).map((b) => b.t));
    const q = (sel) => card.querySelector(sel);
    const svg = q("svg.map");
    const layer = (name) => svg.querySelector(`[data-map-layer="${name}"]`);
    const px = (x) => x - 64, py = (y) => 192 - y;
    const node = (tag, attrs, parent) => {
      const e = document.createElementNS(SVG, tag);
      for (const k in attrs) e.setAttribute(k, attrs[k]);
      return parent ? parent.appendChild(e) : e;
    };
    const titled = (e, text) => { node("title", {}, e).textContent = text; return e; };
    const avg = (xs) => xs.reduce((a, b) => a + b, 0) / xs.length;
    // at reads a per-minute series at a second, between the minutes.
    const at = (arr, t) => {
      if (!arr || !arr.length) return null;
      const i = Math.max(0, t / 60), a = Math.floor(i);
      return a >= arr.length - 1 ? arr[arr.length - 1] : arr[a] + (arr[a + 1] - arr[a]) * (i - a);
    };

    // Roster rows, with each player's gold and last hits per minute.
    const series = new Map((d.players || []).map((p) => [p.slot, p]));
    const rows = [...card.querySelectorAll("tr[data-slot]")].map((tr) => ({
      slot: +tr.dataset.slot, hero: tr.dataset.hero, tr, series: series.get(+tr.dataset.slot),
      icon: tr.querySelector("img")?.getAttribute("src"),
      kd: tr.querySelector("[data-kd]"), lh: tr.querySelector("[data-lh]"), gold: tr.querySelector("[data-gold]"),
      respawn: tr.querySelector("[data-respawn]"),
    }));
    const bySlot = new Map(rows.map((r) => [r.slot, r]));

    // The n-th destroyed building with a key takes the n-th element with
    // it; the two tier 4 towers share a key.
    const buildings = [...svg.querySelectorAll(".bld")].map((g) => ({ g, t: Infinity }));
    [...(m.buildings || [])].sort((a, b) => a.t - b.t).forEach((b) => {
      const free = buildings.find((x) => x.g.dataset.key === b.key && x.t === Infinity);
      if (free) free.t = b.t;
    });

    const wards = (m.wards || []).map((w) => {
      const team = teamOf(w.slot);
      const g = node("g", { class: `ward ${w.type} ${team}`, transform: `translate(${px(w.x)} ${py(w.y)})` }, layer("wards"));
      node("circle", { r: w.type === "obs" ? 1.35 : 1.05 }, g);
      titled(g, `${w.type === "obs" ? "Обсервер" : "Сентри"} ${TEAM_GEN[team]}: ${clock(w.t)}–${clock(w.end)}`);
      // An observer sees 1600 units around it, 12.5 map cells.
      const vision = w.type === "obs" ? node("circle", { class: `vision ${team}`, cx: px(w.x), cy: py(w.y), r: 12.5 }, layer("vision")) : null;
      return { ...w, els: vision ? [g, vision] : [g] };
    });

    // Teamfight deaths: the hero's icon while fresh, then a cross.
    const deaths = (m.deaths || []).map((x) => {
      const g = node("g", { class: `death ${teamOf(x.slot)}`, transform: `translate(${px(x.x)} ${py(x.y)})` }, layer("deaths"));
      node("path", { class: "d-x", d: "M-1.3 -1.3L1.3 1.3M1.3 -1.3L-1.3 1.3" }, g);
      const fresh = node("g", { class: "d-fresh" }, g);
      node("circle", { class: "d-pulse", r: 3.3 }, fresh);
      node("circle", { class: "d-ring", r: 3.3 }, fresh);
      const icon = bySlot.get(x.slot)?.icon;
      if (icon) node("image", { href: icon, x: -2.7, y: -2.7, width: 5.4, height: 5.4, "clip-path": "url(#m-round)" }, fresh);
      titled(g, `${x.hero}: смерть в драке, ${clock(x.t)}`);
      return { ...x, g };
    });

    // A ring around each fight's deaths while it lasts.
    const fights = (m.fights || []).map((f, i) => {
      const pts = deaths.filter((x) => x.fight === i).map((x) => [px(x.x), py(x.y)]);
      if (!pts.length) return null;
      const cx = avg(pts.map((p) => p[0])), cy = avg(pts.map((p) => p[1]));
      const r = Math.max(6, ...pts.map((p) => Math.hypot(p[0] - cx, p[1] - cy) + 4));
      return { ...f, c: titled(node("circle", { class: "fight", cx, cy, r }, layer("fights")), `Драка ${clock(f.start)}–${clock(f.end)}`) };
    }).filter(Boolean);

    const kills = [...(m.kills || [])].sort((a, b) => a.t - b.t);
    const buybacks = m.buybacks || [], roshan = m.roshan || [], aegis = m.aegis || [], feed = m.feed || [];
    const goldAdv = d.advantage?.gold || [];

    // Moments the step buttons jump between.
    const moments = [...new Set([...fights.map((f) => f.start), ...(m.buildings || []).map((b) => b.t), ...roshan.map((r) => r.t)])]
      .sort((a, b) => a - b);

    // The scrubber: gold lead, fights and marks for kills, buildings and Roshan.
    const scrub = q("[data-map-scrub]");
    scrub.setAttribute("aria-valuemax", dur);
    const pct = (sec) => `${Math.max(0, Math.min(100, (sec / dur) * 100))}%`;
    const advSvg = scrub.querySelector(".scrub-adv");
    if (goldAdv.length > 1) {
      const top = Math.max(1000, ...goldAdv.map(Math.abs));
      const x = (i) => Math.min(1000, (i * 60 / dur) * 1000).toFixed(1);
      const area = (sign) => `M0 0 ${goldAdv.map((v, i) => `L${x(i)} ${(-sign * Math.max(0, sign * v) / top).toFixed(3)}`).join(" ")} L${x(goldAdv.length - 1)} 0Z`;
      node("path", { class: "adv-radiant", d: area(1) }, advSvg);
      node("path", { class: "adv-dire", d: area(-1) }, advSvg);
      node("line", { class: "adv-zero", x1: 0, x2: 1000, y1: 0, y2: 0 }, advSvg);
    }
    const marks = scrub.querySelector(".scrub-marks");
    const mark = (cls, sec, title, text) => {
      const e = document.createElement("span");
      e.className = cls;
      e.style.left = pct(sec);
      if (title) e.title = title;
      if (text) e.textContent = text;
      return marks.appendChild(e);
    };
    for (const f of m.fights || []) mark("sm-fight", f.start).style.width = `${Math.max(0.4, ((f.end - f.start) / dur) * 100)}%`;
    for (const k of kills) mark(`sm-kill ${opposite(k.team)}`, k.t);
    for (const b of m.buildings || []) mark(`sm-bld ${opposite(b.team)}`, b.t, `${clock(b.t)} — ${b.name}`);
    for (const r of roshan) mark(`sm-rosh ${r.team}`, r.t, `${clock(r.t)} — Рошан`, "Р");
    const step = dur > 2400 ? 600 : 300;
    for (let s = step, i = 1; s < dur - step / 3; s += step, i++) mark(i % 2 ? "sm-tick minor" : "sm-tick", s, "", clock(s));
    const head = scrub.querySelector(".scrub-head"), played = scrub.querySelector(".scrub-played");
    const hover = scrub.querySelector(".scrub-hover");

    const out = q("[data-map-time]"), playBtn = q("[data-map-play]");
    const stat = (name) => q(`[data-map-stat="${name}"]`);
    const scoreEl = { radiant: q('[data-map-score="radiant"]'), dire: q('[data-map-score="dire"]') };
    const feedList = q("[data-map-feed]");
    let t = dur, playing = false, speed = 60, raf = 0, last = 0, feedAt = -2;

    function render() {
      const now = Math.floor(t);
      out.value = clock(now);
      head.style.left = played.style.width = pct(t);
      scrub.setAttribute("aria-valuenow", now);
      scrub.setAttribute("aria-valuetext", clock(now));

      for (const b of buildings) {
        b.g.classList.toggle("down", b.t <= t);
        b.g.classList.toggle("boom", b.t <= t && t - b.t < 12);
      }
      for (const w of wards) {
        const on = w.t <= t && t < w.end;
        for (const e of w.els) e.style.display = on ? "" : "none";
      }
      for (const x of deaths) {
        x.g.style.display = x.t <= t ? "" : "none";
        x.g.classList.toggle("fresh", x.t <= t && t - x.t < 30);
      }
      for (const f of fights) f.c.style.display = f.start <= t && t <= f.end + 3 ? "" : "none";

      // Score, kills and deaths, and who is waiting to respawn.
      const score = { radiant: 0, dire: 0 };
      const kd = new Map(rows.map((r) => [r.slot, { k: 0, d: 0, last: null }]));
      for (const k of kills) {
        if (k.t > t) break;
        score[opposite(k.team)]++;
        const v = kd.get(k.victim);
        if (v) { v.d++; v.last = k; }
        if (k.killer != null && teamOf(k.killer) !== k.team && kd.has(k.killer)) kd.get(k.killer).k++;
      }
      for (const team in score) if (scoreEl[team]) scoreEl[team].textContent = score[team];
      for (const r of rows) {
        const s = kd.get(r.slot);
        // A death with no respawn time came right before the end.
        let wait = 0;
        const until = s.last && (s.last.respawn ? s.last.t + s.last.respawn : Infinity);
        if (s.last && t < until && !buybacks.some((b) => b.slot === r.slot && b.t >= s.last.t && b.t <= t)) {
          wait = Math.ceil(until - t);
        }
        r.tr.classList.toggle("dead", wait > 0);
        r.respawn.textContent = wait > 0 && wait < Infinity ? wait : "";
        r.kd.textContent = `${s.k} / ${s.d}`;
        r.lh.textContent = num(Math.floor(at(r.series?.lh, t) ?? 0));
        r.gold.textContent = num(at(r.series?.gold, t) ?? 0);
      }

      const lead = at(goldAdv, t);
      const gold = stat("gold");
      gold.className = lead ? (lead > 0 ? "radiant" : "dire") : "";
      gold.textContent = lead == null ? "—" : Math.round(lead) === 0 ? "поровну" : `${TEAM[lead > 0 ? "radiant" : "dire"]} +${num(Math.abs(lead))}`;

      const rosh = stat("roshan");
      const killed = roshan.filter((r) => r.t <= t).pop();
      rosh.className = "";
      rosh.title = killed ? `Убит ${TEAM_INS[killed.team]} в ${clock(killed.t)}; возрождается через 8–11 минут` : "";
      if (!killed || t >= killed.t + ROSHAN_MAX) rosh.textContent = "жив";
      else if (t < killed.t + ROSHAN_MIN) {
        rosh.textContent = `мёртв · ${clock(killed.t + ROSHAN_MIN)}–${clock(killed.t + ROSHAN_MAX)}`;
        rosh.className = killed.team;
      } else rosh.textContent = `может появиться до ${clock(killed.t + ROSHAN_MAX)}`;

      const ag = aegis.filter((a) => a.t <= t).pop();
      const holder = ag && bySlot.get(ag.slot);
      const used = ag && kills.some((k) => k.victim === ag.slot && k.t > ag.t && k.t <= t);
      const aeg = stat("aegis");
      if (holder && !used && t < ag.t + AEGIS) {
        aeg.textContent = `${holder.hero} до ${clock(ag.t + AEGIS)}`;
        aeg.className = teamOf(ag.slot);
      } else {
        aeg.textContent = "—";
        aeg.className = "";
      }

      renderFeed();
    }

    // The six latest events, newest first; ones from the last 15 seconds stand out.
    function renderFeed() {
      let i = feed.length - 1;
      while (i >= 0 && feed[i].t > t) i--;
      if (i !== feedAt) {
        feedAt = i;
        const items = feed.slice(Math.max(0, i - 5), i + 1).reverse();
        feedList.replaceChildren(...items.map((it) => {
          const li = document.createElement("li");
          li.className = it.team;
          li.dataset.t = it.t;
          const tm = document.createElement("span");
          tm.className = "t";
          tm.textContent = clock(it.t);
          const dot = document.createElement("span");
          dot.className = "dot";
          const text = document.createElement("span");
          text.textContent = it.text;
          li.append(tm, dot, text);
          return li;
        }));
        if (!items.length) {
          const li = document.createElement("li");
          li.className = "empty";
          li.textContent = "Пока ничего не произошло.";
          feedList.append(li);
        }
      }
      feedList.querySelectorAll("li[data-t]").forEach((li) => li.classList.toggle("new", t - li.dataset.t < 15));
    }

    function frame(time) {
      if (!playing) return;
      if (last) t = Math.min(dur, t + ((time - last) / 1000) * speed);
      last = time;
      render();
      if (t >= dur) pause();
      else raf = requestAnimationFrame(frame);
    }
    function play() {
      if (t >= dur) t = 0;
      playing = true;
      last = 0;
      playBtn.classList.add("playing");
      playBtn.setAttribute("aria-label", "Пауза");
      raf = requestAnimationFrame(frame);
    }
    function pause() {
      playing = false;
      cancelAnimationFrame(raf);
      playBtn.classList.remove("playing");
      playBtn.setAttribute("aria-label", "Воспроизвести");
    }
    const toggle = () => (playing ? pause() : play());
    function seek(sec) {
      t = Math.max(0, Math.min(dur, sec));
      render();
    }

    playBtn.addEventListener("click", toggle);
    card.querySelectorAll("[data-map-step]").forEach((b) => b.addEventListener("click", () => {
      const next = +b.dataset.mapStep > 0 ? moments.find((s) => s > t + 1) : moments.filter((s) => s < t - 2).pop();
      pause();
      seek(next ?? (+b.dataset.mapStep > 0 ? dur : 0));
    }));
    const speeds = card.querySelectorAll("[data-speed]");
    speeds.forEach((b) => b.addEventListener("click", () => {
      speed = +b.dataset.speed;
      speeds.forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
    }));
    card.querySelectorAll("[data-map-show]").forEach((cb) => {
      const apply = () => svg.classList.toggle("hide-" + cb.dataset.mapShow, !cb.checked);
      cb.addEventListener("change", apply);
      apply();
    });

    const secAt = (e) => {
      const r = scrub.getBoundingClientRect();
      return ((e.clientX - r.left) / r.width) * dur;
    };
    scrub.addEventListener("pointerdown", (e) => {
      scrub.setPointerCapture(e.pointerId);
      pause();
      seek(secAt(e));
    });
    scrub.addEventListener("pointermove", (e) => {
      if (scrub.hasPointerCapture(e.pointerId)) seek(secAt(e));
      const s = Math.max(0, Math.min(dur, secAt(e)));
      hover.style.left = pct(s);
      hover.classList.toggle("flip", s > dur * 0.9);
      hover.firstElementChild.textContent = clock(s);
    });
    // Keys: space plays or pauses, arrows move 10 seconds (a minute with
    // Shift), Home and End go to the ends.
    card.addEventListener("keydown", (e) => {
      if (e.target.closest("input, select, textarea") || (e.target.closest("button") && e.key === " ")) return;
      const by = { ArrowLeft: -10, ArrowRight: 10 }[e.key];
      if (e.key === " ") toggle();
      else if (by) { pause(); seek(t + by * (e.shiftKey ? 6 : 1)); }
      else if (e.key === "Home") { pause(); seek(0); }
      else if (e.key === "End") { pause(); seek(dur); }
      else return;
      e.preventDefault();
    });

    render();
    return { render, seek, pause, card };
  }

  // Times in the timeline and key moments open the map at that second.
  function seekLinks(root) {
    root.addEventListener("click", (e) => {
      const b = e.target.closest("[data-seek]");
      if (!b) return;
      showTab("map");
      const player = initMap();
      if (!player) return;
      player.pause();
      player.seek(+b.dataset.seek);
      player.card.scrollIntoView({ behavior: "smooth", block: "start" });
    });
  }

  function init(root) {
    localTimes(root);
    tabs(root);
    timeline(root);
    rowLinks(root);
    seekLinks(root);
    charts(root);
  }

  document.addEventListener("DOMContentLoaded", () => init(document));
  document.addEventListener("htmx:afterSettle", (e) => { localTimes(e.target); rowLinks(e.target); charts(e.target); });
})();
