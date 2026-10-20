// Стенд v0.37: протяжка связи как в канвасе Obsidian.
// Исполняет НАСТОЯЩИЕ startLinkDrag/dropLink/applyLink из редактора против
// замоканного DOM. Мок DOM — с rect'ами и elementFromPoint, иначе протяжку
// нечем проверять: dropLink определяет цель именно по координатам курсора.
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO = process.argv[2] || process.cwd();
const SRC = path.join(REPO, 'web/static/editor/app.js');

// ── геометрия мока ────────────────────────────────────────────────────────
// canvas без прокрутки и смещения: клиентские координаты == координаты холста.
const RECT = { left: 0, top: 0, right: 2600, bottom: 1700, width: 2600, height: 1700 };
let uid = 0;
const POS = { a: [40, 80], c: [40, 380], b: [460, 200], x: [40, 80], y: [460, 200] };

class El {
  constructor(tag) {
    this.tagName = (tag || 'div').toUpperCase();
    this.children = [];
    this.parent = null;
    this._h = {};
    this._rect = { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 };
    this._id = ++uid;
    this.dataset = {};
    this.style = {};
    this._s = new Set();
    this.classList = {
      _s: this._s,
      add(...c) { c.forEach(x => this._s.add(x)); },
      remove(...c) { c.forEach(x => this._s.delete(x)); },
      toggle(c, on) { on === undefined ? (this._s.has(c) ? this._s.delete(c) : this._s.add(c)) : (on ? this._s.add(c) : this._s.delete(c)); },
      contains(c) { return this._s.has(c); },
    };
  }
  get className() { return [...this._s].join(' '); }
  set className(v) {
    this._s = new Set(String(v).split(/\s+/).filter(Boolean));
    if (this.classList) this.classList._s = this._s;
  }
  get textContent() { return this._t || ''; }
  set textContent(v) { this._t = String(v); this.children = []; }
  set innerHTML(v) { this._html = String(v); this.children = []; this._parsed = null; }
  get innerHTML() { return this._html || ''; }
  appendChild(c) { c.parent = this; this.children.push(c); this._html = null; return c; }
  // По-настоящему отцепляет: иначе пересозданные узлы копятся, и любой
  // поиск по data-id находит устаревший (так сломался первый стенд).
  remove() {
    if (this.parent) this.parent.children = this.parent.children.filter(x => x !== this);
    this.parent = null;
  }
  setAttribute(k, v) { this[k] = v; }
  getAttribute(k) { return this[k]; }
  removeAttribute(k) { delete this[k]; }
  // Настоящий поиск по поддереву: редактор дёргает node.querySelector(...) за
  // чипом выхода, и заглушка молча ломала бы протяжку.
  querySelectorAll(sel) {
    const out = [];
    const kids = el => (el._kids ? el._kids() : el.children);
    const walk = el => {
      for (const c of kids(el)) {
        if (matchSel(c, sel)) out.push(c);
        walk(c);
      }
    };
    walk(this);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  addEventListener(t, f) { (this._h[t] = this._h[t] || []).push(f); }
  removeEventListener(t, f) { if (this._h[t]) this._h[t] = this._h[t].filter(x => x !== f); }
  getBoundingClientRect() { return this._rect; }
  closest(sel) { return walkUp(this, el => matchSel(el, sel)); }
  fire(type, ev = {}) {
    const base = { type, stopPropagation() {}, preventDefault() {}, target: this, ...ev };
    for (const f of this._h[type] || []) f(base);
  }
  // ── разбор собственного innerHTML с вложенностью. Регуляркой по «листьям»
  // обойтись нельзя: <div class="inrow"><span>..</span><span>..</span></div>
  // терял сам div, и полей входа не существовало бы вовсе.
  _kids() {
    if (this.children.length) return this.children;
    if (this._parsed) return this._parsed;
    const root = this;
    const stack = [root];
    const out = [];
    const push = el => { out.push(el); return el; };
    const re = /<(\/?)([\w-]+)((?:[^>"']|"[^"]*"|'[^']*')*?)(\/?)>|([^<]+)/g;
    let m;
    while ((m = re.exec(this._html || ''))) {
      if (m[5] !== undefined && m[5].trim()) continue; // текст нам не нужен
      const closing = m[1] === '/';
      const tag = m[2];
      if (closing) { if (stack.length > 1) stack.pop(); continue; }
      const el = new El(tag);
      el.parent = stack[stack.length - 1];
      applyAttrs(el, m[3] || '');
      push(el);
      if (!m[4]) stack.push(el);
    }
    // Прямоугольники. Редактор их не задаёт, а dropLink целится по координатам,
    // поэтому верстку приходится смитировать. Считаем ОТНОСИТЕЛЬНО верха узла,
    // а потом сдвигаем на node.style.left/top — их renderNodes ставит до
    // innerHTML, так что позиция шага доступна.
    // Позицию берём из явной карты, а не из style: порядок, в котором
    // renderNodes ставит style и innerHTML, — деталь реализации, на которой
    // стенд не должен стоять (и уже стоял).
    // Только для самих узлов холста. Потомков _kids() тоже дёргает обход
    // querySelectorAll, и без этой проверки он затирал бы их _rect
    // прямоугольником узла — а потомки узла не имеют data-id.
    const pos = POS[this.dataset.id] || null;
    const px = pos ? pos[0] : 0, py = pos ? pos[1] : 0;
    let row = 0, chip = 0;
    for (const el of out) {
      const c = el.className;
      let b;
      if (c.includes('nh')) b = box(0, 0, 300, 24);
      else if (c.includes('inrow')) { b = box(2, 50 + row * 22, 296, 22); row++; }
      else if (c.includes('outchip')) { b = box(2, 130 + chip * 20, 80, 18); chip++; }
      else if (c.includes('src')) b = box(180, 50 + (row ? row - 1 : 0) * 22, 116, 20);
      else if (c.includes('nid') || c.includes('nplug')) b = box(6, 3, 120, 18);
      else b = box(2, 130 + chip * 20, 296, 20);
      el._rect = box(b.left + px, b.top + py, b.width, b.height);
    }
    if (pos) this._rect = box(px, py, 300, 210);
    this._parsed = out;
    return out;
  }
}
function box(left, top, width, height) {
  return { left, top, right: left + width, bottom: top + height, width, height };
}
function applyAttrs(el, s) {
  const cls = /class="([^"]*)"/.exec(s);
  if (cls) el.className = cls[1];
  const ds = /data-([\w-]+)="([^"]*)"/.exec(s);
  if (ds) el.dataset[ds[1]] = ds[2];
  el._rectFrom = s;
}
function walkUp(el, pred) { for (let e = el; e; e = e.parent) if (pred(e)) return e; return null; }
// matchSel с поддержкой потомков (пробел) и запятых: редактор ищет вида
// `.node[data-id="a"] .outchip[data-out="lines"]`, и без этого матчера стенд
// не видит собственных узлов.
function matchSimple(el, sel) {
  const idm = /^#([\w-]+)$/.exec(sel);
  if (idm) return el.id === idm[1];
  const m = /^([\w-]+)?((?:\.[\w-]+|\[[^\]]+\])*)$/.exec(sel);
  if (!m) return false;
  if (m[1] && el.tagName !== m[1].toUpperCase()) return false;
  for (const tk of (m[2].match(/\.[\w-]+|\[[^\]]+\]/g) || [])) {
    if (tk.startsWith('.')) { if (!el.classList.contains(tk.slice(1))) return false; }
    else {
      const a = /^\[([\w-]+)(?:="([^"]*)")?\]$/.exec(tk);
      if (!a) return false;
      // В селекторе `data-field`, в dataset — просто `field`.
      const key = a[1].replace(/^data-/, '').replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      if (el.dataset[key] === undefined) return false;
      if (a[2] !== undefined && el.dataset[key] !== a[2]) return false;
    }
  }
  return true;
}
function matchSel(el, sel) {
  for (const union of sel.split(',').map(x => x.trim()).filter(Boolean)) {
    const parts = union.split(/\s+/).filter(Boolean);
    if (!parts.length) continue;
    if (!matchSimple(el, parts[parts.length - 1])) continue;
    let node = el.parent, i = parts.length - 2, ok = true;
    while (i >= 0) {
      let found = false;
      while (node) { if (matchSimple(node, parts[i])) { found = true; node = node.parent; break; } node = node.parent; }
      if (!found) { ok = false; break; }
      i--;
    }
    if (ok) return true;
  }
  return false;
}

// ── DOM ───────────────────────────────────────────────────────────────────
const byId = {};
for (const id of ['canvas', 'edges',
  'props', 'palette', 'file-name', 'file-open', 'file-list', 'btn-save', 'btn-run',
  'btn-yaml', 'close-yaml', 'yaml-view', 'btn-undo', 'btn-redo', 'canvas-hint',
  'errs', 'toast', 'palette-hint', 'steps-count', 'p-file', 'secrets-box', 'network-box',
  'val-badge', 'yaml-text', 'p-steps', 'p-input', 'p-foreach', 'p-net', 'p-sec']) {
  const e = new El(id.startsWith('btn') || id.includes('file') || id === 'yaml-view' || id === 'close-yaml' ? 'button' : 'div');
  e.id = id;
  e._rect = id === 'canvas' ? RECT : { left: 0, top: 0, right: 800, bottom: 40, width: 800, height: 40 };
  byId[id] = e;
}
const doc = new El('body');
const docH = {};
doc.querySelector = s => {
  if (s === '#canvas') return byId.canvas;
  const m = /^#([\w-]+)$/.exec(s);
  return m ? byId[m[1]] || null : null;
};
doc.querySelectorAll = () => [];
doc.getElementById = id => byId[id] || null;
doc.createElement = t => new El(t);
doc.createElementNS = (ns, t) => new El(t);
doc.addEventListener = (t, f) => { (docH[t] = docH[t] || []).push(f); };
doc.removeEventListener = (t, f) => { if (docH[t]) docH[t] = docH[t].filter(x => x !== f); };
doc.elementFromPoint = (x, y) => hitTest(x, y);

function nodeById(id) { return byId.canvas.children.find(n => n.dataset.id === id) || null; }
byId.canvas.querySelectorAll = function (sel) {
  const out = [];
  for (const n of this.children) {
    if (matchSel(n, sel)) out.push(n);
    out.push(...n._kids().filter(c => matchSel(c, sel)));
  }
  return out;
};
byId.canvas.querySelector = function (sel) { return this.querySelectorAll(sel)[0] || null; };
doc.querySelectorAll = function (sel) {
  const m = /^#([\w-]+)$/.exec(sel);
  return m ? (byId[m[1]] ? [byId[m[1]]] : []) : byId.canvas.querySelectorAll(sel);
};
byId.edges.querySelector = function (sel) { return this.children.find(c => matchSel(c, sel)) || null; };
byId.edges.querySelectorAll = function (sel) { return this.children.filter(c => matchSel(c, sel)); };
doc.querySelector = function (sel) {
  const m = /^#([\w-]+)$/.exec(sel);
  if (m) return byId[m[1]] || null;
  return byId.canvas.querySelector(sel);
};

function hitTest(x, y) {
  let best = null;
  for (const n of byId.canvas.children) {
    for (const c of n._kids()) {
      const r = c._rect;
      if (x >= r.left && x <= r.right && y >= r.top && y <= r.bottom) { best = c; break; }
    }
  }
  if (best) return best;
  for (const n of byId.canvas.children) {
    const r = n._rect;
    if (x >= r.left && x <= r.right && y >= r.top && y <= r.bottom) return n;
  }
  return null;
}

// ── плагины ───────────────────────────────────────────────────────────────
const PLUGINS = [
  { id: 'community/one_out', dir: 'plugins\\community\\one_out', input: { cfg: {} }, output: { lines: {} } },
  { id: 'community/two_out', dir: 'plugins\\community\\two_out', input: {}, output: { words: {}, lines: {} } },
  { id: 'community/two_in', dir: 'plugins\\community\\two_in', input: { data: {}, prompt: {} }, output: { ok: {} } },
];

const listeners = { doc: {} };
const win = {
  fidespipeSession: async () => true,
  location: { href: 'http://x/editor/', search: '', origin: 'http://x' },
  addEventListener: (t, f) => { (listeners[t] = listeners[t] || []).push(f); },
  removeEventListener: (t, f) => { if (listeners[t]) listeners[t] = listeners[t].filter(x => x !== f); },
  documentListeners: docH,
  fetch: async () => ({ ok: true, status: 200, json: async () => ({}), text: async () => '' }),
  confirm: () => true,
  alert: () => {},
  console,
  setTimeout: (f) => { return 0; },
  clearTimeout: () => {},
};

// doc-слушатели (mousemove/mouseup во время протяжки) — общий реестр
const docList = {};

const ctx = vm.createContext({
  window: win, document: doc, console, setTimeout: () => 0, clearTimeout: () => {},
  fetch: win.fetch, location: win.location, confirm: win.confirm, alert: win.alert,
  fidespipeSession: win.fidespipeSession, Date, Math, JSON, Object, Array, String, Number, Boolean, RegExp, Error,
});

const src = fs.readFileSync(SRC, 'utf8');
vm.runInContext(src, ctx, { filename: 'app.js' });
const ed = vm.runInContext('({state, renderAll, setLayer, startLinkDrag, dropLink, applyLink, cancelLink, armLink, stepById, outFields, inFields, pushUndo})', ctx);

// Подменяем document-реестр на наш: startLinkDrag вешает mousemove/mouseup
// на document, а не на window.
const realAdd = doc.addEventListener;

// ── сценарий ──────────────────────────────────────────────────────────────
let pass = 0, fail = 0;
function ok(name, cond, extra) {
  if (cond) { pass++; console.log('  OK   ' + name); }
  else { fail++; console.log('  FAIL ' + name + (extra ? '  -> ' + extra : '')); }
}

async function main() {
  ed.state.plugins = PLUGINS;
  ed.state.doc = {
    file: 'demo.yaml', version: '0.34.0', input: [], secrets: [], network: null,
    steps: [
      { id: 'a', plugin: 'plugins/community/one_out', pos: [40, 80] },
      { id: 'c', plugin: 'plugins/community/two_out', pos: [40, 380] },
      { id: 'b', plugin: 'plugins/community/two_in', pos: [460, 200] },
    ],
  };
  // Слои: стенд проверяет связывание ШАГОВ, поэтому явно встаём на слой
  // пайплайна. На слое входов renderAll узлы не рисует вовсе — намеренно.
  ed.setLayer('pipeline');
  ed.renderAll();

  ok('три узла нарисовано', byId.canvas.children.length === 3, byId.canvas.children.length);

  // Точка внутри элемента: координаты берём из настоящих rect'ов, иначе тест
  // врал бы про геометрию, которой нет.
  const at = (el, dx = 0.5, dy = 0.5) => ({
    clientX: el._rect.left + el._rect.width * dx,
    clientY: el._rect.top + el._rect.height * dy,
  });
  const chipA = () => byId.canvas.querySelector('.node[data-id="a"] .outchip[data-out="lines"]');
  const nodeEl = id => byId.canvas.querySelector('.node[data-id="' + id + '"]');
  const rowOf = (id, field) => byId.canvas.querySelector('.node[data-id="' + id + '"] .inrow[data-field="' + field + '"]');
  const drop = pt => { for (const f of (docList.mouseup || [])) f({ ...pt, preventDefault() {}, stopPropagation() {} }); };
  const drag = (st, field, pt) => {
    ed.startLinkDrag(ed.stepById(st), field, { ...pt, preventDefault() {}, stopPropagation() {} });
  };

  // --- 1. протяжка с выхода на поле входа: 1 выход и 1 поле = прямая связь ---
  const rowData = byId.canvas.querySelector('.node[data-id="b"] .inrow[data-field="data"]');
  ok('поле data у b найдено', !!rowData);

  drag('a', 'lines', at(chipA()));
  ok('протяжка началась', !!(ed.state.linkDrag && ed.state.linkDrag.field === 'lines'), JSON.stringify(ed.state.linkDrag));
  for (const f of (docList.mousemove || [])) f({ ...at(nodeEl('b'), 0.5, 0.55), preventDefault() {}, stopPropagation() {} });
  const g = byId.edges.querySelector('#link-ghost');
  ok('резиновая линия создана', !!g);
  ok('у линии есть геометрия', !!g && String(g.d || '').startsWith('M'), g && g.d);
  const hoveredAny = byId.canvas.querySelectorAll('.hovered').length > 0
    || byId.canvas.querySelectorAll('.drop-target').length > 0;
  ok('цель подсвечена при протяжке', hoveredAny,
    'hovered=' + byId.canvas.querySelectorAll('.hovered').length
    + ' drop-target=' + byId.canvas.querySelectorAll('.drop-target').length);

  drop(at(rowData));
  ok('bind записан протяжкой', ed.stepById('b').bind && ed.stepById('b').bind.data === 'steps.a.lines', JSON.stringify(ed.stepById('b').bind));
  ok('состояние протяжки сброшено', ed.state.linkDrag === null);
  ok('резиновая линия убрана', !byId.edges.querySelector('#link-ghost'));
  ok('подсветка снята', byId.canvas.querySelectorAll('.hovered').length === 0
    && byId.canvas.querySelectorAll('.drop-target').length === 0);

  // --- 2. протяжка с узла (выходов два) на узел -> связь сразу, без выбора ---
  // Раньше здесь появлялись 4 пары-кандидата и панель сверху. Теперь дроп идёт
  // по наведению: первый выход источника, первое свободное поле приёмника.
  ed.stepById('b').bind = {};
  ed.renderAll();
  drag('c', null, at(nodeEl('c')));
  drop(at(nodeEl('b'), 0.5, 0.95)); // пустая зона ВНУТРИ узла b
  ok('дроп по узлу связал сразу, без списка пар', ed.stepById('b').bind.data === 'steps.c.words',
    JSON.stringify(ed.stepById('b').bind));
  ok('второе поле не тронуто', ed.stepById('b').bind.prompt === undefined,
    JSON.stringify(ed.stepById('b').bind));
  ok('кандидатов в состоянии нет', !ed.state.linkCandidates, String(ed.state.linkCandidates));
  ok('панели подтверждения в разметке нет', !byId['link-hint'], 'link-hint снова появился');

  // --- 3. свободное поле выбирается раньше занятого ---
  ed.stepById('b').bind = { data: 'steps.z.lines' };
  ed.renderAll();
  drag('c', null, at(nodeEl('c')));
  drop(at(nodeEl('b'), 0.5, 0.95));
  ok('занятое поле не переписано', ed.stepById('b').bind.data === 'steps.z.lines',
    JSON.stringify(ed.stepById('b').bind));
  ok('связь ушла в свободное поле', ed.stepById('b').bind.prompt === 'steps.c.words',
    JSON.stringify(ed.stepById('b').bind));

  // --- 4. протяжка в самого себя не связывает ---
  drag('a', 'lines', at(chipA()));
  drop(at(nodeEl('a')));
  ok('в себя не связалось', !Object.keys(ed.stepById('a').bind || {}).length, JSON.stringify(ed.stepById('a').bind));

  // --- 5. протяжка мимо цели не ломает двукличковый путь ---
  const bindBefore = JSON.stringify(ed.stepById('b').bind);
  drag('a', 'lines', at(chipA()));
  drop({ clientX: 2500, clientY: 1600 });
  ok('мимо цели — bind не тронут', JSON.stringify(ed.stepById('b').bind) === bindBefore);
  ok('после промаха linkDrag пуст', ed.state.linkDrag === null);
  chipA().fire('click', {});
  ok('щелчок по чипу вооружает (двукличковый путь жив)', !!ed.state.link, JSON.stringify(ed.state.link));
  ed.cancelLink();
  ok('отмена снимает вооружение', ed.state.link === null);

  // --- 6. узел -> узел: выход один, входов два -> тоже без вопросов ---
  // Раньше стенд требовал здесь двух пар и выбора. Теперь это ровно то, чего
  // просил пользователь: навёл на узел — соединил, в первое свободное поле.
  ed.stepById('b').bind = {};
  ed.renderAll();
  drag('a', null, at(nodeEl('a')));
  drop(at(nodeEl('b'), 0.5, 0.95));
  ok('1 выход × 2 входа связался без выбора', ed.stepById('b').bind.data === 'steps.a.lines',
    JSON.stringify(ed.stepById('b').bind));
  ok('выборов не спрашивали', ed.state.linkCandidates === undefined || ed.state.linkCandidates.length === 0);

  // --- 7. ровно один вход и один выход -> связь без вопросов ---
  const doc2 = ed.state.doc;
  ed.state.doc.steps = [{ id: 'x', plugin: 'plugins/community/one_out', pos: [40, 80] },
                        { id: 'y', plugin: 'plugins/community/one_in', pos: [460, 200] }];
  PLUGINS.push({ id: 'community/one_in', dir: 'plugins/community/one_in', input: { data: {} }, output: { ok: {} } });
  ed.state.plugins = PLUGINS;
  ed.renderAll();
  drag('x', null, at(nodeEl('x')));
  drop(at(nodeEl('y'), 0.5, 0.95));
  ok('1×1 связался без вопросов', ed.stepById('y').bind && ed.stepById('y').bind.data === 'steps.x.lines',
    JSON.stringify(ed.stepById('y').bind));
  ed.state.doc = doc2;
  ed.state.plugins = PLUGINS.filter(pp => pp.id !== 'community/one_in');

  // --- 8. типы и форматы: связь несовместимая не создаётся вовсе ---
  // Реальный случай с холста: у шага выходы boolean/array, а поле входа ждёт
  // string. Прежнее правило «первый выход» молча писало steps.src.boolean в
  // string-поле, и пайплайн становился неисполнимым — ядро отвергало его с
  // E_TYPE_MISMATCH, но холст показывал связь как готовую.
  PLUGINS.push({ id: 'community/typed_src', dir: 'plugins/community/typed_src',
    input: {}, output: { flag: { type: 'boolean' }, count: { type: 'number' } } });
  PLUGINS.push({ id: 'community/typed_dst', dir: 'plugins/community/typed_dst',
    input: { label: { type: 'string' }, addr: { type: 'string', format: 'ip' }, flag: { type: 'boolean' } },
    output: { ok: { type: 'boolean' } } });
  ed.state.plugins = PLUGINS;
  ed.state.doc.steps = [{ id: 'ts', plugin: 'plugins/community/typed_src', pos: [40, 80] },
                        { id: 'td', plugin: 'plugins/community/typed_dst', pos: [460, 200] }];
  ed.renderAll();

  // 8a. boolean -> boolean: связывается
  drag('ts', null, at(rowOf('td', 'flag'), 0.5, 0.5));
  drop(at(rowOf('td', 'flag'), 0.5, 0.5));
  ok('совместимые типы связались', ed.stepById('td').bind.flag === 'steps.ts.flag',
    JSON.stringify(ed.stepById('td').bind));

  // 8b. boolean -> string: отказ, bind не тронут
  const before = JSON.stringify(ed.stepById('td').bind);
  drag('ts', null, at(rowOf('td', 'label'), 0.5, 0.5));
  drop(at(rowOf('td', 'label'), 0.5, 0.5));
  ok('несовместимый тип не связан', JSON.stringify(ed.stepById('td').bind) === before,
    JSON.stringify(ed.stepById('td').bind));
  ok('после отказа протяжка сброшена', ed.state.linkDrag === null);

  // 8c. boolean -> string/ip: формат тоже не подходит
  drag('ts', null, at(rowOf('td', 'addr'), 0.5, 0.5));
  drop(at(rowOf('td', 'addr'), 0.5, 0.5));
  ok('несовместимый формат не связан', JSON.stringify(ed.stepById('td').bind) === before,
    JSON.stringify(ed.stepById('td').bind));

  // 8d. выход с чипа проверяется так же, как с узла
  drag('ts', 'count', at(rowOf('td', 'flag'), 0.5, 0.5));
  drop(at(rowOf('td', 'flag'), 0.5, 0.5));
  ok('явно взятый чип тоже сверяется по типу', JSON.stringify(ed.stepById('td').bind) === before,
    JSON.stringify(ed.stepById('td').bind));
  ed.state.doc = doc2;
  ed.state.plugins = PLUGINS.filter(pp => pp.id.indexOf('community/typed_') !== 0
    && pp.id !== 'community/one_in');


  console.log('\n' + (fail ? `провалено ${fail}, ` : '') + `пройдено ${pass}`);
  console.log(fail ? '\nСТЕНД НЕ ЗЕЛЕНЫЙ' : '\nвсе проверки пройдены');
  process.exit(fail ? 1 : 0);
}

// Перехватываем addEventListener на document для протяжки
const origAdd = doc.addEventListener.bind(doc);
doc.addEventListener = (t, f) => { (docList[t] = docList[t] || []).push(f); };

main().catch(e => { console.error('ИСКЛЮЧЕНИЕ:', e && e.stack || e); process.exit(1); });