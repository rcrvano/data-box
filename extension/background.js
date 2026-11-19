// data-box service worker: toolbar button → side panel, "add to tasks"
// context menu, and the open-tasks count on the icon.
'use strict';
importScripts('i18n.js', 'api.js');

chrome.sidePanel.setPanelBehavior({ openPanelOnActionClick: true }).catch(() => {});

// Menu: one item per list (the panel caches lists in storage).
async function buildMenus() {
  const { lists = [] } = await chrome.storage.local.get('lists');
  await chrome.contextMenus.removeAll();
  const ctx = ['selection', 'link', 'page'];
  if (lists.length <= 1) {
    chrome.contextMenus.create({ id: `task:${lists[0] ? lists[0].id : 0}`, title: t('addToTasks'), contexts: ctx });
    return;
  }
  chrome.contextMenus.create({ id: 'root', title: t('addToTasks'), contexts: ctx });
  for (const l of lists) chrome.contextMenus.create({ id: `task:${l.id}`, parentId: 'root', title: l.name, contexts: ctx });
}

function notify(title, message) {
  chrome.notifications.create({ type: 'basic', iconUrl: 'icons/icon128.png', title, message, priority: 0 });
}

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (!String(info.menuItemId).startsWith('task:')) return;
  const listId = Number(String(info.menuItemId).slice(5)) || 0;
  const what = (info.selectionText || info.linkText || (tab && tab.title) || '').trim().replace(/\s+/g, ' ').slice(0, 500);
  // Only web addresses go to the server (not chrome://, file://, …).
  const url = [info.linkUrl, tab && tab.url].find((u) => /^https?:\/\//i.test(u || '')) || '';
  const text = [what, url].filter(Boolean).join('\n');
  try {
    await DB.req('POST', '/api/tasks', { text, list_id: listId });
    notify('data-box', t('taskAdded', what.slice(0, 80)));
    refreshBadge();
  } catch (e) {
    notify(t('error'), String(e.message || e));
  }
});

async function refreshBadge() {
  try {
    const tasks = await DB.req('GET', '/api/tasks');
    const n = tasks.filter((t) => !t.done_at).length;
    chrome.action.setBadgeBackgroundColor({ color: '#6366f1' });
    chrome.action.setBadgeText({ text: n ? String(n) : '' });
  } catch (_) {
    chrome.action.setBadgeText({ text: '' });
  }
}

chrome.runtime.onInstalled.addListener(() => {
  buildMenus();
  chrome.alarms.create('badge', { periodInMinutes: 5 });
  refreshBadge();
});
chrome.runtime.onStartup.addListener(() => {
  buildMenus();
  refreshBadge();
});
chrome.alarms.onAlarm.addListener((a) => {
  if (a.name === 'badge') refreshBadge();
});
chrome.storage.onChanged.addListener((ch) => {
  if (ch.lists) buildMenus();
  if (ch.server || ch.token) refreshBadge();
});
chrome.runtime.onMessage.addListener((m) => {
  if (m && m.type === 'badge') refreshBadge();
});
