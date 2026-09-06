/*!
 * app.js — basic-kanban-board-mcp client glue.
 *
 * The page ships htmx v4, its SSE extension, Alpine.js, and SortableJS as
 * vendored files in web/static/vendor/. This file is intentionally tiny and
 * is the ONLY custom JavaScript. It does five things, in order:
 *
 *   1. Applies the saved theme (light/dark) before Alpine boots, so the page
 *      does not flash the wrong palette.
 *   2. Wires the manual theme toggle on the topbar.
 *   3. Re-connects SSE feeds dropped by network blips (htmx's `sse-swap`
 *      extension handles reconnects internally; this is a backstop for the
 *      event-list fallback).
 *   4. Initialises Sortable on every column that opts in via
 *      `data-sortable` and POSTs the new order to the move endpoint.
 *   5. Wires the copy-to-clipboard buttons on the agent-setup page.
 *
 * No inline scripts; no eval; CSP-safe.
 */
(function () {
  'use strict';

  // -- 1. theme ---------------------------------------------------------
  var STORAGE_KEY = 'kanban.theme';
  function currentTheme() {
    var saved = null;
    try { saved = localStorage.getItem(STORAGE_KEY); } catch (e) { /* private mode */ }
    if (saved === 'light' || saved === 'dark') return saved;
    if (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) return 'dark';
    return 'light';
  }
  function applyTheme(t) {
    document.documentElement.setAttribute('data-theme', t);
    var btn = document.querySelector('[data-theme-toggle]');
    if (btn) {
      btn.setAttribute('aria-pressed', t === 'dark' ? 'true' : 'false');
      btn.setAttribute('title', t === 'dark' ? 'Switch to light theme' : 'Switch to dark theme');
    }
  }
  applyTheme(currentTheme());

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-theme-toggle]');
    if (!btn) return;
    var next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
    try { localStorage.setItem(STORAGE_KEY, next); } catch (err) { /* ignore */ }
    applyTheme(next);
  });

  // -- 2. SSE reconnect backstop ----------------------------------------
  // htmx's `sse` extension reconnects on its own; we only watch for the
  // `htmx:sseError` event and retry the connection by hand if it stays
  // closed for more than a few seconds, so the activity feed survives a
  // brief proxy restart.
  document.addEventListener('htmx:sseError', function (e) {
    var el = e.target;
    if (!el || !el.getAttribute) return;
    var src = el.getAttribute('sse-connect');
    if (!src) return;
    var tries = parseInt(el.getAttribute('data-sse-retries') || '0', 10);
    if (tries > 5) return;
    el.setAttribute('data-sse-retries', String(tries + 1));
    setTimeout(function () {
      if (window.htmx) window.htmx.trigger(el, 'sse-reconnect');
    }, 2000 * Math.min(tries + 1, 6));
  });

  // -- 3. sortables ------------------------------------------------------
  function initSortables(root) {
    if (!window.Sortable) return;
    var scope = root || document;
    var cols = scope.querySelectorAll('[data-sortable]');
    for (var i = 0; i < cols.length; i++) {
      var col = cols[i];
      if (col.__sortable) continue;
      var url = col.getAttribute('data-sortable-url');
      if (!url) continue;
      var placeholder = (col.getAttribute('data-sortable-placeholder') || 'task-card-placeholder').trim() || null;
      col.__sortable = window.Sortable.create(col, {
        group: col.getAttribute('data-sortable-group') || 'cards',
        animation: 120,
        ghostClass: 'sortable-ghost',
        chosenClass: 'sortable-chosen',
        dragClass: 'sortable-drag',
        forceFallback: false,
        emptyInsertThreshold: 12,
        onEnd: function (evt) {
          var card = evt.item;
          var key = card && card.getAttribute && card.getAttribute('data-task-key');
          var toCol = evt.to;
          var toColumnName = toCol && toCol.getAttribute && toCol.getAttribute('data-column-name');
          if (!key || !toColumnName) return;
          var body = 'key=' + encodeURIComponent(key) +
                     '&column=' + encodeURIComponent(toColumnName) +
                     '&position=' + encodeURIComponent(evt.newIndex < (evt.oldIndex) ? 'top' : 'bottom');
          var csrf = document.querySelector('meta[name="csrf-token"]');
          var headers = { 'Content-Type': 'application/x-www-form-urlencoded' };
          if (csrf) headers['X-CSRF-Token'] = csrf.getAttribute('content');
          fetch(url, { method: 'POST', headers: headers, body: body, credentials: 'same-origin' })
            .then(function (r) {
              if (!r.ok) {
                toast('Move failed: ' + r.status, 'error');
                if (evt.from && evt.from.insertBefore) evt.from.insertBefore(card, evt.from.children[evt.oldIndex]);
              }
            })
            .catch(function () {
              toast('Move failed: network error', 'error');
              if (evt.from && evt.from.insertBefore) evt.from.insertBefore(card, evt.from.children[evt.oldIndex]);
            });
        }
      });
    }
  }

  // -- 4. copy-to-clipboard ---------------------------------------------
  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text; ta.setAttribute('readonly', ''); ta.style.position = 'fixed'; ta.style.left = '-9999px';
      document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); resolve(); } catch (e) { reject(e); }
      document.body.removeChild(ta);
    });
  }
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-copy]');
    if (!btn) return;
    e.preventDefault();
    var text = btn.getAttribute('data-copy');
    copyText(text).then(function () {
      btn.classList.add('copied');
      var orig = btn.textContent;
      btn.textContent = 'Copied';
      setTimeout(function () { btn.textContent = orig; btn.classList.remove('copied'); }, 1200);
    }).catch(function () { toast('Copy failed', 'error'); });
  });

  // -- 5. toasts --------------------------------------------------------
  function toast(msg, kind) {
    var region = document.querySelector('.toast-region');
    if (!region) {
      region = document.createElement('div');
      region.className = 'toast-region';
      region.setAttribute('role', 'status');
      region.setAttribute('aria-live', 'polite');
      document.body.appendChild(region);
    }
    var el = document.createElement('div');
    el.className = 'toast' + (kind ? ' ' + kind : '');
    el.textContent = msg;
    region.appendChild(el);
    setTimeout(function () {
      el.style.opacity = '0';
      el.style.transition = 'opacity 200ms';
      setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, 220);
    }, 3500);
  }
  window.kanbanToast = toast;

  // -- 6. drawer's "move" menu (keyboard alt to drag) ------------------
  function initMoveMenu() {
    document.addEventListener('change', function (e) {
      var sel = e.target.closest && e.target.closest('[data-move-menu]');
      if (!sel) return;
      var card = sel.closest && sel.closest('.card');
      if (!card) return;
      var key = card.getAttribute('data-task-key');
      var col = sel.value;
      sel.value = '';
      if (!key || !col) return;
      var csrf = document.querySelector('meta[name="csrf-token"]');
      var headers = { 'Content-Type': 'application/x-www-form-urlencoded' };
      if (csrf) headers['X-CSRF-Token'] = csrf.getAttribute('content');
      fetch(sel.getAttribute('data-move-url') || '/fragments/move', {
        method: 'POST', headers: headers,
        body: 'key=' + encodeURIComponent(key) + '&column=' + encodeURIComponent(col),
        credentials: 'same-origin'
      }).then(function (r) {
        if (r.ok) window.location.reload();
        else toast('Move failed: ' + r.status, 'error');
      }).catch(function () { toast('Move failed: network error', 'error'); });
    });
  }

  // -- boot -------------------------------------------------------------
  function boot() {
    initSortables();
    initMoveMenu();
    // Re-init after htmx swaps content into the page.
    document.body.addEventListener('htmx:afterSwap', function () { initSortables(); });
    document.body.addEventListener('htmx:sseMessage', function (e) {
      if (e.detail && e.detail.type === 'resync') window.location.reload();
    });
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
