// Shared by the side panel and the service worker: settings and server calls.
// The token is only ever sent in the Authorization header, never in a URL.
'use strict';

const DB = {
  async cfg() {
    const s = await chrome.storage.local.get(['server', 'token']);
    const server = (s.server || '').trim().replace(/\/+$/, '');
    return { server: DB.safeServer(server) ? server : '', token: (s.token || '').trim() };
  },

  // The token may only go to https://, or http:// on this computer.
  safeServer(raw) {
    try {
      const u = new URL(raw);
      return u.protocol === 'https:' || (u.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(u.hostname));
    } catch (_) {
      return false;
    }
  },

  // req(method, path, body): body is an object (sent as JSON) or FormData.
  async req(method, path, body) {
    const { server, token } = await DB.cfg();
    if (!server || !token) throw new Error(t('configure'));
    const opts = { method, headers: { Authorization: `Bearer ${token}` } };
    if (body instanceof FormData) opts.body = body;
    else if (body !== undefined) {
      opts.body = JSON.stringify(body);
      opts.headers['Content-Type'] = 'application/json';
    }
    let r;
    try {
      r = await fetch(server + path, opts);
    } catch (e) {
      throw new Error(t('unreachable', e.message));
    }
    if (r.status === 204) return null;
    const data = await r.json().catch(() => null);
    if (!r.ok) throw new Error((data && data.error) || `HTTP ${r.status}`);
    return data;
  },

  // A recording as an object URL (fetched with the header).
  async audioURL(id) {
    const { server, token } = await DB.cfg();
    if (!server || !token) throw new Error(t('configure'));
    const r = await fetch(`${server}/api/tasks/${id}/audio`, { headers: { Authorization: `Bearer ${token}` } });
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    return URL.createObjectURL(await r.blob());
  },

  // events(onOpen, onEvent, onError) reads the Server-Sent Events stream with
  // fetch (EventSource cannot send headers) and reconnects with backoff;
  // returns stop().
  events(onOpen, onEvent, onError) {
    let stopped = false, ctrl = null, delay = 1000;
    (async () => {
      while (!stopped) {
        try {
          const { server, token } = await DB.cfg();
          if (!server || !token) throw new Error('not configured');
          ctrl = new AbortController();
          const r = await fetch(`${server}/api/events`, { headers: { Authorization: `Bearer ${token}` }, signal: ctrl.signal });
          if (!r.ok || !r.body) throw new Error(`HTTP ${r.status}`);
          onOpen();
          delay = 1000;
          const reader = r.body.pipeThrough(new TextDecoderStream()).getReader();
          let buf = '';
          for (;;) {
            const { value, done } = await reader.read();
            if (done) break;
            buf += value;
            let i;
            while ((i = buf.indexOf('\n\n')) >= 0) {
              const block = buf.slice(0, i);
              buf = buf.slice(i + 2);
              const data = block.split('\n').filter((l) => l.startsWith('data: ')).map((l) => l.slice(6)).join('\n');
              if (data) onEvent(data);
            }
          }
          throw new Error('stream closed');
        } catch (e) {
          if (stopped) return;
          onError(e);
          await new Promise((res) => setTimeout(res, delay));
          delay = Math.min(delay * 2, 30000);
        }
      }
    })();
    return () => {
      stopped = true;
      if (ctrl) ctrl.abort();
    };
  },
};
