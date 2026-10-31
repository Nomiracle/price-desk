'use strict';

const BADGE = {
  binance: 'BN', okx: 'OK', bybit: 'BB', bitget: 'BG', gate: 'GT', kucoin: 'KC',
  mexc: 'MX', htx: 'HT', kraken: 'KR', bitstamp: 'BS', bitfinex: 'BF',
  cryptocom: 'CR', coinbase: 'CB',
  'uniswap-eth': 'UN', 'uniswap-base': 'UN', 'uniswap-arb': 'UN',
  'pancakeswap-bsc': 'PC', 'aerodrome-base': 'AE', raydium: 'RY', orca: 'OR',
};
const QUICK = ['BTC', 'ETH', 'SOL', 'BNB', 'XRP', 'DOGE'];
const MAX_HISTORY = 60;

const $ = (id) => document.getElementById(id);
const el = {
  input: $('symbol'), list: $('symbols'), chips: $('chips'), pairstate: $('pairstate'),
  rows: $('rows'), state: $('state'), status: $('status'), pairbig: $('pairbig'),
  auto: $('auto'), interval: $('interval'), showoff: $('showoff'), refresh: $('refresh'),
  scope: $('scope'),
  clock: $('clock'), excount: $('excount'),
  median: $('s-median'), medianSub: $('s-median-sub'),
  ask: $('s-ask'), askSub: $('s-ask-sub'),
  bid: $('s-bid'), bidSub: $('s-bid-sub'),
  spread: $('s-spread'), spreadSub: $('s-spread-sub'), spreadCard: $('s-spread-card'),
};

const store = {
  get(key, fallback) {
    try { const v = localStorage.getItem(key); return v === null ? fallback : v; } catch { return fallback; }
  },
  set(key, value) { try { localStorage.setItem(key, value); } catch { /* private mode */ } },
};

const state = {
  base: (store.get('pd_base', 'BTC') || 'BTC').toUpperCase(),
  data: null,
  history: new Map(),
  showOff: store.get('pd_showoff', '0') === '1',
  scope: ['all', 'cex', 'dex'].includes(store.get('pd_scope', 'all')) ? store.get('pd_scope', 'all') : 'all',
  auto: store.get('pd_auto', '1') !== '0',
  interval: Number(store.get('pd_int', '5000')) || 5000,
  sort: { key: 'price', dir: -1 },
  loading: false,
  lastFetchAt: 0,
  lastError: '',
  abort: null,
};

/* ---------- formatting ---------- */

function fmtPrice(v) {
  if (!v || !Number.isFinite(v)) return '—';
  const abs = Math.abs(v);
  let dp;
  if (abs >= 1000) dp = 2;
  else if (abs >= 100) dp = 3;
  else if (abs >= 1) dp = 4;
  else if (abs >= 0.1) dp = 5;
  else if (abs >= 0.01) dp = 6;
  else dp = 8;
  return v.toLocaleString('en-US', { minimumFractionDigits: dp, maximumFractionDigits: dp });
}

function fmtCompact(v) {
  if (!v || !Number.isFinite(v)) return '—';
  if (v >= 1e8) return (v / 1e8).toFixed(2) + ' 亿';
  if (v >= 1e4) return (v / 1e4).toFixed(1) + ' 万';
  return v.toFixed(0);
}

// Pool liquidity is a USD figure from the indexer; keep it in USD units so it
// is not confused with the USDT-converted price columns.
function fmtUSD(v) {
  if (!v || !Number.isFinite(v)) return '—';
  if (v >= 1e9) return '$' + (v / 1e9).toFixed(2) + 'B';
  if (v >= 1e6) return '$' + (v / 1e6).toFixed(1) + 'M';
  if (v >= 1e3) return '$' + (v / 1e3).toFixed(0) + 'K';
  return '$' + v.toFixed(0);
}

function fmtBps(v) {
  if (!Number.isFinite(v)) return '—';
  const sign = v > 0 ? '+' : '';
  return sign + v.toFixed(1);
}

function fmtGross(v) {
  if (!Number.isFinite(v)) return '—';
  const a = Math.abs(v);
  if (a >= 0.01) return v.toFixed(2);
  if (a > 0) return v.toPrecision(3);
  return '0';
}

function fmtPct(v) {
  if (v === null || v === undefined || !Number.isFinite(v)) return '—';
  return (v > 0 ? '+' : '') + v.toFixed(2) + '%';
}

function fmtClock(ts) {
  if (!ts) return '—';
  return new Date(ts).toLocaleTimeString('zh-CN', { hour12: false });
}

function text(node, value) {
  node.textContent = value;
  return node;
}

/* ---------- network ---------- */

async function fetchSymbols() {
  try {
    const res = await fetch('/api/symbols', { cache: 'no-store' });
    if (!res.ok) return;
    const body = await res.json();
    if (body.exchanges) {
      el.excount.textContent = body.exchanges + ' venues' + (body.dex_exchanges ? ' · ' + body.dex_exchanges + ' DEX' : '');
    }
    el.list.textContent = '';
    for (const s of body.symbols || []) {
      const opt = document.createElement('option');
      opt.value = s.base;
      opt.label = s.name;
      el.list.appendChild(opt);
    }
  } catch { /* the page still works with free-form input */ }
}

async function load(base) {
  if (state.abort) state.abort.abort();
  const ctrl = new AbortController();
  state.abort = ctrl;
  state.loading = true;
  if (!state.data || state.data.base !== base) {
    el.state.textContent = '加载中…';
    el.state.className = 'state';
    el.state.style.display = 'block';
  }
  try {
    const res = await fetch('/api/compare?base=' + encodeURIComponent(base), { cache: 'no-store', signal: ctrl.signal });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const body = await res.json();
    if (body.base !== state.base) return; // a newer request already switched the pair
    state.data = body;
    state.lastError = '';
    state.lastFetchAt = Date.now();
    recordHistory(body);
    render();
  } catch (err) {
    if (err.name === 'AbortError') return;
    state.lastError = err.message || '请求失败';
    if (state.data) {
      renderStatus();
    } else {
      el.state.textContent = '数据加载失败：' + state.lastError + '（稍后会自动重试）';
      el.state.className = 'state err';
      el.state.style.display = 'block';
    }
  } finally {
    state.loading = false;
  }
}

function recordHistory(body) {
  const seen = new Set();
  for (const q of body.quotes || []) {
    if (!q.price_usdt || q.error || q.not_listed) continue;
    seen.add(q.exchange);
    const arr = state.history.get(q.exchange) || [];
    arr.push(q.price_usdt);
    if (arr.length > MAX_HISTORY) arr.shift();
    state.history.set(q.exchange, arr);
  }
  for (const key of [...state.history.keys()]) if (!seen.has(key)) state.history.delete(key);
}

/* ---------- rendering ---------- */

function sortedQuotes() {
  const list = (state.data?.quotes || []).filter((q) => {
    if (state.scope !== 'all' && q.kind !== state.scope) return false;
    return state.showOff || (!q.error && !q.not_listed);
  });
  const { key, dir } = state.sort;
  const num = (q) => {
    switch (key) {
      case 'change': return q.change_24h === null || q.change_24h === undefined ? -Infinity : q.change_24h;
      case 'bid': return q.bid_usdt || 0;
      case 'ask': return q.ask_usdt || 0;
      case 'spread': return q.ask_usdt > 0 && q.bid_usdt > 0 ? (q.ask_usdt - q.bid_usdt) / ((q.ask_usdt + q.bid_usdt) / 2) * 10000 : -Infinity;
      case 'dev': return q.price_usdt && state.data?.summary?.median_price ? (q.price_usdt / state.data.summary.median_price - 1) * 10000 : -Infinity;
      case 'vol': return q.volume_24h_quote || 0;
      case 'lat': return q.latency_ms || 0;
      default: return q.price_usdt || 0;
    }
  };
  list.sort((a, b) => {
    if (key === 'name') return dir * a.name.localeCompare(b.name);
    const av = num(a), bv = num(b);
    if (av === bv) return a.name.localeCompare(b.name);
    return dir * (av > bv ? 1 : -1);
  });
  return list;
}

function render() {
  if (!state.data) return;
  const s = state.data.summary;
  const quotes = sortedQuotes();

  el.pairbig.textContent = state.data.base;
  const q = document.createElement('span');
  q.textContent = '/' + state.data.quote;
  el.pairbig.appendChild(q);
  el.pairstate.textContent = '/ ' + state.data.quote;

  text(el.median, fmtPrice(s.median_price));
  text(el.medianSub, `${s.ok_count}/${s.total_count} 所中位 · 离散度 ${s.dispersion_bps.toFixed(1)} bps`);

  if (s.best_ask) {
    text(el.ask, fmtPrice(s.best_ask.price));
    text(el.askSub, `${s.best_ask.name} · 全市场最低卖一`);
  } else { text(el.ask, '—'); text(el.askSub, '无可用盘口'); }
  if (s.best_bid) {
    text(el.bid, fmtPrice(s.best_bid.price));
    text(el.bidSub, `${s.best_bid.name} · 全市场最高买一`);
  } else { text(el.bid, '—'); text(el.bidSub, '无可用盘口'); }

  text(el.spread, fmtBps(s.cross_spread_bps));
  el.spread.className = 'v';
  el.spread.style.color = s.cross_spread_bps > 0 ? 'var(--up)' : 'var(--muted)';
  if (s.best_ask && s.best_bid) {
    if (s.cross_spread_bps > 0) {
      text(el.spreadSub, `买 ${s.best_ask.name} → 卖 ${s.best_bid.name} · 每币 ${fmtGross(s.cross_spread_usdt)} U`);
    } else {
      text(el.spreadSub, `盘口未重叠 · 卖一仍高于买一 ${fmtGross(Math.abs(s.cross_spread_usdt))} U`);
    }
  } else text(el.spreadSub, '盘口不足');

  renderRows(quotes, s);
  renderStatus();

  el.state.style.display = quotes.length ? 'none' : 'block';
  if (!quotes.length) {
    el.state.className = 'state';
    if (state.scope === 'dex') {
      el.state.textContent = '该币种在这些链上没有已收录的池子，试试其他币种或切回「全部场所」。';
    } else {
      el.state.textContent = state.showOff ? '没有返回任何交易所数据。' : '所有交易所均无此交易对，试试「显示未上架」。';
    }
  }
  syncSortMarks();
}

function renderRows(quotes, s) {
  el.rows.textContent = '';
  const bestAskEx = s.best_ask?.exchange;
  const bestBidEx = s.best_bid?.exchange;

  for (const q of quotes) {
    const tr = document.createElement('tr');
    if (q.exchange === bestAskEx) tr.className = 'best-ask';
    else if (q.exchange === bestBidEx) tr.className = 'best-bid';
    if (q.error || q.not_listed) tr.classList.add('off');
    if (q.low_liquidity) tr.classList.add('lowliq');

    // exchange
    const tdName = document.createElement('td');
    tdName.className = 'name';
    const ex = document.createElement('div');
    ex.className = 'ex';
    const badge = document.createElement('span');
    badge.className = 'badge';
    text(badge, BADGE[q.exchange] || q.name.slice(0, 2).toUpperCase());
    const meta = document.createElement('div');
    const nm = document.createElement('div');
    nm.className = 'ex-name';
    text(nm, q.name);
    if (q.kind === 'dex') {
      const t = document.createElement('span'); t.className = 'tag dex'; text(t, 'DEX'); nm.appendChild(t);
    }
    if (q.low_liquidity) {
      const t = document.createElement('span'); t.className = 'tag thin'; text(t, '低流动'); nm.appendChild(t);
    }
    if (q.exchange === bestAskEx) {
      const t = document.createElement('span'); t.className = 'tag ask'; text(t, '最低卖一'); nm.appendChild(t);
    } else if (q.exchange === bestBidEx) {
      const t = document.createElement('span'); t.className = 'tag bid'; text(t, '最高买一'); nm.appendChild(t);
    }
    const sub = document.createElement('div');
    sub.className = 'ex-sub';
    if (q.kind === 'dex') {
      const parts = [q.chain_name, q.market];
      if (q.liquidity_usd > 0) parts.push('LP ' + fmtUSD(q.liquidity_usd));
      text(sub, parts.filter(Boolean).join(' · '));
    } else {
      text(sub, q.market + (q.quote === 'USD' ? ' · USD' : ''));
    }
    meta.append(nm, sub);
    ex.append(badge, meta);
    tdName.appendChild(ex);

    // price
    const tdPrice = document.createElement('td');
    tdPrice.className = 'price num';
    if (q.error || q.not_listed) {
      text(tdPrice, q.error || '未上架');
      tdPrice.classList.add('miss');
    } else {
      const p = document.createElement('span');
      p.className = 'price';
      text(p, (q.quote === 'USD' ? '≈' : '') + fmtPrice(q.price_usdt));
      tdPrice.appendChild(p);
      if (q.quote === 'USD') {
        const note = document.createElement('small');
        text(note, fmtPrice(q.price) + ' USD');
        tdPrice.appendChild(note);
      }
      const m = document.createElement('div');
      m.className = 'm-sub';
      const spreadBps = q.ask_usdt > 0 && q.bid_usdt > 0
        ? (q.ask_usdt - q.bid_usdt) / ((q.ask_usdt + q.bid_usdt) / 2) * 10000 : NaN;
      if (q.kind === 'dex') {
        text(m, `${q.chain_name || '链上'} · 池子 ${q.market} · 流动性 ${fmtUSD(q.liquidity_usd)} · 额 ${fmtCompact(q.volume_24h_quote)}`);
      } else {
        text(m, `买 ${fmtPrice(q.bid_usdt)} / 卖 ${fmtPrice(q.ask_usdt)} · 点差 ${fmtBps(spreadBps)}bps · 额 ${fmtCompact(q.volume_24h_quote)}`);
      }
      tdPrice.appendChild(m);
    }

    // change
    const tdChg = document.createElement('td');
    tdChg.className = 'chg num';
    const ch = document.createElement('span');
    ch.className = 'chg ' + (q.change_24h === null || q.change_24h === undefined ? 'flat' : q.change_24h > 0 ? 'up' : q.change_24h < 0 ? 'down' : 'flat');
    text(ch, fmtPct(q.change_24h));
    tdChg.appendChild(ch);

    // bid / ask
    const tdBid = document.createElement('td');
    tdBid.className = 'num col-opt';
    text(tdBid, fmtPrice(q.bid_usdt));
    const tdAsk = document.createElement('td');
    tdAsk.className = 'num col-opt';
    text(tdAsk, fmtPrice(q.ask_usdt));

    // spread
    const tdSpread = document.createElement('td');
    tdSpread.className = 'num col-opt';
    const spread = q.ask_usdt > 0 && q.bid_usdt > 0
      ? (q.ask_usdt - q.bid_usdt) / ((q.ask_usdt + q.bid_usdt) / 2) * 10000 : NaN;
    text(tdSpread, fmtBps(spread));

    // deviation from median
    const tdDev = document.createElement('td');
    tdDev.className = 'num dev';
    const dev = q.price_usdt && s.median_price ? (q.price_usdt / s.median_price - 1) * 10000 : NaN;
    const devNode = document.createElement('span');
    devNode.className = 'dev ' + (dev > 0.05 ? 'pos' : dev < -0.05 ? 'neg' : 'zero');
    text(devNode, fmtBps(dev));
    tdDev.appendChild(devNode);

    // volume
    const tdVol = document.createElement('td');
    tdVol.className = 'num vol col-opt';
    text(tdVol, fmtCompact(q.volume_24h_quote));

    // latency
    const tdLat = document.createElement('td');
    tdLat.className = 'num col-opt';
    const lat = document.createElement('span');
    lat.className = 'lat' + (q.latency_ms > 2500 ? ' bad' : q.latency_ms > 900 ? ' warn' : '');
    text(lat, (q.latency_ms || 0) + 'ms');
    tdLat.appendChild(lat);

    // sparkline
    const tdSpark = document.createElement('td');
    tdSpark.className = 'col-spark';
    tdSpark.appendChild(sparkline(q.exchange, q.price_usdt));

    // mobile-only detail line (bid/ask/spread/volume live in .col-opt cells)
    const tdDetail = document.createElement('td');
    tdDetail.className = 'm-detail';
    if (!q.error && !q.not_listed) {
      if (q.kind === 'dex') {
        tdDetail.textContent = `${q.chain_name || '链上'} · 池子 ${q.market} · 流动性 ${fmtUSD(q.liquidity_usd)} · 24h额 ${fmtCompact(q.volume_24h_quote)} · ${q.latency_ms || 0}ms`;
      } else {
        tdDetail.textContent = `买 ${fmtPrice(q.bid_usdt)} · 卖 ${fmtPrice(q.ask_usdt)} · 点差 ${fmtBps(spread)}bps · 24h额 ${fmtCompact(q.volume_24h_quote)} · ${q.latency_ms || 0}ms`;
      }
    } else if (q.kind === 'dex') {
      tdDetail.textContent = q.error || '该链上没有此币种的已收录池子';
    } else {
      tdDetail.textContent = q.error || '该交易所未上架此交易对';
    }

    tr.append(tdName, tdPrice, tdChg, tdBid, tdAsk, tdSpread, tdDev, tdVol, tdLat, tdSpark, tdDetail);
    el.rows.appendChild(tr);
  }
}

function sparkline(exchange, current) {
  const svgNS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(svgNS, 'svg');
  const arr = (state.history.get(exchange) || []).filter((v) => Number.isFinite(v));
  svg.setAttribute('class', 'spark' + (arr.length < 2 ? ' flat' : current > (state.data?.summary?.median_price || 0) ? ' bid' : ''));
  svg.setAttribute('viewBox', '0 0 78 26');
  svg.setAttribute('preserveAspectRatio', 'none');
  const path = document.createElementNS(svgNS, 'path');
  if (arr.length < 2) {
    path.setAttribute('d', 'M0 13 L78 13');
    path.setAttribute('stroke', 'rgba(255,255,255,.14)');
  } else {
    const min = Math.min(...arr), max = Math.max(...arr);
    const span = max - min || 1;
    const step = 78 / (arr.length - 1);
    path.setAttribute('d', arr.map((v, i) => `${i === 0 ? 'M' : 'L'}${(i * step).toFixed(1)} ${(24 - ((v - min) / span) * 22).toFixed(1)}`).join(' '));
  }
  svg.appendChild(path);
  return svg;
}

function renderStatus() {
  const s = state.data?.summary;
  el.status.className = 'status' + (state.lastError ? ' err' : '');
  el.status.textContent = '';
  const parts = [];
  if (state.lastError) parts.push('刷新失败，显示 ' + fmtClock(state.lastFetchAt) + ' 数据');
  else parts.push('更新 ' + fmtClock(state.lastFetchAt));
  if (s) parts.push(`${s.ok_count}/${s.total_count} 可用`);
  if (state.data?.elapsed_ms) parts.push(state.data.elapsed_ms + 'ms');
  if (state.auto) parts.push('自动 ' + Math.round(state.interval / 1000) + 's');
  parts.forEach((p, i) => {
    if (i) {
      const sep = document.createElement('span');
      sep.textContent = ' · ';
      sep.style.color = 'var(--dim)';
      el.status.appendChild(sep);
    }
    const b = document.createElement(i === 0 ? 'b' : 'span');
    text(b, p);
    el.status.appendChild(b);
  });
}

function syncSortMarks() {
  for (const th of document.querySelectorAll('thead th[data-sort]')) {
    const on = th.dataset.sort === state.sort.key;
    th.classList.toggle('sorted', on);
    const arrow = th.querySelector('.arrow');
    if (arrow) arrow.textContent = state.sort.dir > 0 ? '▲' : '▼';
  }
}

/* ---------- interaction ---------- */

function setBase(base) {
  const clean = String(base || '').trim().toUpperCase();
  if (!/^[A-Z0-9]{2,12}$/.test(clean)) {
    el.input.value = state.base;
    return;
  }
  if (clean === state.base && state.data) { load(clean); return; }
  state.base = clean;
  state.history.clear();
  state.data = null;
  state.lastError = '';
  store.set('pd_base', clean);
  el.input.value = clean;
  history.replaceState(null, '', '#' + clean);
  document.title = clean + ' 多所比价 · Price Desk';
  renderChips();
  load(clean);
}

function renderChips() {
  el.chips.textContent = '';
  for (const sym of QUICK) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chip' + (sym === state.base ? ' on' : '');
    text(b, sym);
    b.addEventListener('click', () => setBase(sym));
    el.chips.appendChild(b);
  }
}

function startTimer() {
  if (state.timer) clearInterval(state.timer);
  state.timer = setInterval(() => {
    if (!state.auto || document.hidden) return;
    if (state.loading) return;
    if (Date.now() - state.lastFetchAt >= state.interval) load(state.base);
  }, 400);
}

/* ---------- dislocation monitor ---------- */

function watchRow(e) {
  const row = document.createElement('div');
  row.className = 'wev';

  const sev = document.createElement('span');
  const level = e.severity || 'info';
  sev.className = 'sev ' + level;
  sev.textContent = { alert: 'ALERT', watch: 'WATCH', info: 'INFO' }[level] || 'INFO';
  row.appendChild(sev);

  const what = document.createElement('b');
  if (e.kind === 'dispersion') {
    what.textContent = e.symbol + ' 报价离散';
  } else if (e.kind === 'stablecoin_peg') {
    what.textContent = e.symbol + '/USD ' + (e.price || 0).toFixed(5);
  } else if (e.kind === 'venue_down' || e.kind === 'venue_recovered') {
    what.textContent = e.symbol + ' · ' + (e.venue_name || e.venue || '');
  } else if (e.kind === 'venue_normal') {
    what.textContent = e.symbol + ' · ' + (e.venue_name || e.venue || '') + ' 回归';
  } else {
    what.textContent = e.symbol + ' · ' + (e.venue_name || e.venue || '');
  }
  row.appendChild(what);

  if (typeof e.dev_bps === 'number' && e.kind !== 'venue_down' && e.kind !== 'venue_recovered') {
    const dev = document.createElement('span');
    const sign = e.dev_bps > 0 ? '+' : '';
    dev.textContent = sign + e.dev_bps.toFixed(1) + ' bps';
    dev.style.color = e.dev_bps > 0 ? 'var(--up)' : 'var(--down)';
    row.appendChild(dev);
  }

  const t = document.createElement('span');
  t.className = 't';
  t.textContent = new Date(e.at).toLocaleTimeString('zh-CN', { hour12: false });
  row.appendChild(t);
  return row;
}

async function loadWatch() {
  try {
    const [st, ev] = await Promise.all([
      fetch('/api/status', { cache: 'no-store' }).then((r) => r.json()),
      fetch('/api/events?limit=3&since=' + (Date.now() - 86400000), { cache: 'no-store' }).then((r) => r.json()),
    ]);
    if (!st || !st.enabled) return;
    const box = $('watch');
    const list = $('watch-list');
    const stateEl = $('watch-state');
    box.classList.add('on');

    let status = st.warmup
      ? '基线预热中 ' + st.ready_symbols + '/' + st.total_symbols + ' 币种'
      : '监测中 · 每 ' + st.interval_s + 's 采样 · ' + st.cycles + ' 轮';
    if (st.alerts_open > 0) status += ' · 未回归 ' + st.alerts_open;
    if (st.notifier === 'log') status += ' · 仅记录';
    stateEl.textContent = status;

    const events = (ev && ev.events) || [];
    list.textContent = '';
    if (!events.length) {
      const empty = document.createElement('div');
      empty.className = 'wev';
      const b = document.createElement('b');
      b.textContent = '近 24h 无脱锚事件';
      empty.appendChild(b);
      list.appendChild(empty);
      return;
    }
    for (const e of events) list.appendChild(watchRow(e));
  } catch {
    /* the monitor panel must never break the price table */
  }
}

/* ---------- boot ---------- */

el.chips.textContent = '';
renderChips();
el.auto.checked = state.auto;
el.interval.value = String(state.interval);
el.scope.value = state.scope;
el.showoff.setAttribute('aria-pressed', String(state.showOff));
el.input.value = state.base;
el.pairstate.textContent = '/ USDT';
el.pairbig.textContent = state.base;
const qq = document.createElement('span'); qq.textContent = '/USDT'; el.pairbig.appendChild(qq);
if (location.hash) {
  const fromHash = location.hash.replace('#', '').toUpperCase();
  if (/^[A-Z0-9]{2,12}$/.test(fromHash)) state.base = fromHash;
  el.input.value = state.base;
}
document.title = state.base + ' 多所比价 · Price Desk';
history.replaceState(null, '', '#' + state.base);

el.input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); setBase(el.input.value); el.input.blur(); }
});
el.input.addEventListener('change', () => setBase(el.input.value));
el.refresh.addEventListener('click', () => load(state.base));
el.auto.addEventListener('change', () => {
  state.auto = el.auto.checked;
  store.set('pd_auto', state.auto ? '1' : '0');
  if (state.auto) load(state.base);
  renderStatus();
});
el.interval.addEventListener('change', () => {
  state.interval = Number(el.interval.value) || 5000;
  store.set('pd_int', String(state.interval));
  renderStatus();
});
el.scope.addEventListener('change', () => {
  state.scope = ['all', 'cex', 'dex'].includes(el.scope.value) ? el.scope.value : 'all';
  store.set('pd_scope', state.scope);
  render();
});
el.showoff.addEventListener('click', () => {
  state.showOff = !state.showOff;
  el.showoff.setAttribute('aria-pressed', String(state.showOff));
  store.set('pd_showoff', state.showOff ? '1' : '0');
  render();
});
for (const th of document.querySelectorAll('thead th[data-sort]')) {
  th.addEventListener('click', () => {
    const key = th.dataset.sort;
    if (state.sort.key === key) state.sort.dir *= -1;
    else state.sort = { key, dir: key === 'name' ? 1 : -1 };
    render();
  });
}
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && state.auto && Date.now() - state.lastFetchAt > state.interval) load(state.base);
});
window.addEventListener('hashchange', () => {
  const h = location.hash.replace('#', '').toUpperCase();
  if (h && h !== state.base && /^[A-Z0-9]{2,12}$/.test(h)) setBase(h);
});
setInterval(() => {
  el.clock.textContent = new Date().toLocaleTimeString('zh-CN', { hour12: false }) + ' 本地';
}, 1000);

fetchSymbols();
load(state.base);
startTimer();
loadWatch();
setInterval(loadWatch, 20000);
