/* Helios SPA — no framework, no build step. Hash router + fetch + one
   custom player. hls.js (vendored) handles HLS; everything else is DOM. */
'use strict';

const $ = (s, r = document) => r.querySelector(s);
const view = $('#view');

// ---------- utils ----------
function el(tag, attrs = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') n.className = v;
    else if (k === 'html') n.innerHTML = v; // trusted, static markup only
    else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) n.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null) continue;
    n.append(kid.nodeType ? kid : document.createTextNode(kid));
  }
  return n;
}
const api = {
  async req(method, url, body) {
    const res = await fetch(url, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.statusText);
    return data;
  },
  get: (u) => api.req('GET', u),
  post: (u, b) => api.req('POST', u, b),
  put: (u, b) => api.req('PUT', u, b),
  del: (u) => api.req('DELETE', u),
};
function toast(msg, kind = '') {
  const t = el('div', { class: `toast ${kind}` }, msg);
  $('#toasts').append(t);
  setTimeout(() => { t.style.opacity = '0'; t.style.transition = 'opacity .3s'; }, 2600);
  setTimeout(() => t.remove(), 3000);
}
function fmtTime(s) {
  if (!isFinite(s) || s < 0) s = 0;
  s = Math.floor(s);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return h ? `${h}:${String(m).padStart(2, '0')}:${String(sec).padStart(2, '0')}`
           : `${m}:${String(sec).padStart(2, '0')}`;
}
const fmtMins = (s) => `${Math.round(s / 60)} min`;
const fmtClock = (d) => new Date(d).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
const fmtDate = (d) => new Date(d).toLocaleDateString([], { month: 'short', day: 'numeric' });
const epCode = (it) => `S${String(it.season).padStart(2, '0')}E${String(it.episode).padStart(2, '0')}`;
const pref = {
  get: (k, d) => { try { return JSON.parse(localStorage.getItem('helios.' + k)) ?? d; } catch { return d; } },
  set: (k, v) => localStorage.setItem('helios.' + k, JSON.stringify(v)),
};

// ---------- cards ----------
function art(id, kind = 'poster') {
  const wrap = el('div', { class: 'art' });
  const img = el('img', { loading: 'lazy', alt: '' });
  img.src = `/api/img/${id}?type=${kind}`;
  img.onerror = () => { img.remove(); };
  wrap.append(img);
  return wrap;
}
function itemLabel(it) {
  if (it.kind === 'episode') return { title: it.show, sub: `${epCode(it)}${it.title ? ' · ' + it.title : ''}` };
  if (it.kind === 'recording') return { title: it.title, sub: it.subtitle || (it.start ? `${fmtDate(it.start)} · ${it.channel || ''}` : '') };
  return { title: it.title, sub: it.year ? String(it.year) : (it.duration ? fmtMins(it.duration) : '') };
}
function card(it, opts = {}) {
  const { title, sub } = itemLabel(it);
  const a = art(it.id);
  a.append(el('div', { class: 'fallback' }, title));
  a.append(el('div', { class: 'play-hint' },
    el('i', { html: '<svg viewBox="0 0 24 24"><path d="M8 5.5v13l11-6.5z"/></svg>' })));
  if (it.watch && !it.watch.done && it.watch.dur > 0) {
    const p = el('div', { class: 'prog' }, el('i'));
    p.firstChild.style.width = `${Math.min(100, it.watch.pos / it.watch.dur * 100)}%`;
    a.append(p);
  }
  if (it.kind === 'recording') {
    const b = { cut: ['ADS CUT', 'cut'], ready: ['AD SKIP', 'ads'], pending: ['AD SCAN…', 'pending'], failed: ['SCAN FAILED', 'pending'] }[it.breaksState];
    if (it.status === 'recording') a.append(el('span', { class: 'badge rec' }, '● REC'));
    else if (b) a.append(el('span', { class: 'badge ' + b[1] }, b[0]));
  }
  const c = el('button', { class: 'card', onclick: opts.onclick || (() => Player.open(it)) },
    a, el('div', { class: 'meta' }, el('b', {}, title), el('small', {}, sub)));
  return c;
}
function skeletons(n = 8) {
  return el('div', { class: 'row-strip' },
    Array.from({ length: n }, () => el('div', { class: 'card skel' }, el('div', { class: 'art' }))));
}
function strip(title, items) {
  if (!items || !items.length) return null;
  return el('section', { class: 'row' },
    el('div', { class: 'row-head' }, el('h2', {}, title)),
    el('div', { class: 'row-strip' }, items.map((it) => card(it))));
}

// ---------- views ----------
const routes = {
  home: renderHome, movies: renderMovies, shows: renderShows,
  show: renderShow, live: renderLive, dvr: renderDVR, settings: renderSettings,
};
function navigate() {
  const [_, route = 'home', arg = ''] = location.hash.split('/');
  document.querySelectorAll('.rail a[data-nav]').forEach((a) =>
    a.classList.toggle('active', a.dataset.nav === route));
  (routes[route] || renderHome)(decodeURIComponent(arg));
  view.focus({ preventScroll: true });
}
window.addEventListener('hashchange', navigate);

async function renderHome() {
  view.replaceChildren(el('div', { class: 'page' }, skeletons()));
  const h = await api.get('/api/home').catch((e) => (toast(e.message, 'err'), {}));
  const heroItem = (h.continue || [])[0] || (h.movies || [])[0] || (h.recordings || [])[0];
  const frag = document.createDocumentFragment();
  if (heroItem) {
    const { title, sub } = itemLabel(heroItem);
    const bg = `/api/img/${heroItem.id}?type=backdrop`;
    const resume = heroItem.watch && !heroItem.watch.done && heroItem.watch.pos > 60;
    const hero = el('div', { class: 'hero' },
      el('div', { class: 'bg-bleed', style: `background-image:url('${bg}')` }),
      el('div', { class: 'bg', style: `background-image:url('${bg}')` }),
      el('div', { class: 'scrim' }),
      el('div', { class: 'hero-body' },
        el('span', { class: 'eyebrow' }, resume ? 'Continue watching' : 'In your library'),
        el('h1', {}, title),
        el('div', { class: 'hero-meta' },
          sub ? el('span', { class: 'chip' }, sub) : null,
          heroItem.height ? el('span', { class: 'chip' }, heroItem.height >= 2000 ? '4K' : `${heroItem.height}p`) : null,
          heroItem.duration ? el('span', { class: 'chip' }, fmtMins(heroItem.duration)) : null,
          heroItem.breaksState === 'cut' ? el('span', { class: 'chip hot' }, 'Ads removed') : null,
          heroItem.breaksState === 'ready' ? el('span', { class: 'chip hot' }, 'Ad-skip ready') : null),
        el('div', { class: 'hero-actions' },
          el('button', {
            class: 'btn primary',
            html: '<svg viewBox="0 0 24 24"><path d="M8 5.5v13l11-6.5z"/></svg>' + (resume ? `Resume · ${fmtTime(heroItem.watch.pos)}` : 'Play'),
            onclick: () => Player.open(heroItem),
          }),
          resume ? el('button', { class: 'btn', onclick: () => Player.open(heroItem, { from: 0 }) }, 'Start over') : null)));
    frag.append(hero);
  } else {
    frag.append(el('div', { class: 'page' },
      el('div', { class: 'page-head' }, el('h1', {}, 'Welcome to Helios')),
      el('p', { class: 'empty' },
        'The library is empty. Add media folders in Settings, then run a scan — or head to Live TV and record something.')));
  }
  frag.append(
    strip('Continue watching', h.continue),
    strip('Recorded off-air', h.recordings),
    strip('Recently added movies', h.movies),
    strip('Recently added episodes', h.episodes));
  view.replaceChildren(frag);
}

async function renderMovies() {
  view.replaceChildren(el('div', { class: 'page' }, skeletons()));
  const items = await api.get('/api/movies').catch(() => []);
  view.replaceChildren(el('div', { class: 'page' },
    el('div', { class: 'page-head' }, el('h1', {}, 'Movies'), el('span', { class: 'count' }, `${items.length}`)),
    items.length
      ? el('div', { class: 'grid' }, items.map((it) => card(it)))
      : el('p', { class: 'empty' }, 'No movies yet. Point a library folder at your films in Settings and scan.')));
}

async function renderShows() {
  view.replaceChildren(el('div', { class: 'page' }, skeletons()));
  const shows = await api.get('/api/shows').catch(() => []);
  view.replaceChildren(el('div', { class: 'page' },
    el('div', { class: 'page-head' }, el('h1', {}, 'Shows'), el('span', { class: 'count' }, `${shows.length}`)),
    shows.length
      ? el('div', { class: 'grid' }, shows.map((s) => {
          const a = art(s.posterId);
          a.append(el('div', { class: 'fallback' }, s.show));
          return el('button', { class: 'card', onclick: () => (location.hash = `#/show/${encodeURIComponent(s.show)}`) },
            a, el('div', { class: 'meta' },
              el('b', {}, s.show),
              el('small', {}, `${s.seasons} season${s.seasons > 1 ? 's' : ''} · ${s.episodes} ep`)));
        }))
      : el('p', { class: 'empty' }, 'No shows found. Episodes are matched by SxxEyy in the filename.')));
}

async function renderShow(name) {
  const data = await api.get(`/api/shows/${encodeURIComponent(name)}`).catch(() => ({ episodes: [] }));
  const seasons = new Map();
  for (const ep of data.episodes) {
    if (!seasons.has(ep.season)) seasons.set(ep.season, []);
    seasons.get(ep.season).push(ep);
  }
  view.replaceChildren(el('div', { class: 'page' },
    el('span', { class: 'eyebrow' }, 'Series'),
    el('div', { class: 'page-head' }, el('h1', {}, name)),
    [...seasons.entries()].map(([season, eps]) => el('div', { class: 'section' },
      el('h2', {}, `Season ${season}`),
      eps.map((ep) => {
        const w = ep.watch;
        return el('div', { class: 'list-item' },
          el('button', {
            class: 'icon-btn', title: 'Play',
            html: '<svg viewBox="0 0 24 24"><path d="M8 5.5v13l11-6.5z"/></svg>',
            onclick: () => Player.open(ep),
          }),
          el('div', { class: 'grow' },
            el('b', {}, `${epCode(ep)}${ep.title ? ' — ' + ep.title : ''}`),
            el('small', {}, `${fmtMins(ep.duration)}${w && !w.done && w.pos > 60 ? ` · resume at ${fmtTime(w.pos)}` : ''}`)),
          w && w.done ? el('span', { class: 'status', style: 'color:var(--ok);border-color:rgba(90,209,154,.4)' }, 'Watched') : null);
      })))));
}

async function renderLive() {
  view.replaceChildren(el('div', { class: 'page' },
    el('div', { class: 'page-head' }, el('h1', {}, 'Live TV')), skeletons(4)));
  const [lineup, guide] = await Promise.all([
    api.get('/api/livetv/channels').catch(() => ({ channels: [] })),
    api.get('/api/guide?hours=6').catch(() => ({ airings: {} })),
  ]);
  const chans = lineup.channels || [];
  const now = Date.now();
  const nowNext = (num) => {
    const list = (guide.airings || {})[num] || [];
    const cur = list.find((a) => new Date(a.start) <= now && now < new Date(a.end));
    const next = list.find((a) => new Date(a.start) > now);
    return { cur, next };
  };
  const page = el('div', { class: 'page' },
    el('div', { class: 'page-head' },
      el('h1', {}, 'Live TV'),
      el('span', { class: 'count' }, lineup.device ? `${lineup.device.ModelNumber || 'HDHomeRun'} · ${chans.length} channels` : ''),
      el('span', { class: 'flex', style: 'flex:1' }),
      el('button', {
        class: 'btn small', onclick: async (e) => {
          e.target.disabled = true;
          try { await api.post('/api/livetv/refresh'); toast('Lineup refreshed', 'beam'); renderLive(); }
          catch (err) { toast(err.message, 'err'); e.target.disabled = false; }
        },
      }, 'Refresh lineup')));
  if (!chans.length) {
    page.append(el('p', { class: 'empty' },
      'No tuner yet. Helios auto-discovers HDHomeRuns on the LAN — if broadcast is blocked (VLANs, k8s), set the tuner IP in Settings and refresh.'));
  }
  for (const ch of chans) {
    const { cur, next } = nowNext(ch.guideNumber);
    const row = el('div', { class: 'chan' },
      el('div', { class: 'num' }, ch.guideNumber, el('small', {}, ch.guideName)),
      el('div', { class: 'now' },
        cur ? el('b', {}, cur.title) : el('b', { class: 'muted' }, 'No guide data'),
        cur ? el('small', {}, `${fmtClock(cur.start)}–${fmtClock(cur.end)}${next ? ` · Next: ${next.title}` : ''}`) : null,
        cur ? (() => {
          const bar = el('div', { class: 'airbar' }, el('i'));
          const a = new Date(cur.start).getTime(), b = new Date(cur.end).getTime();
          bar.firstChild.style.width = `${Math.min(100, (now - a) / (b - a) * 100)}%`;
          return bar;
        })() : null),
      el('div', { class: 'acts' },
        el('button', { class: 'btn small primary', onclick: () => Player.openLive(ch, cur) }, 'Watch'),
        el('button', {
          class: 'btn small', title: cur ? `Record ${cur.title}` : 'Record next hour',
          onclick: async () => {
            const body = cur
              ? { channelId: ch.guideNumber, title: cur.title, subtitle: cur.subtitle || '', start: cur.start, end: cur.end }
              : { channelId: ch.guideNumber, title: `${ch.guideName} capture`, start: new Date().toISOString(), end: new Date(Date.now() + 36e5).toISOString() };
            try { await api.post('/api/dvr/record', body); toast(`Recording ${body.title}`, 'beam'); }
            catch (e) { toast(e.message, 'err'); }
          },
        }, '⏺ Rec'),
        cur ? el('button', {
          class: 'btn small', title: 'Record every airing',
          onclick: async () => {
            try { await api.post('/api/dvr/rules', { title: cur.title }); toast(`Series pass: ${cur.title}`, 'beam'); }
            catch (e) { toast(e.message, 'err'); }
          },
        }, 'Series') : null));
    page.append(row);
  }
  view.replaceChildren(page);
}

async function renderDVR() {
  const [recs, rules, lineup] = await Promise.all([
    api.get('/api/dvr/recordings').catch(() => []),
    api.get('/api/dvr/rules').catch(() => []),
    api.get('/api/livetv/channels').catch(() => ({ channels: [] })),
  ]);
  const upcoming = recs.filter((r) => r.status === 'scheduled' || r.status === 'recording')
    .sort((a, b) => new Date(a.start) - new Date(b.start));
  const done = recs.filter((r) => r.status === 'done');
  const failed = recs.filter((r) => r.status === 'failed');

  const upcomingList = upcoming.length ? upcoming.map((r) => el('div', { class: 'list-item' },
    el('span', { class: `status ${r.status}` }, r.status === 'recording' ? '● REC' : 'Scheduled'),
    el('div', { class: 'grow' },
      el('b', {}, r.title + (r.subtitle ? ` — ${r.subtitle}` : '')),
      el('small', {}, `${fmtDate(r.start)} ${fmtClock(r.start)}–${fmtClock(r.end)} · ch ${r.channel || ''}`)),
    el('button', {
      class: 'btn small danger', onclick: async (e) => {
        await api.del(`/api/dvr/recordings/${r.id}`).catch((err) => toast(err.message, 'err'));
        e.target.closest('.list-item').remove();
      },
    }, r.status === 'recording' ? 'Stop' : 'Cancel')))
    : [el('p', { class: 'empty' }, 'Nothing on the schedule. Add a series pass below or record from Live TV.')];

  const ruleForm = (() => {
    const title = el('input', { placeholder: 'Exact show title as it appears in the guide' });
    const chan = el('select', {},
      el('option', { value: '' }, 'Any channel'),
      (lineup.channels || []).map((c) => el('option', { value: c.guideNumber }, `${c.guideNumber} ${c.guideName}`)));
    const keep = el('input', { type: 'number', min: '0', value: '0', title: 'Keep newest N (0 = all)' });
    return el('div', { class: 'form' },
      el('div', { class: 'form-row' },
        el('div', { class: 'field' }, el('label', {}, 'Title'), title),
        el('div', { class: 'field' }, el('label', {}, 'Channel'), chan),
        el('div', { class: 'field', style: 'max-width:110px' }, el('label', {}, 'Keep'), keep)),
      el('div', {},
        el('button', {
          class: 'btn primary small', onclick: async () => {
            if (!title.value.trim()) return toast('Rule needs a title', 'err');
            try {
              await api.post('/api/dvr/rules', { title: title.value.trim(), channelId: chan.value, keep: +keep.value || 0 });
              toast('Series pass added', 'beam'); renderDVR();
            } catch (e) { toast(e.message, 'err'); }
          },
        }, 'Add series pass')));
  })();

  const ruleList = rules.map((ru) => el('div', { class: 'list-item' },
    el('div', { class: 'grow' },
      el('b', {}, ru.title),
      el('small', {}, `${ru.channelId ? 'Channel ' + ru.channelId : 'Any channel'}${ru.keep ? ` · keep ${ru.keep}` : ''}`)),
    el('button', {
      class: 'btn small danger', onclick: async (e) => {
        await api.del(`/api/dvr/rules/${ru.id}`).catch((err) => toast(err.message, 'err'));
        e.target.closest('.list-item').remove();
      },
    }, 'Delete')));

  const recCard = (r) => {
    const c = card(r);
    const actions = el('div', { style: 'display:flex;gap:6px;margin-top:6px;flex-wrap:wrap' },
      el('button', {
        class: 'btn small', title: 'Re-detect ad breaks',
        onclick: async (e) => { e.stopPropagation(); busy(e.target, () => api.post(`/api/dvr/recordings/${r.id}/adscan`), 'Ad scan done'); },
      }, 'Rescan ads'),
      r.breaks && r.breaks.length ? el('button', {
        class: 'btn small', title: 'Cut detected breaks out of the file',
        onclick: async (e) => { e.stopPropagation(); busy(e.target, () => api.post(`/api/dvr/recordings/${r.id}/adscan?cut=1`), 'Ads removed'); },
      }, 'Cut ads') : null,
      el('button', {
        class: 'btn small danger',
        onclick: async (e) => {
          e.stopPropagation();
          await api.del(`/api/dvr/recordings/${r.id}`).catch((err) => toast(err.message, 'err'));
          wrap.remove();
        },
      }, 'Delete'));
    const wrap = el('div', {}, c, actions);
    return wrap;
  };
  async function busy(btn, fn, okMsg) {
    btn.disabled = true;
    try { await fn(); toast(okMsg, 'beam'); renderDVR(); }
    catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  }

  view.replaceChildren(el('div', { class: 'page' },
    el('div', { class: 'page-head' }, el('h1', {}, 'DVR')),
    el('div', { class: 'section' }, el('h2', {}, 'Up next'), upcomingList),
    el('div', { class: 'section' }, el('h2', {}, 'Series passes'), ruleList, ruleForm),
    el('div', { class: 'section' }, el('h2', {}, `Recordings`),
      done.length ? el('div', { class: 'grid' }, done.map(recCard))
                  : el('p', { class: 'empty' }, 'No finished recordings yet.')),
    failed.length ? el('div', { class: 'section' }, el('h2', {}, 'Failed'),
      failed.map((r) => el('div', { class: 'list-item' },
        el('span', { class: 'status failed' }, 'Failed'),
        el('div', { class: 'grow' }, el('b', {}, r.title), el('small', {}, r.error || '')),
        el('button', {
          class: 'btn small danger', onclick: async (e) => {
            await api.del(`/api/dvr/recordings/${r.id}`).catch(() => {});
            e.target.closest('.list-item').remove();
          },
        }, 'Clear')))) : null));
}

async function renderSettings() {
  const s = await api.get('/api/settings').catch(() => ({}));
  const f = {};
  const field = (key, label, hint, attrs = {}) => {
    f[key] = el('input', { value: s[key] ?? '', ...attrs });
    return el('div', { class: 'field' }, el('label', {}, label), f[key],
      hint ? el('div', { class: 'hint' }, hint) : null);
  };
  f.commercialMode = el('select', {},
    ['off', 'skip', 'delete'].map((m) => el('option', { value: m, selected: s.commercialMode === m },
      { off: 'Off', skip: 'Skip — mark breaks, player jumps them', delete: 'Delete — cut breaks out of the file' }[m])));
  f.mediaDirs = el('input', { value: (s.mediaDirs || []).join(', ') });
  f.autoDeleteWatched = el('input', { type: 'checkbox', checked: !!s.autoDeleteWatched });

  view.replaceChildren(el('div', { class: 'page' },
    el('div', { class: 'page-head' }, el('h1', {}, 'Settings')),
    el('div', { class: 'form' },
      el('div', { class: 'field' }, el('label', {}, 'Library folders'), f.mediaDirs,
        el('div', { class: 'hint' }, 'Comma-separated absolute paths. Movies: "Title (2024).mkv" · Episodes: "Show S01E02.mkv"')),
      field('recordingsDir', 'Recordings folder'),
      field('xmltvUrl', 'XMLTV guide', 'URL or file path (.xml / .xml.gz). Pairs with zap2xml or Schedules Direct grabbers.'),
      field('hdhrIp', 'HDHomeRun IP', 'Leave empty for LAN auto-discovery.'),
      el('div', { class: 'field' }, el('label', {}, 'Commercials'), f.commercialMode,
        el('div', { class: 'hint' }, 'Uses comskip when installed; otherwise a black-frame + silence heuristic.')),
      el('div', { class: 'form-row' },
        field('prePadMin', 'Pre-pad (min)', null, { type: 'number', min: '0' }),
        field('postPadMin', 'Post-pad (min)', null, { type: 'number', min: '0' })),
      el('label', { class: 'check' }, f.autoDeleteWatched, 'Delete recordings after they are fully watched'),
      el('div', { style: 'display:flex;gap:10px;flex-wrap:wrap' },
        el('button', {
          class: 'btn primary', onclick: async () => {
            const body = {
              ...s,
              mediaDirs: f.mediaDirs.value.split(',').map((x) => x.trim()).filter(Boolean),
              recordingsDir: f.recordingsDir.value.trim(),
              xmltvUrl: f.xmltvUrl.value.trim(),
              hdhrIp: f.hdhrIp.value.trim(),
              commercialMode: f.commercialMode.value,
              prePadMin: +f.prePadMin.value || 0,
              postPadMin: +f.postPadMin.value || 0,
              autoDeleteWatched: f.autoDeleteWatched.checked,
            };
            try { await api.put('/api/settings', body); toast('Settings saved', 'beam'); }
            catch (e) { toast(e.message, 'err'); }
          },
        }, 'Save changes'),
        el('button', {
          class: 'btn', onclick: async (e) => {
            e.target.disabled = true;
            await api.post('/api/scan').catch((err) => toast(err.message, 'err'));
            toast('Library scan started', 'beam'); e.target.disabled = false;
          },
        }, 'Scan library'),
        el('button', {
          class: 'btn', onclick: async (e) => {
            e.target.disabled = true;
            try { await api.post('/api/livetv/refresh'); toast('Tuner + guide refreshing', 'beam'); }
            catch (err) { toast(err.message, 'err'); }
            e.target.disabled = false;
          },
        }, 'Refresh tuner & guide')))));
}

// ---------- search ----------
const searchInput = $('#search');
let searchTimer;
searchInput.addEventListener('input', () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(async () => {
    const q = searchInput.value.trim();
    if (!q) return navigate();
    const items = await api.get(`/api/search?q=${encodeURIComponent(q)}`).catch(() => []);
    view.replaceChildren(el('div', { class: 'page' },
      el('div', { class: 'page-head' }, el('h1', {}, `Results for “${q}”`), el('span', { class: 'count' }, `${items.length}`)),
      items.length ? el('div', { class: 'grid' }, items.map((it) => card(it)))
                   : el('p', { class: 'empty' }, 'Nothing matched. Search covers titles, shows, and recordings.')));
  }, 250);
});
document.addEventListener('keydown', (e) => {
  if (e.key === '/' && document.activeElement !== searchInput && $('#player').hidden) {
    e.preventDefault(); searchInput.focus();
  }
});

// ---------- player ----------
const Player = (() => {
  const root = $('#player'), video = $('#video'), ambient = $('#ambient');
  const ambientCtx = ambient.getContext('2d', { willReadFrequently: false });
  const ui = {
    play: $('#p-play'), fill: $('#p-fill'), buffer: $('#p-buffer'), thumb: $('#p-thumb'),
    marks: $('#p-marks'), scrub: $('#p-scrub'), cur: $('#p-cur'), dur: $('#p-dur'),
    heading: $('#p-heading'), eyebrow: $('#p-eyebrow'), live: $('#p-live'),
    skip: $('#p-skip'), skiptime: $('#p-skiptime'), vol: $('#p-vol'), mute: $('#p-mute'),
    quality: $('#p-quality'), autoskip: $('#p-autoskip'),
  };
  const ICON_PLAY = '<svg viewBox="0 0 24 24"><path d="M8 5.5v13l11-6.5z"/></svg>';
  const ICON_PAUSE = '<svg viewBox="0 0 24 24"><path d="M7 5h3.6v14H7zM13.4 5H17v14h-3.6z"/></svg>';

  const S = {
    item: null, live: false, channel: null,
    mode: 'direct', session: null, offset: 0,
    duration: 0, breaks: [], hls: null,
    saveTimer: null, ambientTimer: null, idleTimer: null,
    restarts: 0, currentBreak: null,
  };
  ui.autoskip.checked = pref.get('autoskip', true);
  ui.autoskip.onchange = () => pref.set('autoskip', ui.autoskip.checked);

  const absTime = () => S.offset + (video.currentTime || 0);

  function directEligible(it) {
    return it && it.vcodec === 'h264' &&
      ['mov', 'mp4', 'm4v', 'matroska', 'webm'].includes(it.container);
  }
  function qualityOptions(it) {
    const opts = [];
    if (directEligible(it)) opts.push(['direct', 'Direct play']);
    if (it && it.vcodec === 'h264') opts.push(['original', 'Original (remux)']);
    opts.push(['1080', '1080p'], ['720', '720p'], ['480', '480p']);
    return opts;
  }

  async function open(item, opts = {}) {
    close(false);
    S.item = item; S.live = false; S.channel = null; S.restarts = 0;
    S.duration = item.duration || 0;
    S.breaks = (item.breaksState === 'ready' && item.breaks) ? item.breaks : [];
    const { title, sub } = itemLabel(item);
    ui.heading.textContent = title;
    ui.eyebrow.textContent = sub || { movie: 'Movie', episode: 'Episode', recording: 'Recording' }[item.kind] || '';
    ui.live.hidden = true;
    renderQuality(qualityOptions(item));
    renderMarks();
    show();
    const from = opts.from != null ? opts.from
      : (item.watch && !item.watch.done && item.watch.pos > 60 ? item.watch.pos : 0);
    await start(from);
  }

  async function openLive(ch, airing) {
    close(false);
    S.item = null; S.live = true; S.channel = ch.guideNumber; S.restarts = 0;
    S.duration = 0; S.breaks = [];
    ui.heading.textContent = airing ? airing.title : ch.guideName;
    ui.eyebrow.textContent = `${ch.guideNumber} · ${ch.guideName}`;
    ui.live.hidden = false;
    renderQuality([['original', 'Original (remux)'], ['1080', '1080p'], ['720', '720p'], ['480', '480p']], '720');
    renderMarks();
    show();
    await start(0);
  }

  function renderQuality(opts, forced) {
    ui.quality.replaceChildren(...opts.map(([v, label]) => el('option', { value: v }, label)));
    const saved = forced || pref.get('quality', opts[0][0]);
    ui.quality.value = opts.some(([v]) => v === saved) ? saved : opts[0][0];
    ui.quality.onchange = () => { pref.set('quality', ui.quality.value); start(S.live ? 0 : absTime()); };
  }

  async function start(fromSec) {
    teardownStream();
    const q = ui.quality.value;
    try {
      if (!S.live && q === 'direct') {
        S.mode = 'direct'; S.offset = 0;
        video.src = `/stream/direct/${S.item.id}`;
        video.currentTime = fromSec;
      } else {
        S.mode = 'hls';
        const body = S.live
          ? { channel: S.channel, quality: q }
          : { id: S.item.id, start: fromSec, quality: q };
        const res = await api.post('/api/stream/start', body);
        S.session = res.sessionId; S.offset = res.offset || 0;
        if (window.Hls && Hls.isSupported()) {
          S.hls = new Hls({ maxBufferLength: 30, lowLatencyMode: false });
          S.hls.loadSource(res.url);
          S.hls.attachMedia(video);
          S.hls.on(Hls.Events.ERROR, (_, data) => { if (data.fatal) recover(); });
        } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
          video.src = res.url;
        } else {
          throw new Error('This browser cannot play HLS');
        }
      }
      await video.play().catch(() => {}); // autoplay policies: user already clicked
    } catch (e) {
      toast(e.message, 'err');
    }
  }

  // Direct play can fail late (codec/container edge cases): fall back to HLS once.
  video.addEventListener('error', () => {
    if (!root.hidden && S.mode === 'direct' && S.restarts < 1) {
      S.restarts++;
      toast('Direct play failed — transcoding instead', 'beam');
      const opts = qualityOptions(S.item).filter(([v]) => v !== 'direct');
      renderQuality(opts, pref.get('quality', '720') === 'direct' ? '720' : pref.get('quality', '720'));
      start(absTime());
    } else if (!root.hidden && S.mode === 'hls') {
      recover();
    }
  });
  function recover() {
    if (S.restarts >= 3) { toast('Playback failed', 'err'); return; }
    S.restarts++;
    const t = S.live ? 0 : absTime();
    setTimeout(() => start(t), 400);
  }

  function seek(absT) {
    if (S.live) return;
    absT = Math.max(0, Math.min(absT, S.duration || absT));
    if (S.mode === 'direct') { video.currentTime = absT; return; }
    const rel = absT - S.offset;
    const end = video.seekable.length ? video.seekable.end(video.seekable.length - 1) : 0;
    if (rel >= 0 && rel <= end + 1) video.currentTime = rel;
    else start(absT); // outside the transcoded window: new session at the target
  }

  // ---- ad breaks ----
  function activeBreak(t) {
    return S.breaks.find((b) => t >= b.start && t < b.end - 0.25) || null;
  }
  function handleBreaks() {
    if (S.live || !S.breaks.length) return;
    const t = absTime();
    const b = activeBreak(t);
    if (!b) { S.currentBreak = null; ui.skip.hidden = true; return; }
    if (ui.autoskip.checked) {
      seek(b.end + 0.05);
      toast(`Skipped ${fmtTime(b.end - b.start)} ad break`, 'beam');
      S.currentBreak = null; ui.skip.hidden = true;
      return;
    }
    S.currentBreak = b;
    ui.skiptime.textContent = fmtTime(b.end - t);
    ui.skip.hidden = false;
  }
  ui.skip.onclick = () => { if (S.currentBreak) { seek(S.currentBreak.end + 0.05); ui.skip.hidden = true; } };

  function renderMarks() {
    ui.marks.replaceChildren();
    if (!S.duration) return;
    for (const b of S.breaks) {
      const i = el('i');
      i.style.left = `${b.start / S.duration * 100}%`;
      i.style.width = `${Math.max(0.4, (b.end - b.start) / S.duration * 100)}%`;
      ui.marks.append(i);
    }
  }

  // ---- transport UI ----
  video.addEventListener('timeupdate', () => {
    const t = absTime();
    ui.cur.textContent = fmtTime(t);
    if (S.duration) {
      const pct = Math.min(100, t / S.duration * 100);
      ui.fill.style.width = pct + '%';
      ui.thumb.style.left = pct + '%';
      if (video.buffered.length) {
        ui.buffer.style.width = Math.min(100, (S.offset + video.buffered.end(video.buffered.length - 1)) / S.duration * 100) + '%';
      }
    }
    handleBreaks();
  });
  video.addEventListener('play', () => { ui.play.innerHTML = ICON_PAUSE; });
  video.addEventListener('pause', () => { ui.play.innerHTML = ICON_PLAY; });
  video.addEventListener('loadedmetadata', () => {
    if (!S.live && !S.duration && isFinite(video.duration)) S.duration = video.duration;
    ui.dur.textContent = S.live ? 'LIVE' : fmtTime(S.duration);
    renderMarks();
  });

  ui.play.onclick = () => (video.paused ? video.play() : video.pause());
  $('#p-r10').onclick = () => seek(absTime() - 10);
  $('#p-f30').onclick = () => seek(absTime() + 30);
  ui.vol.oninput = () => { video.volume = +ui.vol.value; video.muted = false; };
  ui.mute.onclick = () => { video.muted = !video.muted; };
  $('#p-fs').onclick = () => (document.fullscreenElement ? document.exitFullscreen() : root.requestFullscreen());
  $('#p-pip').onclick = () => { if (document.pictureInPictureElement) document.exitPictureInPicture(); else video.requestPictureInPicture().catch(() => {}); };
  $('#p-back').onclick = () => close(true);

  function scrubTo(e) {
    if (S.live || !S.duration) return;
    const r = ui.scrub.getBoundingClientRect();
    const x = (e.touches ? e.touches[0].clientX : e.clientX) - r.left;
    seek(Math.max(0, Math.min(1, x / r.width)) * S.duration);
  }
  let scrubbing = false;
  ui.scrub.addEventListener('pointerdown', (e) => { scrubbing = true; ui.scrub.setPointerCapture(e.pointerId); scrubTo(e); });
  ui.scrub.addEventListener('pointermove', (e) => scrubbing && scrubTo(e));
  ui.scrub.addEventListener('pointerup', () => { scrubbing = false; });

  // ---- ambient light bleed (the signature) ----
  function ambientTick() {
    if (video.readyState >= 2 && !video.paused) {
      try { ambientCtx.drawImage(video, 0, 0, ambient.width, ambient.height); } catch { /* cross-origin never applies: same origin */ }
    }
  }

  // ---- idle chrome ----
  function poke() {
    root.classList.remove('idle');
    clearTimeout(S.idleTimer);
    S.idleTimer = setTimeout(() => { if (!video.paused) root.classList.add('idle'); }, 2800);
  }
  root.addEventListener('pointermove', poke);
  root.addEventListener('click', (e) => {
    if (e.target === video) (video.paused ? video.play() : video.pause());
    poke();
  });

  // ---- progress persistence ----
  function saveProgress() {
    if (S.live || !S.item || !S.duration || absTime() < 15) return;
    const body = JSON.stringify({ pos: absTime(), dur: S.duration });
    navigator.sendBeacon
      ? navigator.sendBeacon(`/api/watch/${S.item.id}`, new Blob([body], { type: 'application/json' }))
      : api.post(`/api/watch/${S.item.id}`, { pos: absTime(), dur: S.duration });
  }
  window.addEventListener('pagehide', saveProgress);

  function show() {
    root.hidden = false;
    document.body.style.overflow = 'hidden';
    ui.play.innerHTML = ICON_PLAY;
    ui.skip.hidden = true;
    ui.fill.style.width = ui.buffer.style.width = '0%';
    ui.cur.textContent = '0:00';
    ui.dur.textContent = S.live ? 'LIVE' : fmtTime(S.duration);
    ambientCtx.fillStyle = '#000';
    ambientCtx.fillRect(0, 0, ambient.width, ambient.height);
    S.ambientTimer = setInterval(ambientTick, 400);
    S.saveTimer = setInterval(saveProgress, 5000);
    poke();
  }

  function teardownStream() {
    if (S.hls) { S.hls.destroy(); S.hls = null; }
    if (S.session) { api.del(`/api/stream/${S.session}`).catch(() => {}); S.session = null; }
    video.removeAttribute('src');
    video.load();
  }

  function close(nav) {
    if (root.hidden) return;
    saveProgress();
    teardownStream();
    clearInterval(S.ambientTimer);
    clearInterval(S.saveTimer);
    clearTimeout(S.idleTimer);
    root.hidden = true;
    root.classList.remove('idle');
    document.body.style.overflow = '';
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    if (nav) navigate(); // refresh watch-state chips behind the player
  }

  document.addEventListener('keydown', (e) => {
    if (root.hidden || e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT') return;
    const k = e.key.toLowerCase();
    if (k === ' ') { e.preventDefault(); ui.play.click(); }
    else if (k === 'arrowleft') seek(absTime() - 10);
    else if (k === 'arrowright') seek(absTime() + 30);
    else if (k === 'f') $('#p-fs').click();
    else if (k === 'm') ui.mute.click();
    else if (k === 'escape') close(true);
    else if (k === 's' && S.currentBreak) ui.skip.click();
    poke();
  });

  return { open, openLive };
})();

// ---------- boot ----------
navigate();
