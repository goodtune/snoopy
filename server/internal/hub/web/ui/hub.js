// Snoopy hub UI: one EventSource (/api/events) feeds every page. Names,
// hosts and versions come from the network; they are only ever inserted
// with textContent or attribute setters, never as HTML.
'use strict';

(function () {
  const body = document.body;
  const page = body.dataset.page;
  const services = new Map(); // id -> service JSON
  const frames = new Map(); // id -> {path, t (client ms)}
  let skew = 0; // server clock - client clock, ms
  let connected = false;
  let loaded = false;

  function el(tag, attrs, ...children) {
    const e = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v === undefined || v === null || v === false) continue;
        if (k === 'class') e.className = v;
        else if (k === 'text') e.textContent = v;
        else e.setAttribute(k, v === true ? '' : String(v));
      }
    }
    for (const c of children) {
      if (c !== null && c !== undefined) e.append(c);
    }
    return e;
  }

  function sorted(onlyOnline) {
    return [...services.values()]
      .filter((s) => !onlyOnline || s.online)
      .sort((a, b) => {
        const x = a.name.toLowerCase();
        const y = b.name.toLowerCase();
        return x < y ? -1 : x > y ? 1 : a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
      });
  }

  function viewURL(id) {
    return '/view/' + encodeURIComponent(id);
  }

  function safeRTSP(url) {
    return typeof url === 'string' && /^rtsp:\/\/[^\s"'<>]+$/.test(url) ? url : '';
  }

  function ago(t) {
    if (!t) return 'no frame yet';
    const s = Math.max(0, Math.round((Date.now() - t) / 1000));
    if (s < 60) return 'last frame ' + s + 's ago';
    const m = Math.floor(s / 60);
    if (m < 60) return 'last frame ' + m + ' min ago';
    const h = Math.floor(m / 60);
    if (h < 48) return 'last frame ' + h + ' h ago';
    return 'last frame ' + Math.floor(h / 24) + ' days ago';
  }

  function frameTime(s) {
    const f = frames.get(s.id);
    if (f && f.t) return f.t;
    return s.last_frame_at ? Date.parse(s.last_frame_at) - skew : 0;
  }

  function imagePath(s) {
    const f = frames.get(s.id);
    if (f && f.path) return f.path;
    if (s.image) return s.image;
    // Online but no frame seen yet: the hub serves the desktop's waiting
    // image. Offline and never seen: there is nothing to show.
    return s.online ? '/s/' + encodeURIComponent(s.id) + '/latest.jpg' : '';
  }

  // Swap an <img> to a new source only once it has loaded, so frames
  // never flash blank.
  function setImage(img, src) {
    if (!src || img.dataset.src === src) return;
    img.dataset.src = src;
    const pre = new Image();
    pre.onload = () => {
      if (img.dataset.src === src) img.src = src;
    };
    pre.src = src;
  }

  let toastTimer = 0;
  function toast(msg) {
    const t = document.getElementById('toast');
    if (!t) return;
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, 2500);
  }

  async function copy(text) {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
      } else {
        const ta = el('textarea', { readonly: true, class: 'visually-hidden' });
        ta.value = text;
        document.body.append(ta);
        ta.select();
        const ok = document.execCommand('copy');
        ta.remove();
        if (!ok) throw new Error('copy failed');
      }
      toast('Copied ' + text);
    } catch (e) {
      window.prompt('Copy this URL:', text);
    }
  }

  // RTSP link plus copy buttons. The URL never carries credentials.
  function rtspWidget(s, withIP) {
    const url = safeRTSP(s.rtsp_url);
    if (!url) return null;
    const box = el('span', { class: 'rtsp' });
    box.append(el('a', {
      class: 'rtsp-link', href: url,
      title: 'Open the RTSP stream (VLC or another player): ' + url,
      'aria-label': 'RTSP stream of ' + s.name + ', opens in VLC or another RTSP player',
      text: 'RTSP',
    }));
    const b = el('button', {
      type: 'button', class: 'button small',
      title: 'Copy ' + url, 'aria-label': 'Copy RTSP URL of ' + s.name, text: 'Copy',
    });
    b.addEventListener('click', () => copy(url));
    box.append(b);
    const ip = safeRTSP(s.rtsp_url_ip);
    if (withIP && ip) {
      const b2 = el('button', {
        type: 'button', class: 'button small',
        title: 'Copy ' + ip, 'aria-label': 'Copy RTSP URL of ' + s.name + ' by IP address', text: 'Copy IP URL',
      });
      b2.addEventListener('click', () => copy(ip));
      box.append(b2);
    }
    return box;
  }

  function rtspKey(s) {
    return (s.rtsp_url || '') + '|' + (s.rtsp_url_ip || '') + '|' + s.name;
  }

  // ---------------------------------------------------------------- index
  const index = {
    tiles: new Map(),
    selected: new Set(),
    selecting: false,

    init() {
      this.grid = document.getElementById('grid');
      this.offline = document.getElementById('offline');
      this.offlineSection = document.getElementById('offline-section');
      this.empty = document.getElementById('empty');
      this.summary = document.getElementById('summary');
      this.bar = document.getElementById('selectbar');
      this.count = document.getElementById('select-count');
      this.viewBtn = document.getElementById('view-selected');
      this.toggle = document.getElementById('select-toggle');
      this.toggle.addEventListener('click', () => this.setSelecting(!this.selecting));
      document.getElementById('select-clear').addEventListener('click', () => {
        this.selected.clear();
        this.render();
      });
      this.viewBtn.addEventListener('click', () => {
        const ids = sorted(true).map((s) => s.id).filter((id) => this.selected.has(id));
        if (ids.length) location.href = '/wall?s=' + ids.map(encodeURIComponent).join(',');
      });
    },

    setSelecting(on) {
      this.selecting = on;
      this.toggle.setAttribute('aria-pressed', on ? 'true' : 'false');
      this.toggle.textContent = on ? 'Done' : 'Select';
      this.bar.hidden = !on;
      this.grid.classList.toggle('selecting', on);
      if (!on) this.selected.clear();
      this.render();
    },

    tile(s) {
      let t = this.tiles.get(s.id);
      if (!t) {
        const img = el('img', { alt: '', loading: 'lazy', decoding: 'async' });
        const shot = el('a', { class: 'shot', href: viewURL(s.id) }, img);
        const name = el('a', { class: 'name', href: viewURL(s.id) });
        const host = el('span', { class: 'host' });
        const age = el('span', { class: 'age' });
        const rtsp = el('span', { class: 'rtsp-slot' });
        const box = el('input', { type: 'checkbox' });
        const pickText = el('span', { text: 'Select' });
        const pick = el('label', { class: 'pick' }, box, pickText);
        box.addEventListener('change', () => {
          if (box.checked) this.selected.add(s.id);
          else this.selected.delete(s.id);
          this.updateSelection();
        });
        const li = el('li', { class: 'tile', 'data-id': s.id },
          shot,
          el('div', { class: 'meta' },
            el('div', { class: 'title-row' }, name, host),
            el('div', { class: 'info-row' }, el('span', null, el('span', { class: 'dot', 'aria-hidden': 'true' }), age), rtsp)),
          pick);
        t = { li, img, name, host, age, rtsp, box, rtspKey: null };
        this.tiles.set(s.id, t);
      }
      t.name.textContent = s.name;
      t.name.title = s.name;
      t.host.textContent = s.host;
      t.host.title = s.host;
      t.img.alt = 'Screen of ' + s.name;
      t.box.setAttribute('aria-label', 'Select ' + s.name);
      t.box.checked = this.selected.has(s.id);
      t.li.classList.toggle('selected', this.selecting && this.selected.has(s.id));
      if (t.rtspKey !== rtspKey(s)) {
        t.rtspKey = rtspKey(s);
        t.rtsp.replaceChildren();
        const w = rtspWidget(s, false);
        if (w) t.rtsp.append(w);
      }
      setImage(t.img, imagePath(s));
      t.age.textContent = ago(frameTime(s));
      return t.li;
    },

    render() {
      const online = sorted(true);
      const offline = sorted(false).filter((s) => !s.online);
      for (const id of [...this.tiles.keys()]) {
        const s = services.get(id);
        if (!s || !s.online) {
          this.tiles.get(id).li.remove();
          this.tiles.delete(id);
          this.selected.delete(id);
        }
      }
      online.forEach((s, i) => {
        const li = this.tile(s);
        if (this.grid.children[i] !== li) this.grid.insertBefore(li, this.grid.children[i] || null);
      });
      this.offline.replaceChildren(...offline.map((s) => el('li', null,
        el('span', { class: 'name', text: s.name }),
        el('span', { class: 'host', text: s.host }),
        el('span', { class: 'badge-off', text: 'Offline' }),
        el('span', { class: 'age', text: s.last_seen ? 'last seen ' + ago(Date.parse(s.last_seen) - skew).replace('last frame ', '') : 'never seen' }))));
      this.offlineSection.hidden = offline.length === 0;
      this.empty.hidden = !loaded || online.length > 0;
      const n = online.length;
      this.toggle.hidden = n === 0 && !this.selecting;
      document.getElementById('wall-all').hidden = n === 0;
      this.summary.textContent = !connected && !loaded ? 'Connecting…'
        : (n === 1 ? '1 screen online' : n + ' screens online') +
          (offline.length ? ', ' + offline.length + ' offline' : '') +
          (connected ? '' : ' (reconnecting…)');
      this.updateSelection();
    },

    updateSelection() {
      const n = this.selected.size;
      this.count.textContent = n === 0 ? 'Pick screens to view together' : n === 1 ? '1 selected' : n + ' selected';
      this.viewBtn.disabled = n === 0;
      for (const [id, t] of this.tiles) t.li.classList.toggle('selected', this.selecting && this.selected.has(id));
    },

    frame(id) {
      const t = this.tiles.get(id);
      const s = services.get(id);
      if (t && s) {
        setImage(t.img, imagePath(s));
        t.age.textContent = ago(frameTime(s));
      }
    },

    tick() {
      for (const [id, t] of this.tiles) {
        const s = services.get(id);
        if (s) t.age.textContent = ago(frameTime(s));
      }
    },
  };

  // ----------------------------------------------------------------- view
  const view = {
    init() {
      this.id = body.dataset.id;
      this.img = document.getElementById('v-img');
      this.stage = document.getElementById('main');
      this.prev = document.getElementById('prev');
      this.next = document.getElementById('next');
      this.rtspKey = null;
      fullscreenButton(this.stage);
      document.addEventListener('keydown', (e) => {
        if (e.altKey || e.ctrlKey || e.metaKey || isTyping(e.target)) return;
        if (e.key === 'ArrowLeft' && !this.prev.hidden) { location.href = this.prev.href; e.preventDefault(); }
        if (e.key === 'ArrowRight' && !this.next.hidden) { location.href = this.next.href; e.preventDefault(); }
      });
    },

    render() {
      const s = services.get(this.id);
      const state = document.getElementById('v-state');
      if (!s) {
        if (loaded) {
          state.textContent = 'No longer known to the hub';
          state.className = 'state off';
          this.stage.classList.add('offline');
        }
        this.nav();
        return;
      }
      document.getElementById('v-name').textContent = s.name;
      document.getElementById('v-host').textContent = s.host;
      document.getElementById('v-version').textContent = s.version ? 'snoopy ' + s.version : '';
      document.title = s.name + ' · Snoopy hub';
      state.textContent = s.online ? 'Online' : 'Offline';
      state.className = 'state ' + (s.online ? 'on' : 'off');
      this.stage.classList.toggle('offline', !s.online);
      this.img.alt = 'Screen of ' + s.name;
      if (this.rtspKey !== rtspKey(s)) {
        this.rtspKey = rtspKey(s);
        const slot = document.getElementById('v-rtsp');
        slot.replaceChildren();
        const w = rtspWidget(s, true);
        if (w) slot.append(w);
      }
      const src = imagePath(s);
      document.getElementById('v-none').hidden = !!src || !!this.img.dataset.src;
      setImage(this.img, src);
      this.tick();
      this.nav();
    },

    nav() {
      // Online screens in name order, plus this one if it went offline.
      const ids = sorted(false).filter((x) => x.online || x.id === this.id).map((x) => x.id);
      const i = ids.indexOf(this.id);
      const show = ids.length > 1 && i >= 0;
      this.prev.hidden = !show;
      this.next.hidden = !show;
      if (show) {
        const p = services.get(ids[(i - 1 + ids.length) % ids.length]);
        const n = services.get(ids[(i + 1) % ids.length]);
        this.prev.href = viewURL(p.id);
        this.prev.title = 'Previous: ' + p.name;
        this.next.href = viewURL(n.id);
        this.next.title = 'Next: ' + n.name;
      }
    },

    frame(id) {
      if (id === this.id) {
        const s = services.get(id);
        if (s) setImage(this.img, imagePath(s));
        this.tick();
      }
    },

    tick() {
      const s = services.get(this.id);
      document.getElementById('v-age').textContent = s ? ago(frameTime(s)) : '';
    },
  };

  // ----------------------------------------------------------------- wall
  const wall = {
    cells: new Map(),

    init() {
      this.ids = (body.dataset.ids || '').split(',').filter(Boolean);
      this.list = document.getElementById('wall');
      this.empty = document.getElementById('empty');
      this.summary = document.getElementById('summary');
      fullscreenButton(document.getElementById('main'));
      window.addEventListener('resize', () => this.layout());
      document.addEventListener('fullscreenchange', () => this.layout());
    },

    shown() {
      if (this.ids.length === 0) return sorted(true);
      return this.ids.map((id) => services.get(id) || { id, name: 'Unknown screen', host: '', online: false, unknown: true });
    },

    cell(s) {
      let c = this.cells.get(s.id);
      if (!c) {
        const img = el('img', { alt: '' });
        const n = el('span', { class: 'n' });
        const a = el('span', { class: 'a' });
        const off = el('span', { class: 'off', text: 'Offline' });
        const link = el('a', { href: viewURL(s.id) }, img);
        const li = el('li', { class: 'cell' }, link, el('div', { class: 'label' }, n, off, a));
        c = { li, img, n, a, off, link };
        this.cells.set(s.id, c);
      }
      c.n.textContent = s.name;
      c.img.alt = 'Screen of ' + s.name;
      c.link.setAttribute('aria-label', 'Open ' + s.name);
      c.off.hidden = s.online;
      c.off.textContent = s.unknown ? 'Unknown' : 'Offline';
      c.li.classList.toggle('offline', !s.online);
      if (!s.unknown) setImage(c.img, imagePath(s));
      c.a.textContent = s.unknown ? '' : ago(frameTime(s));
      return c.li;
    },

    render() {
      const list = this.shown();
      const keep = new Set(list.map((s) => s.id));
      for (const [id, c] of this.cells) {
        if (!keep.has(id)) { c.li.remove(); this.cells.delete(id); }
      }
      list.forEach((s, i) => {
        const li = this.cell(s);
        if (this.list.children[i] !== li) this.list.insertBefore(li, this.list.children[i] || null);
      });
      this.empty.hidden = !loaded || list.length > 0;
      this.list.hidden = list.length === 0;
      const online = list.filter((s) => s.online).length;
      this.summary.textContent = !loaded ? 'Connecting…'
        : online + ' of ' + list.length + ' online' + (connected ? '' : ' (reconnecting…)');
      this.layout();
    },

    // Pick the grid that makes 16:9 screens as large as possible; among
    // grids within 10% of that, prefer more columns on a landscape
    // viewport (screens side by side) and fewer on a portrait one.
    layout() {
      const n = this.list.children.length;
      if (!n) return;
      const w = this.list.clientWidth || window.innerWidth;
      const h = this.list.clientHeight || window.innerHeight;
      const scales = [];
      for (let cols = 1; cols <= n; cols++) {
        const r = Math.ceil(n / cols);
        if (cols > 1 && Math.ceil(n / (cols - 1)) === r) continue; // same rows, emptier
        scales.push([cols, Math.min(w / cols / 16, h / r / 9)]);
      }
      const top = Math.max(...scales.map((s) => s[1]));
      const ok = scales.filter((s) => s[1] >= top * 0.9).map((s) => s[0]);
      const best = w >= h ? Math.max(...ok) : Math.min(...ok);
      const rows = Math.ceil(n / best);
      // Size the cells to 16:9 so labels sit on the picture; a different
      // aspect ratio is letterboxed inside its cell.
      const gap = 4;
      const cw = Math.floor(Math.min((w - gap * (best - 1)) / best, ((h - gap * (rows - 1)) / rows) * 16 / 9));
      const ch = Math.floor(cw * 9 / 16);
      this.list.style.gridTemplateColumns = 'repeat(' + best + ', ' + cw + 'px)';
      this.list.style.gridTemplateRows = 'repeat(' + rows + ', ' + ch + 'px)';
    },

    frame(id) {
      const c = this.cells.get(id);
      const s = services.get(id);
      if (c && s) {
        setImage(c.img, imagePath(s));
        c.a.textContent = ago(frameTime(s));
      }
    },

    tick() {
      for (const [id, c] of this.cells) {
        const s = services.get(id);
        if (s) c.a.textContent = ago(frameTime(s));
      }
    },
  };

  function isTyping(t) {
    return t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
  }

  function fullscreenButton(target) {
    const b = document.getElementById('fullscreen');
    if (!b) return;
    if (!document.fullscreenEnabled) { b.hidden = true; return; }
    const toggle = () => {
      if (document.fullscreenElement) document.exitFullscreen();
      else target.requestFullscreen().catch(() => toast('Fullscreen is not available'));
    };
    b.addEventListener('click', toggle);
    document.addEventListener('fullscreenchange', () => {
      b.textContent = document.fullscreenElement ? 'Exit fullscreen' : 'Fullscreen';
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'f' && !e.altKey && !e.ctrlKey && !e.metaKey && !isTyping(e.target)) toggle();
    });
  }

  const ui = { index, view, wall }[page];
  if (!ui) return;
  ui.init();

  function parse(e) {
    try { return JSON.parse(e.data); } catch (err) { return null; }
  }

  const es = new EventSource('/api/events');
  es.onopen = () => { connected = true; if (loaded) ui.render(); };
  es.onerror = () => { connected = false; if (loaded) ui.render(); };
  es.addEventListener('snapshot', (e) => {
    const d = parse(e);
    if (!d) return;
    skew = Date.parse(d.now) - Date.now();
    services.clear();
    for (const s of d.services) services.set(s.id, s);
    connected = true;
    loaded = true;
    ui.render();
  });
  for (const kind of ['added', 'updated', 'online', 'offline']) {
    es.addEventListener(kind, (e) => {
      const s = parse(e);
      if (!s) return;
      services.set(s.id, s);
      ui.render();
    });
  }
  es.addEventListener('removed', (e) => {
    const d = parse(e);
    if (!d) return;
    services.delete(d.id);
    frames.delete(d.id);
    ui.render();
  });
  es.addEventListener('frame', (e) => {
    const f = parse(e);
    if (!f || !services.has(f.id)) return;
    const prev = frames.get(f.id);
    const real = !/\/waiting\.jpg$/.test(f.path);
    frames.set(f.id, { path: f.path, t: real ? Date.now() : (prev ? prev.t : 0) });
    ui.frame(f.id);
  });
  setInterval(() => ui.tick(), 1000);
})();
