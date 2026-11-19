// data-box side panel. Server data is only ever put into the page through
// textContent / DOM nodes (never innerHTML).
'use strict';

const $ = (id) => document.getElementById(id);
const T = t; // i18n; inside task helpers the name t is the task

// The server's voice-note placeholder ("🎤 Голосовая заметка 1:15") in the UI language.
const VOICE_RE = /^🎤 Голосовая заметка( \d+:\d\d)?$/;
const shownText = (text) => text.replace(VOICE_RE, (_, d) => `🎤 ${T('voiceNote')}${d || ''}`);

function el(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) e[k] = v;
    else if (k === 'class') e.className = v;
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const k of kids.flat()) if (k !== null && k !== undefined && k !== false) e.append(k);
  return e;
}

const state = {
  lists: [],
  tasks: [],
  list: Number(localStorage.getItem('list')) || 0,
  me: null,
  stt: false, // Chrome speech recognition sends audio to Google: opt-in
};

// ---- loading ----

function showStatus(msg) {
  $('status').hidden = !msg;
  $('status').textContent = msg || '';
}

let loading = null;
async function load() {
  if (loading) return loading;
  loading = (async () => {
    try {
      const [lists, tasks] = await Promise.all([DB.req('GET', '/api/lists'), DB.req('GET', '/api/tasks')]);
      state.lists = lists;
      state.tasks = tasks;
      if (!lists.some((l) => l.id === state.list)) state.list = lists.length ? lists[0].id : 0;
      chrome.storage.local.set({ lists: lists.map((l) => ({ id: l.id, name: l.name })) });
      chrome.runtime.sendMessage({ type: 'badge' }).catch(() => {});
      showStatus('');
    } catch (e) {
      showStatus(e.message);
    } finally {
      loading = null;
    }
    render();
  })();
  return loading;
}

// Live updates (Server-Sent Events over fetch, reconnecting by itself).
let stopEvents = null, soon = null;
async function connect() {
  if (stopEvents) stopEvents();
  stopEvents = DB.events(
    () => { $('live').classList.add('on'); $('live').title = t('online'); load(); },
    () => { clearTimeout(soon); soon = setTimeout(load, 150); },
    () => { $('live').classList.remove('on'); $('live').title = t('offline'); });
}

// ---- rendering ----

const URL_RE = /https?:\/\/[^\s<>"']+/g;

// Text with clickable http(s) links.
function linkify(text) {
  const out = [];
  let last = 0;
  for (const m of text.matchAll(URL_RE)) {
    const url = m[0].replace(/[.,;:!?)]+$/, '');
    out.push(text.slice(last, m.index));
    out.push(el('a', { href: url, target: '_blank', rel: 'noopener noreferrer' }, url));
    last = m.index + url.length;
  }
  out.push(text.slice(last));
  return out;
}

function ago(ts) {
  const s = Math.max(0, Date.now() / 1000 - ts);
  if (s < 60) return t('justNow');
  if (s < 3600) return t('min', Math.floor(s / 60));
  if (s < 86400) return t('hours', Math.floor(s / 3600));
  return new Date(ts * 1000).toLocaleDateString(LANG === 'ru' ? 'ru-RU' : 'en-GB', { day: 'numeric', month: 'short' });
}

const dur = (s) => `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;

function render() {
  const cur = state.lists.find((l) => l.id === state.list);
  $('tabs').replaceChildren(...state.lists.map((l) => {
    const n = state.tasks.filter((t) => t.list_id === l.id && !t.done_at).length;
    return el('button', {
      class: l.id === state.list ? 'on' : '', style: `--c:${safeColor(l.color)}`,
      onclick: () => { state.list = l.id; localStorage.setItem('list', l.id); render(); },
    }, el('span', { class: 'sw' }), l.name, n ? el('span', { class: 'n' }, String(n)) : null);
  }));
  if (!cur) {
    $('open').replaceChildren();
    $('doneBox').hidden = true;
    $('empty').hidden = true;
    return;
  }
  const mine = state.tasks.filter((t) => t.list_id === cur.id);
  const open = mine.filter((t) => !t.done_at);
  const done = mine.filter((t) => t.done_at);
  $('open').replaceChildren(...open.map(taskRow));
  $('done').replaceChildren(...done.map(taskRow));
  $('empty').hidden = open.length > 0;
  $('doneBox').hidden = done.length === 0;
  $('doneTitle').textContent = t('doneN', done.length);
}

function safeColor(c) {
  return /^#[0-9a-f]{3,8}$/i.test(c || '') ? c : '#94a3b8';
}

function patch(t, body) {
  return DB.req('PATCH', `/api/tasks/${t.id}`, body).then(load).catch((e) => showStatus(e.message));
}

function taskRow(t) {
  const li = el('li', { class: `task${t.pri ? ' pri' : ''}${t.done_at ? ' done' : ''}` });
  const cb = el('input', { type: 'checkbox', title: t.done_at ? T('reopen') : T('markDone') });
  cb.checked = !!t.done_at;
  cb.onchange = () => patch(t, { done: cb.checked });

  const del = el('button', { class: 'icon', title: T('del') }, '🗑');
  del.onclick = () => {
    if (!del.classList.contains('confirm')) {
      del.classList.add('confirm');
      del.textContent = T('delConfirm');
      setTimeout(() => { del.classList.remove('confirm'); del.textContent = '🗑'; }, 3000);
      return;
    }
    DB.req('DELETE', `/api/tasks/${t.id}`).then(load).catch((e) => showStatus(e.message));
  };

  const meta = el('div', { class: 'meta' },
    t.audio ? playButton(t) : null,
    el('span', {}, [t.author, ago(t.created_at)].filter(Boolean).join(' · ')));

  li.append(
    cb,
    el('div', { class: 'text' }, ...linkify(shownText(t.text))),
    el('div', { class: 'acts' },
      el('button', { class: `icon star${t.pri ? ' on' : ''}`, title: T('important'), onclick: () => patch(t, { pri: !t.pri }) }, t.pri ? '★' : '☆'),
      el('button', { class: 'icon', title: T('edit'), onclick: () => editRow(li, t) }, '✎'),
      del),
    meta);
  return li;
}

let player = null;
function playButton(t) {
  const label = () => `▶ ${t.audio_sec ? dur(t.audio_sec) : T('voice')}`;
  const b = el('button', { class: 'play', title: T('listen') }, label());
  b.onclick = async () => {
    if (player && player.dataset.id === String(t.id)) {
      player.paused ? player.play() : player.pause();
      return;
    }
    if (player) { player.pause(); URL.revokeObjectURL(player.src); }
    try {
      player = new Audio(await DB.audioURL(t.id));
    } catch (e) {
      return showStatus(T('cantLoad', e.message));
    }
    player.dataset.id = String(t.id);
    player.onplay = () => (b.textContent = T('playing'));
    player.onpause = player.onended = () => (b.textContent = label());
    player.play().catch((e) => showStatus(T('cantPlay', e.message)));
  };
  return b;
}

function editRow(li, t) {
  // A textarea keeps the lines (a task is often "text\nlink").
  const input = el('textarea', { rows: String(Math.min(6, t.text.split('\n').length + 1)) });
  input.value = t.text;
  const sel = el('select', {}, ...state.lists.map((l) => {
    const o = el('option', { value: String(l.id) }, l.name);
    o.selected = l.id === t.list_id;
    return o;
  }));
  const save = () => {
    const text = input.value.trim();
    if (!text) return;
    patch(t, { text, list_id: Number(sel.value) });
  };
  input.onkeydown = (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); save(); }
    if (e.key === 'Escape') render();
  };
  li.replaceChildren(el('div', { class: 'edit' }, input, sel,
    el('div', { class: 'row' },
      el('button', { class: 'primary', onclick: save }, T('save')),
      el('button', { onclick: render }, T('cancel')))));
  input.focus();
}

// ---- adding ----

$('addForm').onsubmit = async (e) => {
  e.preventDefault();
  if (rec) return finishRec(true);
  const text = $('newText').value.trim();
  if (!text) return;
  $('newText').value = '';
  try {
    await DB.req('POST', '/api/tasks', { text, list_id: state.list });
    load();
  } catch (err) {
    $('newText').value = text;
    showStatus(err.message);
  }
};

$('addList').onclick = () => {
  openSettings();
  setTimeout(() => $('newListName') && $('newListName').focus(), 0);
};

// ---- voice notes: MediaRecorder for the audio, optional Chrome speech
// recognition for live text that the user can correct before saving ----

let rec = null;

async function startRec() {
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: true });
  } catch (e) {
    // A side panel cannot show the permission prompt: ask in a tab once.
    showStatus(t('micNeeded'));
    chrome.tabs.create({ url: chrome.runtime.getURL('mic.html') });
    return;
  }
  const type = ['audio/webm;codecs=opus', 'audio/ogg;codecs=opus', 'audio/webm'].find((m) => MediaRecorder.isTypeSupported(m));
  const mr = new MediaRecorder(stream, type ? { mimeType: type } : {});
  const chunks = [];
  mr.ondataavailable = (e) => e.data.size && chunks.push(e.data);
  rec = { mr, stream, chunks, started: Date.now(), base: $('newText').value.trim(), sr: null, timer: null };
  mr.start(1000);

  const SR = state.stt && (window.SpeechRecognition || window.webkitSpeechRecognition);
  if (SR) {
    const sr = new SR();
    sr.lang = navigator.language || 'ru-RU';
    sr.continuous = true;
    sr.interimResults = true;
    let finalText = '';
    sr.onresult = (e) => {
      let interim = '';
      for (let i = e.resultIndex; i < e.results.length; i++) {
        const txt = e.results[i][0].transcript.trim();
        if (e.results[i].isFinal) finalText += (finalText ? ' ' : '') + txt;
        else interim += txt;
      }
      $('newText').value = [rec && rec.base, finalText, interim].filter(Boolean).join(' ');
    };
    sr.onerror = () => {};
    try { sr.start(); rec.sr = sr; } catch (_) {}
  }
  $('micBtn').classList.add('on');
  $('recBar').hidden = false;
  $('recTime').textContent = '0:00';
  rec.timer = setInterval(() => {
    const s = Math.floor((Date.now() - rec.started) / 1000);
    $('recTime').textContent = dur(s);
    if (s >= 10 * 60) finishRec(true); // keeps uploads well under the server limit
  }, 500);
}

function finishRec(save) {
  const r = rec;
  if (!r) return;
  rec = null;
  clearInterval(r.timer);
  if (r.sr) try { r.sr.stop(); } catch (_) {}
  $('micBtn').classList.remove('on');
  $('recBar').hidden = true;
  const sec = Math.max(1, Math.round((Date.now() - r.started) / 1000));
  r.mr.onstop = async () => {
    r.stream.getTracks().forEach((t) => t.stop());
    if (!save) {
      $('newText').value = r.base;
      return;
    }
    const blob = new Blob(r.chunks, { type: r.mr.mimeType || 'audio/webm' });
    const text = $('newText').value.trim();
    $('newText').value = '';
    const fd = new FormData();
    fd.append('text', text);
    fd.append('list_id', String(state.list));
    fd.append('audio_sec', String(sec));
    fd.append('audio', blob, blob.type.includes('ogg') ? 'voice.ogg' : 'voice.webm');
    try {
      await DB.req('POST', '/api/tasks', fd);
      load();
    } catch (e) {
      $('newText').value = text;
      showStatus(t('voiceNotSaved', e.message));
    }
  };
  r.mr.stop();
}

$('micBtn').onclick = () => (rec ? finishRec(true) : startRec());
$('recSave').onclick = () => finishRec(true);
$('recCancel').onclick = () => finishRec(false);

// ---- settings ----

async function openSettings() {
  const s = await chrome.storage.local.get(['server', 'token', 'stt']);
  $('sServer').value = s.server || '';
  $('sToken').value = s.token || '';
  $('sStt').checked = s.stt === true;
  $('sMsg').textContent = state.me ? t('signedIn', state.me.name) : '';
  renderListSettings();
  $('main').hidden = true;
  $('settings').hidden = false;
}

function closeSettings() {
  $('settings').hidden = true;
  $('main').hidden = false;
}

$('gear').onclick = () => ($('settings').hidden ? openSettings() : closeSettings());
$('sClose').onclick = closeSettings;

$('sSave').onclick = async () => {
  const raw = $('sServer').value.trim().replace(/\/+$/, '');
  let origin;
  try {
    const u = new URL(raw);
    if (!DB.safeServer(raw)) {
      $('sMsg').textContent = t('needHttps');
      return;
    }
    origin = `${u.origin}/*`;
  } catch (_) {
    $('sMsg').textContent = t('badServer');
    return;
  }
  // Must be the first await: Chrome only grants it inside the click.
  const granted = await chrome.permissions.request({ origins: [origin] });
  if (!granted) {
    $('sMsg').textContent = t('noAccess');
    return;
  }
  await chrome.storage.local.set({ server: raw, token: $('sToken').value.trim(), stt: $('sStt').checked });
  state.stt = $('sStt').checked;
  try {
    state.me = await DB.req('GET', '/api/me');
    $('sMsg').textContent = t('connected', state.me.name);
    connect();
    load();
  } catch (e) {
    $('sMsg').textContent = `❌ ${e.message}`;
  }
};

function renderListSettings() {
  const rows = state.lists.map((l) => {
    const name = el('input', { value: l.name });
    const color = el('input', { type: 'color', value: safeColor(l.color).length === 7 ? safeColor(l.color) : '#6366f1' });
    const upd = () => DB.req('PATCH', `/api/lists/${l.id}`, { name: name.value.trim(), color: color.value })
      .then(load).then(renderListSettings).catch((e) => ($('sMsg').textContent = e.message));
    name.onchange = upd;
    color.onchange = upd;
    const del = el('button', { class: 'icon', title: t('deleteList') }, '🗑');
    del.onclick = () => {
      const n = state.tasks.filter((t) => t.list_id === l.id).length;
      if (!del.classList.contains('confirm')) {
        del.classList.add('confirm');
        del.textContent = n ? t('deleteListN', n) : t('delConfirm');
        return;
      }
      DB.req('DELETE', `/api/lists/${l.id}`).then(load).then(renderListSettings).catch((e) => ($('sMsg').textContent = e.message));
    };
    return el('li', {}, color, name, del);
  });
  const nn = el('input', { id: 'newListName', placeholder: t('newListPh') });
  nn.onkeydown = (e) => {
    if (e.key !== 'Enter' || !nn.value.trim()) return;
    DB.req('POST', '/api/lists', { name: nn.value.trim() })
      .then((l) => { state.list = l.id; localStorage.setItem('list', l.id); })
      .then(load).then(renderListSettings).catch((err) => ($('sMsg').textContent = err.message));
  };
  $('sLists').replaceChildren(...rows, el('li', {}, nn));
}

// ---- start ----

applyI18n();

(async () => {
  const s = await chrome.storage.local.get(['server', 'token', 'stt']);
  state.stt = s.stt === true;
  if (!s.server || !s.token) {
    openSettings();
    $('sMsg').textContent = t('setup');
    return;
  }
  DB.req('GET', '/api/me').then((me) => (state.me = me)).catch(() => {});
  await load();
  connect();
})();
