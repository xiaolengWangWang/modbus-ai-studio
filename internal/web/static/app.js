'use strict';

// Modbus AI Studio Linux Web 版的页面。状态都在服务端：页面打开后取一次完整状态，之后靠推送（/api/events）更新。

const $ = (id) => document.getElementById(id);

// h 创建元素。文本一律走 textContent，设备返回的名称、错误信息不会被当成 HTML。
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false || c === '') continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

// 浏览器本地的小偏好（标签页、报文区高度、上次的连接参数）。隐私模式下读写会失败，失败就用默认值。
const prefs = {
  get(k, d) {
    try { const v = localStorage.getItem('modbus-ai.' + k); return v === null ? d : JSON.parse(v); } catch { return d; }
  },
  set(k, v) {
    try { localStorage.setItem('modbus-ai.' + k, JSON.stringify(v)); } catch { /* 忽略 */ }
  },
};

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (method !== 'GET') { // 写操作一律发 JSON，服务端据此拒绝跨站提交
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body ?? {});
  }
  let r;
  try {
    r = await fetch(path, opt);
  } catch {
    throw new Error('连不上服务，请检查网络或程序是否在运行');
  }
  let data = null;
  try { data = await r.json(); } catch { /* 下载等非 JSON 响应 */ }
  if (r.status === 401 && path !== '/api/login') {
    showLogin('登录已过期，请重新登录');
    throw new Error('请先登录');
  }
  if (!r.ok) throw new Error((data && data.error) || `HTTP ${r.status}`);
  return data;
}

// ---- 数据类型与字节序 ----

const TYPES = ['UINT16', 'INT16', 'UINT32', 'INT32', 'FLOAT32', 'UINT64', 'INT64', 'FLOAT64'];
const WIDTH = { UINT16: 0, INT16: 0, UINT32: 1, INT32: 1, FLOAT32: 1, UINT64: 2, INT64: 2, FLOAT64: 2 };
const ORDERS = [['AB', 'BA'], ['ABCD', 'CDAB', 'BADC', 'DCBA'], ['ABCDEFGH', 'GHEFCDAB', 'BADCFEHG', 'HGFEDCBA']];
// 同一种字节序在 16 / 32 / 64 位下的写法，换类型时保持同一种排列
const FAMILIES = [['AB', 'ABCD', 'ABCDEFGH'], ['BA', 'BADC', 'BADCFEHG'], ['AB', 'CDAB', 'GHEFCDAB'], ['BA', 'DCBA', 'HGFEDCBA']];
const ORDER_TEXT = {
  AB: 'AB 大端', BA: 'BA 字节交换',
  ABCD: 'ABCD 大端', CDAB: 'CDAB 字交换', BADC: 'BADC 字节交换', DCBA: 'DCBA 小端',
  ABCDEFGH: 'ABCDEFGH 大端', GHEFCDAB: 'GHEFCDAB 字交换', BADCFEHG: 'BADCFEHG 字节交换', HGFEDCBA: 'HGFEDCBA 小端',
};

function setupTypes(typeSel, orderSel, type, order) {
  typeSel.replaceChildren(...TYPES.map((t) => h('option', { value: t }, t)));
  typeSel.value = TYPES.includes(type) ? type : 'UINT16';
  const fill = (wanted) => {
    const list = ORDERS[WIDTH[typeSel.value]];
    orderSel.replaceChildren(...list.map((o) => h('option', { value: o }, ORDER_TEXT[o])));
    orderSel.value = list.includes(wanted) ? wanted : list[0];
  };
  // 当前字节序属于哪一种排列。16 位的 AB / BA 各对应两种 32 位排列（AB 是 ABCD 或 CDAB），
  // 保持原来那种，FLOAT32 CDAB 切到 UINT16 再切回来仍是 CDAB。
  let fam = 0;
  const pick = () => {
    if (!FAMILIES[fam].includes(orderSel.value)) fam = Math.max(0, FAMILIES.findIndex((f) => f.includes(orderSel.value)));
  };
  typeSel.onchange = () => { pick(); fill(FAMILIES[fam][WIDTH[typeSel.value]]); };
  orderSel.onchange = pick;
  fill(order);
  pick();
}

// ---- 登录 ----

let events = null;
let retryTimer = null;

function showLogin(msg) {
  if (events) { events.close(); events = null; }
  clearTimeout(retryTimer);
  $('app').hidden = true;
  $('login').hidden = false;
  $('login-error').textContent = msg || '';
  $('password').value = '';
  $('password').focus();
}

async function enterApp() {
  $('login').hidden = true;
  $('app').hidden = false;
  packets = [];
  lastSeq = 0;
  renderPackets();
  listen(); // 推送连上后先补发服务端已有的报文
  refreshPorts();
  if (!$('tab-records').hidden) loadRecords();
}

function listen() {
  if (events) events.close();
  events = new EventSource('/api/events?since=' + lastSeq);
  events.addEventListener('state', (e) => render(JSON.parse(e.data)));
  events.addEventListener('packets', (e) => addPackets(JSON.parse(e.data)));
  events.onerror = async () => {
    // 网络中断时浏览器会自己重连；登录失效或服务端拒绝时连接关闭，查一下原因
    if (events.readyState !== EventSource.CLOSED) return;
    try {
      const s = await (await fetch('/api/session')).json();
      if (!s.loggedIn) { showLogin('登录已过期，请重新登录'); return; }
    } catch { /* 服务暂时连不上 */ }
    retryTimer = setTimeout(listen, 3000);
  };
}

// ---- 状态 ----

let state = null;

function render(st) {
  state = st;
  $('version').textContent = st.version ? ' ' + st.version : '';
  renderConn(st.conn);
  renderTables(st.tables);
  renderLogs(st.logs || []);
  renderProbe(st.probe);
}

const CONN_TEXT = { connected: '已连接', connecting: '连接中…', reconnecting: '连接断开，正在重连', disconnected: '未连接', probing: '检测寄存器中，常规连接暂时让给检测' };
let connFilled = false;
// 窄屏时连接设置是否收起：连上后自动收起，断开后自动展开；点顶部连接状态手动切换，直到连接状态再变化。
let connFold = null;
let connFoldState = '';

function applyConnFold(c) {
  if (c.state !== connFoldState) { connFoldState = c.state; connFold = null; }
  $('app').classList.toggle('conn-folded', connFold ?? c.state === 'connected');
}

function renderConn(c) {
  const chip = $('conn-chip');
  chip.className = 'conn-chip ' + c.state;
  let text = CONN_TEXT[c.state] || c.state;
  if (c.desc) text += ' · ' + c.desc;
  if (c.error && c.state !== 'connected') text += (c.state === 'disconnected' ? ' · 上次失败：' : ' · ') + c.error;
  $('conn-text').textContent = text;
  chip.title = text + (c.local ? `\n本机 ${c.local} → 设备 ${c.remote}` : '') + (c.since ? `\n自 ${c.since}` : '') + '\n（窄屏时点击显示或收起连接设置）';
  applyConnFold(c);
  $('connect').disabled = c.state === 'connecting';
  $('disconnect').disabled = c.state === 'disconnected';
  if (!connFilled) {
    connFilled = true;
    fillConn(c.config && c.config.mode ? c.config : prefs.get('conn', null));
  }
}

function fillConn(cfg) {
  if (cfg) {
    $('c-mode').value = cfg.mode || 'tcp';
    $('c-target').value = cfg.target || '';
    $('c-sim').checked = !!cfg.simulator;
    if (cfg.baud) $('c-baud').value = String(cfg.baud);
    if (cfg.dataBits) $('c-databits').value = String(cfg.dataBits);
    if (cfg.parity) $('c-parity').value = cfg.parity;
    if (cfg.stopBits) $('c-stop').value = String(cfg.stopBits);
    if (cfg.timeoutMs) $('c-timeout').value = cfg.timeoutMs;
    if (cfg.port) $('c-port').dataset.want = cfg.port;
  }
  updateModeFields();
}

function connConfig() {
  return {
    mode: $('c-mode').value,
    target: $('c-target').value.trim(),
    port: $('c-port').value,
    baud: +$('c-baud').value,
    dataBits: +$('c-databits').value,
    parity: $('c-parity').value,
    stopBits: +$('c-stop').value,
    timeoutMs: +$('c-timeout').value,
    simulator: $('c-sim').checked,
  };
}

function isSerial() { return ['rtu', 'ascii'].includes($('c-mode').value); }

function updateModeFields() {
  const serial = isSerial();
  document.querySelectorAll('.connbar .tcp-only').forEach((el) => { el.hidden = serial; });
  document.querySelectorAll('.connbar .serial-only').forEach((el) => { el.hidden = !serial; });
  $('c-target').disabled = $('c-sim').checked;
}

function setMsg(text, bad) {
  const m = $('conn-msg');
  m.textContent = text || '';
  m.className = 'msg' + (bad ? ' err' : '');
}

async function doConnect() {
  const cfg = connConfig();
  setMsg('正在连接…');
  try {
    await api('POST', '/api/connect', cfg);
    prefs.set('conn', cfg);
    setMsg('');
  } catch (e) {
    setMsg(e.message, true);
  }
}

async function refreshPorts() {
  const sel = $('c-port');
  const want = sel.value || sel.dataset.want || '';
  try {
    const ports = await api('GET', '/api/serial-ports');
    sel.replaceChildren(...(ports.length ? ports.map((p) => h('option', { value: p }, p)) : [h('option', { value: '' }, '（没有找到串口）')]));
    if (ports.includes(want)) sel.value = want;
  } catch (e) {
    if (isSerial()) setMsg(e.message, true);
  }
}

// 两步确认：第一次点击变成“确认…”，3 秒内再点一次才执行。不用浏览器的弹窗。
function confirmStep(btn, label, action) {
  if (btn.dataset.armed) {
    delete btn.dataset.armed;
    btn.textContent = btn.dataset.text;
    btn.classList.remove('danger');
    action();
    return;
  }
  btn.dataset.armed = '1';
  btn.dataset.text = btn.textContent;
  btn.textContent = label;
  btn.classList.add('danger');
  setTimeout(() => {
    if (btn.dataset.armed) {
      delete btn.dataset.armed;
      btn.textContent = btn.dataset.text;
      btn.classList.remove('danger');
    }
  }, 3000);
}

// ---- 读取表 ----

const cards = new Map(); // 读取表 ID → 已经画好的卡片，状态推送时只改变化的文字

const STAT = {
  ok: (t) => `正常 · ${t.rtt.toFixed(1)} ms · ${t.updated} · 第 ${t.polls} 次${t.fails ? `，失败 ${t.fails} 次` : ''}`,
  error: (t) => `读取失败 · ${t.updated} · 失败 ${t.fails} / ${t.polls} 次`,
  offline: () => (state && state.conn.state === 'reconnecting' ? '等待重连' : '未连接'),
  paused: () => '已暂停',
  probing: () => '检测寄存器中，暂停读取',
  waiting: () => '等待读取…',
};

function renderTables(tables) {
  $('tables-empty').hidden = tables.length > 0;
  $('pause-all').hidden = !tables.length;
  $('set-slave').hidden = !tables.length;
  $('pause-all').textContent = tables.some((t) => !t.paused) ? '全部暂停' : '全部继续';
  const box = $('tables');
  const seen = new Set();
  tables.forEach((t, i) => {
    seen.add(t.id);
    const key = [t.title, t.config.name, t.writable, ...t.rows.map((r) => r.ref + '|' + (r.name || ''))].join('\n');
    let c = cards.get(t.id);
    if (!c || c.key !== key) {
      const fresh = buildCard(t);
      fresh.key = key;
      if (c) c.el.replaceWith(fresh.el);
      c = fresh;
      cards.set(t.id, c);
    }
    updateCard(c, t);
    if (box.children[i] !== c.el) box.insertBefore(c.el, box.children[i] || null);
  });
  for (const [id, c] of cards) {
    if (!seen.has(id)) { c.el.remove(); cards.delete(id); }
  }
}

function buildCard(t) {
  const c = { t };
  const named = t.rows.some((r) => r.name);
  const units = t.rows.some((r) => r.unit);
  c.name = h('span', { class: 'name' });
  c.stat = h('span', { class: 'stat' });
  c.pause = h('button', { onclick: () => api('POST', `/api/tables/${c.t.id}/pause`, { paused: !c.t.paused }).catch(showError) });
  const edit = h('button', { onclick: () => openTableDialog(c.t) }, '编辑');
  const del = h('button', { class: 'ghost' }, '删除');
  del.onclick = () => confirmStep(del, '确认删除', () => api('DELETE', `/api/tables/${c.t.id}`).catch(showError));
  c.err = h('div', { class: 'error-line', hidden: true });
  c.rows = t.rows.map((r) => {
    const cells = {
      regs: h('td', { class: 'regs' }),
      val: h('td', { class: 'val' }),
      hint: h('td', { class: 'hint' }),
    };
    cells.tr = h('tr', { class: r.writable ? 'writable' : null, title: r.writable ? '点击写入' : null },
      h('td', { class: 'ref' }, r.ref),
      named ? h('td', {}, r.name || '') : null,
      cells.regs, cells.val,
      units ? h('td', { class: 'unit' }, r.unit || '') : null,
      cells.hint);
    if (r.writable) cells.tr.onclick = () => openWrite(c.t, c.t.rows[c.rows.indexOf(cells)]);
    return cells;
  });
  const head = h('tr', {}, h('th', {}, '地址'), named ? h('th', {}, '名称') : null, h('th', { class: 'regs' }, t.config.function <= 2 ? '状态' : '寄存器'),
    h('th', { class: 'val' }, '值'), units ? h('th', {}, '单位') : null, h('th', {}, ''));
  c.el = h('article', { class: 'card' },
    h('header', {}, c.name, c.stat, h('div', { class: 'actions' }, c.pause, edit, del), h('span', { class: 'sub' }, t.title)),
    c.err,
    t.rows.length ? h('div', { class: 'scroll' }, h('table', { class: 'grid' }, h('thead', {}, head), h('tbody', {}, c.rows.map((x) => x.tr)))) : null);
  return c;
}

function updateCard(c, t) {
  c.t = t;
  c.name.textContent = t.config.name || `读取表 ${t.id}`;
  c.stat.textContent = (STAT[t.status] || (() => t.status))(t);
  c.stat.className = 'stat ' + t.status;
  c.pause.textContent = t.paused ? '继续' : '暂停';
  c.err.hidden = !t.error;
  const errKey = (t.error || '') + '|' + (t.hint || ''); // 原因和建议跟桌面版同一套规则（服务端给）
  if (c.errKey !== errKey) {
    c.errKey = errKey;
    c.err.replaceChildren(t.error || '', ...(t.hint ? [h('div', { class: 'small hint-line' }, t.hint)] : []));
  }
  c.el.classList.toggle('stale', t.status !== 'ok');
  t.rows.forEach((r, i) => {
    const cells = c.rows[i];
    if (!cells) return;
    if (cells.val.textContent !== r.value) {
      const first = cells.val.textContent === '';
      cells.val.textContent = r.value;
      if (!first) { // 数值变化时闪一下
        cells.val.classList.remove('changed');
        void cells.val.offsetWidth;
        cells.val.classList.add('changed');
      }
    }
    if (cells.regs.textContent !== r.regs) cells.regs.textContent = r.regs;
    const hint = r.hint || '';
    if (cells.hint.textContent !== hint) cells.hint.textContent = hint;
  });
}

function showError(e) { setMsg(e.message, true); }

// ---- 新建 / 编辑读取表 ----

let editing = null;
const DEFAULT_ADDR = { 1: '0', 2: '10001', 3: '40001', 4: '30001' };

function openTableDialog(t) {
  editing = t || null;
  const f = $('table-form');
  const cfg = t ? t.config : prefs.get('tableDefaults', { slave: 1, function: 3, address: '40001', count: 10, type: 'UINT16', order: 'AB', scale: 1, intervalMs: 1000 });
  $('table-title').textContent = t ? `编辑读取表 ${t.id}` : '新建读取表';
  f.elements.name.value = t ? (cfg.name || '') : '';
  f.elements.slave.value = cfg.slave ?? 1;
  f.elements.function.value = String(cfg.function || 3);
  f.elements.address.value = cfg.address || DEFAULT_ADDR[cfg.function || 3];
  f.elements.count.value = cfg.count || 10;
  f.elements.scale.value = cfg.scale || 1;
  f.elements.intervalMs.value = cfg.intervalMs || 1000;
  setupTypes(f.elements.type, f.elements.order, cfg.type || 'UINT16', cfg.order || '');
  const points = !!(cfg.points && cfg.points.length);
  $('table-points').hidden = !points;
  for (const n of ['function', 'address', 'count', 'type', 'order', 'scale']) f.elements[n].disabled = points;
  $('table-error').textContent = '';
  updateTableFields();
  $('table-dialog').showModal();
}

function updateTableFields() {
  const f = $('table-form');
  const bits = +f.elements.function.value <= 2;
  f.querySelectorAll('.reg-only').forEach((el) => { el.hidden = bits; });
}

async function saveTable() {
  const f = $('table-form').elements;
  const cfg = {
    name: f.name.value.trim(),
    slave: +f.slave.value,
    function: +f.function.value,
    address: f.address.value.trim(),
    count: +f.count.value,
    type: f.type.value,
    order: f.order.value,
    scale: +f.scale.value || 1,
    intervalMs: +f.intervalMs.value,
  };
  if (editing && editing.config.points) cfg.points = editing.config.points;
  try {
    if (editing) await api('PUT', `/api/tables/${editing.id}`, cfg);
    else await api('POST', '/api/tables', cfg);
    if (!cfg.points) prefs.set('tableDefaults', cfg);
    $('table-dialog').close();
  } catch (e) {
    $('table-error').textContent = e.message;
  }
}

// ---- 写入 ----

function openWrite(t, row) {
  const f = $('write-form').elements;
  const coil = t ? t.config.function === 1 : f.target.value === 'coil';
  if (t) f.slave.value = t.config.slave;
  f.target.value = coil ? 'coil' : 'register';
  if (row) f.address.value = coil ? String(row.offset) : row.ref;
  setupTypes(f.type, f.order, row ? row.type : f.type.value || 'UINT16', row ? row.order : f.order.value);
  if (row) f.scale.value = row.scale || 1;
  f.value.value = row ? row.value : '';
  updateWriteFields();
  $('write-result').hidden = true;
  $('write-error').textContent = '';
  $('write-dialog').showModal();
  f.value.select();
}

function updateWriteFields() {
  const f = $('write-form').elements;
  const coil = f.target.value === 'coil';
  $('write-form').querySelectorAll('.reg-only').forEach((el) => { el.hidden = coil; });
  // 占多个寄存器的类型（32 / 64 位）只能用 16
  const opts = coil ? [['0', '自动（05）'], ['5', '05 写单个线圈'], ['15', '15 写多个线圈']]
    : WIDTH[f.type.value] ? [['0', '自动（16）'], ['16', '16 写多个寄存器']]
      : [['0', '自动（06）'], ['6', '06 写单个寄存器'], ['16', '16 写多个寄存器']];
  const keep = f.function.value;
  f.function.replaceChildren(...opts.map(([v, t]) => h('option', { value: v }, t)));
  f.function.value = opts.some(([v]) => v === keep) ? keep : '0';
}

async function doWrite() {
  const f = $('write-form').elements;
  const btn = $('write-go');
  const body = {
    slave: +f.slave.value,
    coil: f.target.value === 'coil',
    address: f.address.value.trim(),
    type: f.type.value,
    order: f.order.value,
    scale: +f.scale.value || 1,
    value: f.value.value.trim(),
    function: +f.function.value,
  };
  $('write-error').textContent = '';
  btn.disabled = true;
  btn.textContent = '写入并回读中…';
  try {
    showWriteResult(await api('POST', '/api/write', body));
  } catch (e) {
    $('write-error').textContent = e.message;
    $('write-result').hidden = true;
  } finally {
    btn.disabled = false;
    btn.textContent = '写入';
  }
}

function showWriteResult(r) {
  const box = $('write-result');
  box.hidden = false;
  box.replaceChildren(...[ // 没有的部分是 null，要先去掉，否则会显示成文字“null”
    h('div', { class: 'verdict ' + (r.result === 'PASS' ? 'pass' : 'fail') }, r.text || r.result),
    h('div', {}, `原值 ${r.original} → 目标 ${r.target}`, r.rounded ? '（按倍率取整后写入）' : '',
      r.written ? h('span', { class: 'mono' }, `，写入寄存器 ${r.written}`) : ''),
    r.writeError ? h('div', { class: 'error' }, '写入出错：' + r.writeError) : null,
    r.readbacks.length ? h('div', {}, '回读：' + r.readbacks.map((b) => `${b.atMs} ms ${b.error ? '失败（' + b.error + '）' : b.value}`).join(' · ')) : null,
    r.hint ? h('div', { class: 'muted' }, '提示：' + r.hint) : null,
  ].filter(Boolean));
}

// ---- 识别协议 ----

let detected = null;

async function doDetect() {
  const target = $('c-target').value.trim();
  if (!target || $('c-sim').checked) { setMsg('识别协议需要填写设备地址（IP:端口）', true); return; }
  detected = null;
  $('detect-use').hidden = true;
  const box = $('detect-result');
  box.replaceChildren(h('div', {}, `正在识别 ${target}：依次尝试 Modbus TCP、RTU over TCP、ASCII over TCP，Slave ID 先试第一个读取表的，再试 1 和 255…`));
  $('detect-dialog').showModal();
  try {
    const r = await api('POST', '/api/detect', { target, timeoutMs: +$('c-timeout').value });
    box.replaceChildren(
      h('div', { class: 'verdict ' + (r.ok ? 'pass' : 'fail') },
        r.ok ? `识别结果：${r.name}，Slave ${r.slave}${r.byException ? '（靠异常响应确认，协议同样是对的）' : ''}` : `没有识别出协议：${r.error}`),
      ...r.attempts.map((a) => h('div', { class: 'mono' }, `${a.mode.padEnd(15)} Slave ${String(a.slave).padEnd(4)}${a.result}`)));
    if (r.ok) { detected = r; $('detect-use').hidden = false; }
  } catch (e) {
    box.replaceChildren(h('div', { class: 'error' }, e.message));
  }
}

// ---- 报文 ----

const MAX_PACKETS = 2000; // 页面里保留的报文
const MAX_ROWS = 1000;    // 同时显示的行数
let packets = [];
let lastSeq = 0; // 已收到的最后一条报文序号，补发和推送重复时按它去掉

const isBad = (p) => p.status !== 'SENT' && p.status !== 'SUCCESS';
const squash = (s) => s.replace(/\s+/g, '').toUpperCase();

function packetMatches(p) {
  const f = $('p-filter').value;
  if (f === 'error' && !isBad(p)) return false;
  if ((f === 'TX' || f === 'RX') && p.dir !== f) return false;
  const q = squash($('p-search').value);
  return !q || squash(p.data || '').includes(q);
}

// 报文状态的说法和桌面版一样；晚到响应后面写明回的是哪条请求、晚了多久（服务端给的 error）。
const STATUS = {
  TIMEOUT: '超时', EXCEPTION: '异常', NONSTANDARD: '非标准响应', CRC_ERROR: 'CRC 错误，已丢弃', LATE_RESPONSE: '晚到响应，已丢弃',
  UNEXPECTED: '不匹配，已丢弃', CANCELLED: '已取消', CONNECTION_ERROR: '连接错误', PARSE_ERROR: '格式错误，已丢弃',
};

function packetRow(p) {
  const tx = p.dir === 'TX';
  const label = p.foreign ? '非本连接的响应，已丢弃' : STATUS[p.status] || p.status;
  return h('div', { class: 'pk' + (isBad(p) ? ' bad' : '') },
    h('span', { class: 't' }, p.time + '  '),
    h('span', { class: tx ? 'tx' : 'rx' }, `${tx ? 'Tx' : 'Rx'}:${String(p.req).padStart(6, '0')}`),
    '  ' + (p.data || '（无数据）'),
    isBad(p) ? `  ← ${label}${p.error ? '（' + p.error + '）' : ''}` : '',
    !tx && p.rtt ? `  ${p.rtt.toFixed(1)} ms` : '');
}

// packetStats 在报文工具栏里数晚到响应和别的主站的响应：后者多说明串口服务器 / DTU 在几个主站之间转发。
function packetStats() {
  let late = 0;
  let foreign = 0;
  for (const p of packets) {
    if (p.foreign) foreign++;
    else if (p.status === 'LATE_RESPONSE') late++;
  }
  $('p-stats').textContent = [late && `晚到 ${late}`, foreign && `别的主站的响应 ${foreign}`].filter(Boolean).join(' · ');
}

function atBottom(box) { return box.scrollHeight - box.scrollTop - box.clientHeight < 40; }

function addPackets(list) {
  list = list.filter((p) => p.seq > lastSeq);
  if (!list.length) return;
  lastSeq = list[list.length - 1].seq;
  packets.push(...list);
  if (packets.length > MAX_PACKETS) packets.splice(0, packets.length - MAX_PACKETS);
  const box = $('packets');
  const follow = !$('p-hold').checked && atBottom(box);
  const rows = list.filter(packetMatches).map(packetRow);
  box.append(...rows);
  while (box.childElementCount > MAX_ROWS) box.firstElementChild.remove();
  if (follow) box.scrollTop = box.scrollHeight;
  $('packet-count').textContent = packets.length ? `(${packets.length})` : '';
  packetStats();
}

function renderPackets() {
  const box = $('packets');
  box.replaceChildren(...packets.filter(packetMatches).slice(-MAX_ROWS).map(packetRow));
  box.scrollTop = box.scrollHeight;
  $('packet-count').textContent = packets.length ? `(${packets.length})` : '';
  packetStats();
}

// ---- 导入点表、改站号 ----

// importPoints 读出文件发给服务端：按地址把相近的点合成读取表，替换现有的读取表。
async function importPoints(file) {
  if (file.size > 16 * 1024 * 1024) { setMsg('点表文件超过 16 MB，请拆分后导入', true); return; }
  setMsg(`正在导入点表 ${file.name}…`);
  try {
    const url = await new Promise((ok, fail) => {
      const r = new FileReader();
      r.onload = () => ok(r.result);
      r.onerror = () => fail(r.error);
      r.readAsDataURL(file);
    });
    const r = await api('POST', '/api/points', { name: file.name, data: url.slice(url.indexOf(',') + 1) });
    setMsg('');
    const lines = [`从${r.format}导入了 ${r.points} 个点，建了 ${r.tables} 个读取表，读 Slave ${r.slave}。`];
    if (r.left) lines.push(`还有 ${r.left} 段超出读取表上限，没有建表；把点表拆开分几次导入。`);
    lines.push(`设备站号不是 ${r.slave} 时，点“改站号”一次改好全部读取表。`);
    if (r.noOrder) lines.push('文件里没有字节序，32 / 64 位点先按 ABCD 解码；数值不对时读取表会提示改用哪种。');
    if (r.skipped && r.skipped.length) {
      lines.push(`跳过 ${r.skipped.length} 行：`, ...r.skipped.slice(0, 12));
      if (r.skipped.length > 12) lines.push('……');
    }
    if (r.notes) lines.push(...r.notes);
    $('import-result').replaceChildren(...lines.map((l) => h('div', {}, l)));
    $('import-dialog').showModal();
  } catch (e) {
    setMsg('导入点表失败：' + e.message, true);
  }
}

// openSlaveDialog 改站号：读取表用了几个站号时先选改哪一组，默认第一组；也可以改全部读取表。
function openSlaveDialog() {
  const tables = state ? state.tables : [];
  if (!tables.length) return;
  const count = new Map();
  for (const t of tables) count.set(t.config.slave, (count.get(t.config.slave) || 0) + 1);
  const f = $('slave-form');
  const opts = [...count].sort((a, b) => a[0] - b[0]).map(([id, n]) => h('option', { value: id }, `Slave ${id} 的读取表（${n} 个）`));
  if (count.size > 1) opts.push(h('option', { value: 0 }, `全部读取表（${tables.length} 个）`));
  f.elements.from.replaceChildren(...opts);
  f.elements.from.value = tables[0].config.slave;
  f.elements.to.value = tables[0].config.slave;
  $('slave-error').textContent = '';
  $('slave-dialog').showModal();
  f.elements.to.select();
}

async function applySlave() {
  const f = $('slave-form');
  if (!f.reportValidity()) return;
  try {
    await api('POST', '/api/slave', { from: +f.elements.from.value, to: +f.elements.to.value });
    $('slave-dialog').close();
  } catch (e) {
    $('slave-error').textContent = e.message;
  }
}

// ---- 检测寄存器 ----
// 和桌面版、modbus-cli 同一套算法，在服务端后台跑；进度跟着状态推送，结束后取完整结果。

const PROBE_EXPLAIN = {
  auto: '先批量读，读通的段不再逐个读；读不通的段逐个定位到单个地址。最快，适合先摸清一段地址。',
  single: '每个地址单独读一次，找出全部读不通的地址。请求数等于数量，范围大时要等一会儿。',
  quick: '批量读整个范围，从返回全 0 的段里抽查单独读，看设备是不是给没数据的地址补 0，最多 16 次验证请求；读取表里有点表时按点读。',
  points: '按读取表里的点表：地址相连的点一段读一次，读不通的段逐点再读，找出无效的点。',
  each: '按读取表里的点表：每段整段读一次，再每个点单独读一次，找出整段有返回、单独却读不通的点。',
};
const PROBE_MAX = { 1: 2000, 2: 2000, 3: 125, 4: 125 };
let probeWatch = 0;   // 对话框里正在看的检测
let probeShown = 0;   // 已经显示过结果的检测
let probeSeen = -1;   // 进度按钮已提示过的检测；-1 表示还没收到第一份状态
let probeResult = null;

function probeView(view) { // setup 填参数、run 进度、result 结果
  $('probe-setup').hidden = view !== 'setup';
  $('probe-run').hidden = view !== 'run';
  $('probe-result').hidden = view !== 'result';
  $('probe-start').hidden = view !== 'setup';
  $('probe-stop').hidden = view !== 'run';
  $('probe-stop').disabled = false;
  $('probe-stop').textContent = '停止';
  $('probe-again').hidden = view !== 'result';
  $('probe-report').hidden = view !== 'result';
  $('probe-use').hidden = view !== 'result' || !(probeResult && probeResult.readable);
  $('probe-close').textContent = view === 'run' ? '后台检测' : '关闭';
  $('probe-close').title = view === 'run' ? '关掉对话框，检测在服务端继续；读取区上方的按钮可以回来看进度' : '';
}

function openProbe() {
  const p = state && state.probe;
  $('probe-error').textContent = '';
  if (p && (p.running || p.id > probeSeen)) { showProbe(p); return; }
  setupProbe();
  $('probe-dialog').showModal();
}

// showProbe 打开对话框看某次检测：在跑就看进度，结束了就看结果。
function showProbe(p) {
  probeWatch = p.id;
  if (p.running) {
    probeView('run');
    renderProbeRun(p);
  } else {
    probeShown = 0;
    loadProbeResult();
  }
  if (!$('probe-dialog').open) $('probe-dialog').showModal();
}

function setupProbe() {
  const f = $('probe-form').elements;
  const tables = state ? state.tables : [];
  const pointTables = tables.some((t) => t.config.points && t.config.points.length);
  const t = tables.find((x) => !(x.config.points && x.config.points.length)) || tables[0];
  for (const o of f.how.options) if (o.value === 'points' || o.value === 'each') o.disabled = !pointTables;
  const how = prefs.get('probeHow', '');
  const allowed = [...f.how.options].some((o) => o.value === how && !o.disabled);
  f.how.value = allowed ? how : pointTables ? 'points' : 'auto';
  if (t) {
    f.slave.value = t.config.slave || 1;
    f.function.value = String(t.config.function);
    f.address.value = (t.rows[0] && t.rows[0].ref) || t.config.address; // 点表读取表的起始地址存的是 0x0000，显示成 40001
    f.count.value = t.config.count;
  }
  $('probe-title').textContent = '检测寄存器';
  probeResult = null;
  probeView('setup');
  updateProbeFields();
}

function updateProbeFields() {
  const f = $('probe-form').elements;
  const byPoints = f.how.value === 'points' || f.how.value === 'each';
  $('probe-form').querySelectorAll('.range-only').forEach((el) => { el.hidden = byPoints; });
  $('probe-explain').textContent = PROBE_EXPLAIN[f.how.value] + ' 检测期间读取表暂停，网络设备让出常规连接，检测完马上恢复。';
}

async function startProbe() {
  const f = $('probe-form').elements;
  const body = { how: f.how.value, slave: +f.slave.value, function: +f.function.value, address: f.address.value.trim(), count: +f.count.value };
  $('probe-error').textContent = '';
  try {
    const b = await api('POST', '/api/probe', body);
    prefs.set('probeHow', body.how);
    probeWatch = b.id;
    probeResult = null;
    probeView('run');
    renderProbeRun(b);
  } catch (e) {
    $('probe-error').textContent = e.message;
  }
}

// renderProbe 跟着状态推送更新：读取区上方的进度按钮，以及对话框里正在看的检测。
function renderProbe(p) {
  if (probeSeen < 0) probeSeen = p && !p.running ? p.id : 0; // 打开页面前做完的检测不再提示
  const btn = $('probe-status');
  const fresh = p && !p.running && p.id > probeSeen;
  btn.hidden = !(p && (p.running || fresh));
  if (p && p.running) btn.textContent = `检测中 ${p.total ? Math.floor((p.done * 100) / p.total) : 0}% · 查看`;
  else if (fresh) btn.textContent = '检测完成 · 查看结果';
  if (!p || p.id !== probeWatch || !$('probe-dialog').open) return;
  if (p.running) renderProbeRun(p);
  else if (probeShown !== p.id) loadProbeResult();
}

function renderProbeRun(p) {
  $('probe-title').textContent = '检测寄存器 · ' + p.title;
  $('probe-bar').max = p.total || 1;
  $('probe-bar').value = p.done;
  $('probe-progress').textContent = p.progress;
  $('probe-note').textContent = p.note || '';
}

async function loadProbeResult() {
  try {
    const v = await api('GET', '/api/probe');
    if (v.running) return; // 还在跑，等推送
    probeShown = probeWatch = v.id;
    probeSeen = Math.max(probeSeen, v.id);
    probeResult = v;
    $('probe-title').textContent = '检测结果 · ' + v.title;
    const verdict = $('probe-verdict');
    verdict.textContent = (v.error ? v.error + ' · ' : '') + v.summary;
    verdict.className = 'verdict ' + (v.error || v.problems ? 'fail' : 'pass');
    $('probe-text').textContent = `${v.text}\n\n${v.progress} · ${v.requests} 次请求`;
    const rows = v.rows.slice(0, 300);
    $('probe-fails').replaceChildren(
      ...(rows.length ? [h('table', {}, h('thead', {}, h('tr', {}, v.columns.map((c) => h('th', {}, c)))),
        h('tbody', {}, rows.map((r) => h('tr', {}, r.map((x) => h('td', {}, x))))))] : []),
      ...(v.rows.length > rows.length ? [h('div', { class: 'muted' }, `只列出前 ${rows.length} 行，共 ${v.rows.length} 行；完整清单在检测报告里`)] : []));
    $('probe-use').textContent = v.readable ? `按可读范围新建读取表（${v.readable.address} 起 ${Math.min(v.readable.count, PROBE_MAX[v.function])} 个）` : '';
    probeView('result');
    if (state) renderProbe(state.probe);
  } catch (e) {
    $('probe-error').textContent = e.message;
  }
}

// downloadProbeReport 下载检测报告（Markdown、JSONL 明细、CSV 失败清单打成 zip），出错时在对话框里说明。
async function downloadProbeReport() {
  const note = $('probe-report-note').value.trim();
  $('probe-error').textContent = '';
  try {
    const r = await fetch('/api/probe/report?note=' + encodeURIComponent(note));
    if (!r.ok) {
      let msg = `HTTP ${r.status}`;
      try { msg = (await r.json()).error || msg; } catch { /* 不是 JSON */ }
      throw new Error(msg);
    }
    const blob = await r.blob();
    const cd = r.headers.get('Content-Disposition') || '';
    const star = cd.match(/filename\*=utf-8''([^;]+)/i);
    const plain = cd.match(/filename="([^"]+)"/);
    const name = star ? decodeURIComponent(star[1]) : plain ? plain[1] : '检测报告.zip';
    const a = h('a', { href: URL.createObjectURL(blob), download: name });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 10000);
  } catch (e) {
    $('probe-error').textContent = '下载检测报告失败：' + e.message;
  }
}

// useReadable 按检测出的最长可读范围新建读取表（一次读不完时取前面一次能读的数量）。
function useReadable() {
  const v = probeResult;
  if (!v || !v.readable) return;
  $('probe-dialog').close();
  openTableDialog(null);
  const f = $('table-form').elements;
  f.slave.value = v.slave;
  f.function.value = String(v.function);
  f.address.value = v.readable.address;
  f.count.value = Math.min(v.readable.count, PROBE_MAX[v.function]);
  updateTableFields();
}

// ---- 日志、记录文件 ----

const KIND = { CONNECT: '连接', CONNECT_FAIL: '连接失败', DISCONNECT: '断开', RECONNECT: '重连', READ_FAIL: '读取失败', READ_OK: '读取恢复', WRITE: '写入', IMPORT: '导入点表', SLAVE: '改站号', PROBE: '检测寄存器' };
let logKey = '';

function renderLogs(logs) {
  const key = logs.length + '|' + (logs.length ? logs[logs.length - 1].time + logs[logs.length - 1].detail : '');
  if (key === logKey) return;
  logKey = key;
  $('logs').replaceChildren(...logs.slice().reverse().map((l) => h('div', { class: 'lg' + (/FAIL|DISCONNECT/.test(l.kind) ? ' bad' : '') },
    h('span', { class: 'k' }, `${l.time} ${KIND[l.kind] || l.kind}`), l.detail)));
  $('log-count').textContent = logs.length ? `(${logs.length})` : '';
}

function size(n) {
  return n >= 1e6 ? (n / 1e6).toFixed(1) + ' MB' : Math.max(1, Math.round(n / 1e3)) + ' KB';
}

async function loadRecords() {
  const box = $('records');
  try {
    const r = await api('GET', '/api/records');
    $('r-dir').textContent = r.enabled
      ? `保存在 ${r.dir}：每天一个文件，超过 50 MB 换下一个编号。表结构与桌面版相同，可在桌面版“历史报文”里点“打开文件…”查看（1.2.1 起），也可用 SQLite 工具打开。`
      : '没有开启报文记录。';
    box.replaceChildren(r.files.length
      ? h('table', {}, h('tr', {}, h('th', {}, '文件'), h('th', {}, '大小'), h('th', {}, '修改时间'), h('th', {}, '')),
        r.files.map((f) => h('tr', {},
          h('td', {}, f.name, f.current ? h('span', { class: 'muted' }, '（正在写入）') : ''),
          h('td', {}, size(f.size)), h('td', {}, f.modified),
          h('td', {}, h('a', { href: '/api/records/' + encodeURIComponent(f.name), download: f.name }, '下载')))))
      : h('p', { class: 'muted' }, '还没有记录文件。'));
  } catch (e) {
    box.replaceChildren(h('p', { class: 'error' }, e.message));
  }
}

// ---- 下方面板：标签页、高度、收起 ----

function showTab(name) {
  document.querySelectorAll('.tabs [data-tab]').forEach((b) => b.classList.toggle('active', b.dataset.tab === name));
  for (const t of ['packets', 'logs', 'records']) $('tab-' + t).hidden = t !== name;
  if (name === 'records' && !$('app').hidden) loadRecords(); // 登录前不取，否则 401 会让登录页显示“登录已过期”
  prefs.set('tab', name);
}

function setupDock() {
  const dock = $('dock');
  const height = prefs.get('dockHeight', 0);
  if (height) dock.style.height = height + 'px';
  if (prefs.get('dockCollapsed', false)) { dock.classList.add('collapsed'); $('dock-toggle').textContent = '展开'; }
  $('dock-toggle').onclick = () => {
    const collapsed = dock.classList.toggle('collapsed');
    $('dock-toggle').textContent = collapsed ? '展开' : '收起';
    prefs.set('dockCollapsed', collapsed);
  };
  $('grip').addEventListener('pointerdown', (e) => {
    if (dock.classList.contains('collapsed')) return;
    e.preventDefault();
    const move = (ev) => {
      const hgt = Math.min(Math.max(window.innerHeight - ev.clientY, 90), window.innerHeight - 160);
      dock.style.height = hgt + 'px';
    };
    const up = () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', up);
      prefs.set('dockHeight', parseInt(dock.style.height, 10) || 0);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  });
  document.querySelectorAll('.tabs [data-tab]').forEach((b) => { b.onclick = () => showTab(b.dataset.tab); });
  showTab(prefs.get('tab', 'packets'));
}

// ---- 启动 ----

function bind() {
  $('login-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    $('login-error').textContent = '';
    try {
      await api('POST', '/api/login', { password: $('password').value });
      enterApp();
    } catch (err) {
      $('login-error').textContent = err.message;
    }
  });
  $('logout').onclick = async () => {
    try { await api('POST', '/api/logout'); } catch { /* 已经退出 */ }
    showLogin('');
  };
  $('c-mode').onchange = () => { updateModeFields(); if (isSerial()) refreshPorts(); };
  $('c-sim').onchange = updateModeFields;
  $('c-refresh').onclick = refreshPorts;
  $('connect').onclick = doConnect;
  $('disconnect').onclick = () => api('POST', '/api/disconnect').then(() => setMsg('')).catch(showError);
  $('detect').onclick = doDetect;
  $('demo').onclick = () => {
    const run = async () => {
      setMsg('正在启动内置模拟器…');
      try { await api('POST', '/api/demo'); setMsg(''); } catch (e) { setMsg(e.message, true); }
    };
    if (state && state.tables.length) confirmStep($('demo'), '确认替换读取表', run);
    else run();
  };
  $('pause-all').onclick = () => { // 有在读的表就全部暂停，否则全部继续
    const running = state && state.tables.some((t) => !t.paused);
    api('POST', '/api/pause-all', { paused: running }).catch(showError);
  };
  const toggleConn = () => {
    connFold = !$('app').classList.contains('conn-folded');
    $('app').classList.toggle('conn-folded', connFold);
  };
  $('conn-chip').onclick = toggleConn;
  $('conn-chip').onkeydown = (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleConn(); } };
  $('add-table').onclick = () => openTableDialog(null);
  $('import-points').onclick = () => {
    if (state && state.tables.length) confirmStep($('import-points'), '再点一次：替换现有读取表', () => $('import-file').click());
    else $('import-file').click();
  };
  $('import-file').onchange = () => {
    const file = $('import-file').files[0];
    $('import-file').value = ''; // 同一个文件改过后可以再导入
    if (file) importPoints(file);
  };
  $('import-slave').onclick = (e) => { e.preventDefault(); $('import-dialog').close(); openSlaveDialog(); };
  $('set-slave').onclick = openSlaveDialog;
  $('probe').onclick = openProbe;
  $('probe-status').onclick = () => { if (state && state.probe) showProbe(state.probe); };
  const pf = $('probe-form');
  pf.elements.how.onchange = updateProbeFields;
  pf.elements.function.onchange = () => {
    const a = pf.elements.address;
    if (Object.values(DEFAULT_ADDR).includes(a.value.trim())) a.value = DEFAULT_ADDR[pf.elements.function.value];
  };
  pf.addEventListener('submit', (e) => {
    const v = e.submitter && e.submitter.value;
    if (!v || v === 'close') return;
    e.preventDefault();
    if (v === 'start') startProbe();
    else if (v === 'stop') {
      $('probe-stop').disabled = true;
      $('probe-stop').textContent = '正在停止…';
      api('POST', '/api/probe/stop').catch((err) => { $('probe-error').textContent = err.message; });
    } else if (v === 'report') downloadProbeReport();
    else if (v === 'use') useReadable();
    else if (v === 'again') {
      $('probe-error').textContent = '';
      probeResult = null;
      $('probe-title').textContent = '检测寄存器';
      probeView('setup');
      updateProbeFields();
    }
  });
  const sf = $('slave-form');
  sf.elements.from.onchange = () => { if (+sf.elements.from.value) sf.elements.to.value = sf.elements.from.value; };
  sf.addEventListener('submit', (e) => {
    if (e.submitter && e.submitter.value === 'save') { e.preventDefault(); applySlave(); }
  });

  const tf = $('table-form');
  tf.elements.function.onchange = () => {
    const a = tf.elements.address;
    if (Object.values(DEFAULT_ADDR).includes(a.value.trim())) a.value = DEFAULT_ADDR[tf.elements.function.value];
    updateTableFields();
  };
  tf.addEventListener('submit', (e) => {
    if (e.submitter && e.submitter.value === 'save') { e.preventDefault(); saveTable(); }
  });

  const wf = $('write-form');
  wf.elements.target.onchange = updateWriteFields;
  wf.elements.type.addEventListener('change', updateWriteFields); // onchange 留给 setupTypes 换字节序
  wf.addEventListener('submit', (e) => {
    if (e.submitter && e.submitter.value === 'write') { e.preventDefault(); doWrite(); }
  });
  $('detect-use').onclick = () => {
    if (detected) { $('c-mode').value = detected.mode; updateModeFields(); setMsg(`已选择 ${detected.name}，点“连接”开始`); }
  };

  $('p-filter').onchange = renderPackets;
  $('p-search').oninput = renderPackets;
  $('p-clear').onclick = () => { packets = []; renderPackets(); };
  $('r-refresh').onclick = loadRecords;
  setupDock();
}

async function start() {
  bind();
  let s = {};
  try { s = await (await fetch('/api/session')).json(); } catch { /* 显示登录页 */ }
  if (s.loggedIn) enterApp();
  else showLogin('');
}

start();
