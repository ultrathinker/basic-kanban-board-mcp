/*!
 * app.js — basic-kanban-board-mcp client glue.
 *
 * The only custom JavaScript on the page. No inline scripts, no eval, no
 * inline event handlers: the CSP is `script-src 'self'`, so everything is
 * wired here by data- attribute.
 *
 * It does six things:
 *   1. Applies the saved theme before paint, and wires the topbar toggle.
 *   2. Live updates: one EventSource per page, which refreshes the marked
 *      live regions from the server. See the LIVE UPDATES note below.
 *      KANB-19: a static "Update" link (data-live-refresh) runs that exact
 *      same fetch-and-swap immediately, for anyone who wants a refresh right
 *      now rather than waiting on the next SSE event.
 *   3. Drag between columns (SortableJS), posting key + column + position +
 *      if_version.
 *   4. The keyboard move menu — same POST, no mouse required.
 *   5. Acceptance checkboxes on the task page.
 *   6. Copy buttons, and toasts for everything that can fail.
 *
 * LIVE UPDATES — why this is not htmx's sse extension.
 *
 * The vendored htmx is v4, which has no extension mechanism at all: it
 * exposes neither `htmx.defineExtension` nor `htmx.createEventSource`, and
 * `hx-ext` is not one of its attributes. The vendored sse extension is
 * built against htmx 2's API, so `hx-ext="sse"` was inert and loading the
 * extension threw on every page. Separately, /events emits JSON, not HTML
 * fragments, so `sse-swap` would have printed JSON into the page.
 *
 * So SSE is used here as a CHANGE SIGNAL, not as a transport for markup:
 * an event arrives, and the page re-fetches its own URL and swaps the
 * server-rendered live regions in. The HTML always comes from the same
 * template that rendered the page, so there is exactly one definition of a
 * card — no second renderer in JavaScript to drift out of sync.
 */
(function () {
  'use strict';

  var LIVE_DEBOUNCE_MS = 300;
  var MAX_SSE_FAILURES = 10;

  // -- small helpers -----------------------------------------------------

  function csrfToken() {
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.getAttribute('content') || '' : '';
  }

  function postForm(url, fields) {
    var body = Object.keys(fields)
      .filter(function (k) { return fields[k] !== null && fields[k] !== undefined && fields[k] !== ''; })
      .map(function (k) { return encodeURIComponent(k) + '=' + encodeURIComponent(fields[k]); })
      .join('&');
    var headers = { 'Content-Type': 'application/x-www-form-urlencoded' };
    var csrf = csrfToken();
    if (csrf) headers['X-CSRF-Token'] = csrf;
    return fetch(url, { method: 'POST', headers: headers, body: body, credentials: 'same-origin' });
  }

  // errorMessage pulls the server's remediation text out of the standard
  // error envelope so a failed move says which rule refused it, rather than
  // just showing a status code. "Fail loud" applies to the UI too.
  function errorMessage(response, fallback) {
    return response.json().then(function (payload) {
      var err = payload && payload.error;
      if (!err) return fallback;
      var msg = err.message || fallback;
      if (err.remediation) msg += ' — ' + err.remediation;
      return msg;
    }).catch(function () { return fallback; });
  }

  function toast(msg, kind) {
    var region = document.getElementById('toasts') || document.querySelector('.toast-region');
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
    setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, 6000);
  }
  window.kanbanToast = toast;

  // -- 1. theme ----------------------------------------------------------

  var STORAGE_KEY = 'kanban.theme';

  function savedTheme() {
    try {
      var v = localStorage.getItem(STORAGE_KEY);
      if (v === 'light' || v === 'dark') return v;
    } catch (e) { /* private mode, blocked storage */ }
    return null;
  }

  function systemTheme() {
    return (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) ? 'dark' : 'light';
  }

  function applyTheme(t) {
    document.documentElement.setAttribute('data-theme', t);
    var btn = document.querySelector('[data-theme-toggle]');
    if (btn) {
      btn.setAttribute('aria-pressed', t === 'dark' ? 'true' : 'false');
      btn.setAttribute('title', t === 'dark' ? 'Switch to the light theme' : 'Switch to the dark theme');
    }
  }

  // Applied at parse time (the script is deferred but still runs before
  // first paint of the body in practice) so the palette does not flash.
  applyTheme(savedTheme() || systemTheme());

  document.addEventListener('click', function (e) {
    var btn = e.target.closest && e.target.closest('[data-theme-toggle]');
    if (!btn) return;
    var next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
    try { localStorage.setItem(STORAGE_KEY, next); } catch (err) { /* ignore */ }
    applyTheme(next);
  });

  // -- 2. live updates ---------------------------------------------------

  var live = {
    source: null,
    failures: 0,
    timer: null,
    announceTimer: null,
    inFlight: false,
    dragging: false,
    // dirtyChartKeys accumulates progressBarKey()-shaped keys for metrics a
    // progress.recorded/progress.track_deleted SSE event named, between now
    // and the next refreshLiveRegions pass — see markProgressEventDirty and
    // captureProgressState's own comment for why an open chart on one of
    // these keys must not be restored from its cache.
    dirtyChartKeys: {}
  };

  function setLiveState(state, label) {
    var els = document.querySelectorAll('[data-live-status]');
    for (var i = 0; i < els.length; i++) {
      els[i].setAttribute('data-state', state);
      var text = els[i].querySelector('[data-live-label]');
      if (text) text.textContent = label;
    }
  }

  // announceUpdate is the accessible half of a live swap. The swapped
  // regions are not aria-live themselves — re-reading a whole board to a
  // screen reader on every agent write is unusable — so the one small
  // status element carries the announcement instead.
  function announceUpdate() {
    if (live.source === null) return;
    setLiveState('open', 'updated');
    if (live.announceTimer) clearTimeout(live.announceTimer);
    live.announceTimer = setTimeout(function () { setLiveState('open', 'live'); }, 2500);
  }

  function liveRegions() {
    var out = [];
    var decls = document.querySelectorAll('[data-live-region]');
    for (var i = 0; i < decls.length; i++) {
      var sel = decls[i].getAttribute('data-live-region');
      var el = sel && document.querySelector(sel);
      if (el) out.push(el);
    }
    return out;
  }

  // markNewRows flags the activity rows that were not in the feed before the
  // swap, so the enter animation only plays for genuinely new events.
  function markNewRows(region, previousFirstRowText) {
    if (!region.classList.contains('activity')) return;
    var rows = region.querySelectorAll('.row');
    for (var i = 0; i < rows.length && i < 20; i++) {
      if (previousFirstRowText !== null && rows[i].textContent.trim() === previousFirstRowText) break;
      rows[i].classList.add('new');
    }
  }

  // progressBarKey/trackKey/captureProgressState/restoreProgressState — KANB-12.
  //
  // A live region such as #board or #project-progress can contain several
  // independent progress bars (one per card, plus the project's own two).
  // Blindly replacing that region's innerHTML — which is exactly what the
  // block below does, and rightly so: it is the one swap mechanism this page
  // uses — throws away two kinds of per-bar UI state the server does not
  // know about at all: an open history chart (KANB-13) and an armed
  // delete-track confirmation (section 8 above). Neither is reflected in the
  // freshly rendered HTML (a bar's chart container always renders closed;
  // a track row never renders pre-armed), so without this, a progress
  // assessment landing on ANY task would silently close the chart the owner
  // has open on a DIFFERENT one, or cancel a confirmation they were about to
  // click through — exactly the "yanked view" this feature must not cause.
  //
  // The fix is to capture what was open, by identity (project/task/assessor
  // — stable across a re-render, unlike any DOM node reference), before the
  // swap, then restore it into the freshly swapped-in markup afterwards. The
  // chart's restore uses the HTML already fetched for it rather than firing
  // a second request: it was fetched once under the "fetch once, cache in
  // the DOM" contract fetchProgressChart documents, and the swap just threw
  // that cached DOM node away, not the fact that it was already fetched.
  //
  // The one case that must NOT reuse the cache: the metric an open chart is
  // itself scoped to just recorded a new mark or lost a track (see
  // markProgressEventDirty below). The whole reason to open a chart is
  // watching the descent; restoring yesterday's cached SVG onto a bar whose
  // percent just changed would silently hide the one point the owner opened
  // it to see. captureProgressState/restoreProgressState drop the cache and
  // fetch fresh instead, but ONLY for a bar named in live.dirtyChartKeys — an
  // open chart on an unrelated task/project is left on its cheap cached copy
  // exactly as before.
  function progressBarKey(bar) {
    return (bar.getAttribute('data-project') || '') + ' ' + (bar.getAttribute('data-task') || '');
  }

  // currentLiveProjectKey reads the project key straight out of the page's
  // own SSE source URL ("/events/p/BMB" -> "BMB") rather than a second
  // server-provided data attribute: every progress bar on a board page
  // already scopes its own subscription to exactly one project, so this is
  // the same key data-project already carries on every bar here, just read
  // from the one place that exists regardless of whether any bar is on the
  // page at all.
  function currentLiveProjectKey() {
    var decl = document.querySelector('[data-live-source]');
    var url = decl && decl.getAttribute('data-live-source');
    var m = url && /\/events\/p\/([^/?]+)/.exec(url);
    return m ? m[1] : '';
  }

  // markProgressEventDirty reads one incoming SSE frame's JSON payload (a
  // domain.Event) and, if it is a progress.recorded or progress.track_deleted
  // event, records the metric it names in live.dirtyChartKeys so the next
  // refreshLiveRegions pass knows not to trust that one chart's cache. Any
  // other event type, or a payload that fails to parse, is ignored here —
  // scheduleRefresh runs regardless either way; this only tracks which
  // chart(s), if any, must be treated as stale by that refresh.
  function markProgressEventDirty(e) {
    var ev;
    try { ev = JSON.parse(e.data); } catch (err) { return; }
    if (!ev || (ev.Type !== 'progress.recorded' && ev.Type !== 'progress.track_deleted')) return;
    var project = currentLiveProjectKey();
    if (!project) return;
    var taskKey = (ev.Payload && ev.Payload.key) || '';
    live.dirtyChartKeys[project + ' ' + taskKey] = true;
  }

  function trackKey(row) {
    return (row.getAttribute('data-project') || '') + ' ' +
      (row.getAttribute('data-task') || '') + ' ' +
      (row.getAttribute('data-assessor') || '');
  }

  function captureProgressState(root) {
    var openCharts = {};
    var staleCharts = {};
    var bars = root.querySelectorAll('[data-progress-chart-toggle][aria-expanded="true"]');
    for (var i = 0; i < bars.length; i++) {
      var key = progressBarKey(bars[i]);
      var container = progressChartContainer(bars[i]);
      if (!container || container.childElementCount === 0) continue;
      if (live.dirtyChartKeys[key]) {
        // This exact metric just recorded a mark or lost a track: the
        // cached SVG is now showing stale data, so it must not be restored.
        // staleCharts remembers only that the chart WAS open, so the
        // restore step below re-opens it and fetches a fresh copy instead.
        staleCharts[key] = true;
      } else {
        openCharts[key] = container.innerHTML;
      }
    }
    var armed = {};
    var rows = root.querySelectorAll('.progress-track.is-confirming');
    for (var j = 0; j < rows.length; j++) {
      armed[trackKey(rows[j])] = true;
    }
    return { openCharts: openCharts, staleCharts: staleCharts, armed: armed };
  }

  function restoreProgressState(root, state) {
    var bars = root.querySelectorAll('[data-progress-chart-toggle]');
    for (var i = 0; i < bars.length; i++) {
      var key = progressBarKey(bars[i]);
      var container = progressChartContainer(bars[i]);
      if (!container) continue;
      if (state.staleCharts[key]) {
        // Reopen against an empty container (the fresh markup's own chart
        // container always renders empty) so fetchProgressChart's "already
        // filled" guard does not treat this as already current, then fetch
        // the real, up-to-date chart the same way a first click would.
        setProgressChartOpen(bars[i], container, true);
        fetchProgressChart(bars[i], container);
        continue;
      }
      var html = state.openCharts[key];
      if (html === undefined) continue;
      container.innerHTML = html;
      setProgressChartOpen(bars[i], container, true);
    }
    var rows = root.querySelectorAll('[data-progress-track]');
    for (var j = 0; j < rows.length; j++) {
      if (state.armed[trackKey(rows[j])]) rows[j].classList.add('is-confirming');
    }
  }

  // appendNewChatEntries is the thoughts-feed's own swap strategy — never a
  // blind innerHTML replace like the generic path below. fresh is the
  // freshly fetched document's counterpart of the live <ol>; every entry in
  // it not already present (matched by data-chat-id, the message's own,
  // stable id) is handed to appendChatEntry ONE AT A TIME, in order, so each
  // one gets the real append-then-follow-or-mark behaviour that function
  // implements instead of a wholesale re-render that would reset scroll and
  // re-trigger every entry as "new".
  function appendNewChatEntries(fresh) {
    var feed = chatFeedEl();
    if (!feed) return;
    var known = {};
    var existing = feed.querySelectorAll('[data-chat-id]');
    for (var i = 0; i < existing.length; i++) {
      known[existing[i].getAttribute('data-chat-id')] = true;
    }
    var incoming = fresh.querySelectorAll('[data-chat-id]');
    for (var j = 0; j < incoming.length; j++) {
      var id = incoming[j].getAttribute('data-chat-id');
      if (!id || known[id]) continue;
      appendChatEntry(document.importNode(incoming[j], true));
    }
  }

  function refreshLiveRegions() {
    if (live.inFlight || live.dragging) return;
    var regions = liveRegions();
    if (regions.length === 0) return;
    live.inFlight = true;
    fetch(window.location.href, {
      credentials: 'same-origin',
      headers: { 'Accept': 'text/html', 'X-Live-Refresh': '1' }
    })
      .then(function (r) {
        if (!r.ok) throw new Error('status ' + r.status);
        return r.text();
      })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, 'text/html');
        for (var i = 0; i < regions.length; i++) {
          var current = regions[i];
          var fresh = current.id ? doc.getElementById(current.id) : null;
          if (!fresh) continue;
          // The thoughts feed never gets the generic wholesale swap: see
          // appendNewChatEntries's own comment for why.
          if (current.hasAttribute('data-chat-feed')) {
            appendNewChatEntries(fresh);
            continue;
          }
          var firstRow = current.querySelector('.row');
          var previousFirstRowText = firstRow ? firstRow.textContent.trim() : null;
          var progressState = captureProgressState(current);
          current.innerHTML = fresh.innerHTML;
          restoreProgressState(current, progressState);
          markNewRows(current, previousFirstRowText);
        }
        // Every region has now either restored or refetched every chart
        // markProgressEventDirty flagged since the last refresh: the flags
        // are one-shot, so clear them rather than carrying them into the
        // NEXT refresh and refetching a chart a second time for nothing.
        live.dirtyChartKeys = {};
        initSortables();
        announceUpdate();
      })
      .catch(function () { /* a failed refresh is not worth a toast; SSE will fire again */ })
      .then(function () { live.inFlight = false; });
  }

  function scheduleRefresh() {
    if (live.timer) clearTimeout(live.timer);
    live.timer = setTimeout(function () {
      live.timer = null;
      refreshLiveRegions();
    }, LIVE_DEBOUNCE_MS);
  }

  function startLive() {
    var decl = document.querySelector('[data-live-source]');
    if (!decl || typeof window.EventSource !== 'function') return;
    var url = decl.getAttribute('data-live-source');
    if (!url) return;

    setLiveState('connecting', 'connecting');
    var es = new EventSource(url, { withCredentials: true });
    live.source = es;

    es.addEventListener('open', function () {
      live.failures = 0;
      setLiveState('open', 'live');
    });

    // The server names every event three ways: its own domain.EventType, and
    // the "board-update" / "activity-feed" aliases. Listening to the two
    // aliases covers every task/project event without enumerating them.
    //
    // markProgressEventDirty inspects the frame's own JSON payload before
    // scheduling the refresh every event triggers regardless: a
    // progress.recorded/progress.track_deleted frame additionally flags its
    // metric so a chart the owner has open on that exact metric is refetched
    // rather than restored from its (now stale) cache — see that function's
    // own comment.
    ['board-update', 'activity-feed'].forEach(function (name) {
      es.addEventListener(name, function (e) {
        markProgressEventDirty(e);
        scheduleRefresh();
      });
    });

    // A resync sentinel means the replay buffer could not cover the gap, so
    // the page's own state is untrustworthy: reload rather than patch.
    es.addEventListener('resync', function () { window.location.reload(); });

    es.addEventListener('shutdown', function () {
      setLiveState('closed', 'server stopped');
      es.close();
      live.source = null;
    });

    es.onerror = function () {
      if (es.readyState === EventSource.CLOSED) {
        live.failures++;
        if (live.failures >= MAX_SSE_FAILURES) {
          setLiveState('closed', 'offline');
          es.close();
          live.source = null;
          toast('Live updates stopped. Reload the page to reconnect.', 'error');
          return;
        }
      }
      setLiveState('connecting', 'reconnecting');
    };
  }

  // initLiveRefresh wires the static "Update" link (KANB-19's replacement
  // for the visible LIVE indicator): a click runs the exact same
  // fetch-and-swap refreshLiveRegions() runs on an SSE event, immediately
  // instead of on the next debounced tick. preventDefault stops the browser
  // from also following the link's own href — that href is the progressive-
  // enhancement fallback for when this script never runs at all, and must
  // stay a plain link to the current page for that case.
  function initLiveRefresh() {
    document.addEventListener('click', function (e) {
      var link = e.target.closest && e.target.closest('[data-live-refresh]');
      if (!link) return;
      e.preventDefault();
      refreshLiveRegions();
    });
  }

  // -- 3. drag between columns -------------------------------------------

  function moveTask(card, columnName, position) {
    var key = card.getAttribute('data-task-key');
    var version = card.getAttribute('data-version');
    if (!key || !columnName) return Promise.resolve(false);
    // if_version is what makes this a real optimistic-concurrency write
    // rather than a read-then-write: the card carries the version it was
    // rendered with, and the server refuses the move if anything else
    // changed the task in the meantime.
    return postForm(card.getAttribute('data-move-url') || '/fragments/move', {
      key: key,
      column: columnName,
      position: position || 'bottom',
      if_version: version
    }).then(function (r) {
      if (r.ok) {
        scheduleRefresh();
        return true;
      }
      return errorMessage(r, 'Move failed (' + r.status + ')').then(function (msg) {
        toast(msg, 'error');
        return false;
      });
    });
  }

  function initSortables(root) {
    if (!window.Sortable) return;
    var scope = root || document;
    var lists = scope.querySelectorAll('[data-sortable]');
    for (var i = 0; i < lists.length; i++) {
      var list = lists[i];
      if (list.__sortable) continue;
      var url = list.getAttribute('data-sortable-url');
      if (!url) continue;
      list.__sortable = window.Sortable.create(list, {
        group: list.getAttribute('data-sortable-group') || 'cards',
        draggable: '.card',
        animation: 120,
        ghostClass: 'sortable-ghost',
        chosenClass: 'sortable-chosen',
        dragClass: 'sortable-drag',
        emptyInsertThreshold: 12,
        onStart: function () { live.dragging = true; },
        onEnd: function (evt) {
          live.dragging = false;
          var card = evt.item;
          var to = evt.to;
          var columnName = to && to.getAttribute && to.getAttribute('data-column-name');
          if (!columnName) return;
          if (evt.from === evt.to && evt.oldIndex === evt.newIndex) return;
          // The service ranks by "top" or "bottom" only, so anything that
          // did not land first is a bottom insert.
          var position = evt.newIndex === 0 ? 'top' : 'bottom';
          moveTask(card, columnName, position).then(function (ok) {
            if (ok || !evt.from || !evt.from.insertBefore) return;
            // Put the card back where it came from; the server rejected it.
            var siblings = evt.from.children;
            evt.from.insertBefore(card, siblings[evt.oldIndex] || null);
          });
        }
      });
    }
  }

  // -- 4. keyboard move menu ---------------------------------------------

  function initMoveMenu() {
    document.addEventListener('change', function (e) {
      var sel = e.target.closest && e.target.closest('[data-move-menu]');
      if (!sel) return;
      var card = sel.closest('.card');
      var column = sel.value;
      sel.value = '';
      if (!card || !column) return;
      var url = sel.getAttribute('data-move-url') || '/fragments/move';
      card.setAttribute('data-move-url', url);
      moveTask(card, column, 'bottom').then(function (ok) {
        if (ok) toast(card.getAttribute('data-task-key') + ' → ' + column);
      });
    });
  }

  // -- 5. acceptance checkboxes ------------------------------------------

  function initAcceptance() {
    document.addEventListener('change', function (e) {
      var box = e.target.closest && e.target.closest('[data-acceptance]');
      if (!box) return;
      var wanted = box.checked;
      postForm('/fragments/acceptance', {
        key: box.getAttribute('data-task-key'),
        index: box.getAttribute('data-index'),
        done: wanted ? 'true' : 'false'
      }).then(function (r) {
        if (r.ok) {
          var label = document.querySelector('label[for="' + box.id + '"]');
          if (label) label.classList.toggle('done', wanted);
          return;
        }
        box.checked = !wanted;
        return errorMessage(r, 'Could not update that item (' + r.status + ')')
          .then(function (msg) { toast(msg, 'error'); });
      }).catch(function () {
        box.checked = !wanted;
        toast('Could not update that item: network error', 'error');
      });
    });
  }

  // -- 6. copy buttons + auto-submitting selects -------------------------

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.left = '-9999px';
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand('copy'); resolve(); } catch (err) { reject(err); }
      document.body.removeChild(ta);
    });
  }

  function initCopy() {
    document.addEventListener('click', function (e) {
      var btn = e.target.closest && e.target.closest('[data-copy]');
      if (!btn) return;
      e.preventDefault();
      copyText(btn.getAttribute('data-copy')).then(function () {
        var original = btn.textContent;
        btn.textContent = 'Copied';
        setTimeout(function () { btn.textContent = original; }, 1200);
      }).catch(function () {
        toast('Copy failed — select the text and copy manually', 'error');
      });
    });
  }

  // -- 6.1 dialogs (prompts, new project) --------------------------------

  // Native <dialog> popups, opened from a topbar link by data-dialog-open="#id"
  // and closed by a data-dialog-close button, the Escape key (free with
  // <dialog>), or a click on the backdrop. The opener is a real <a href="…">,
  // so a browser without <dialog>.showModal (or with JS off) just follows the
  // link to the same content rendered on a page.
  function initDialog() {
    document.addEventListener('click', function (e) {
      var opener = e.target.closest && e.target.closest('[data-dialog-open]');
      if (opener) {
        var sel = opener.getAttribute('data-dialog-open');
        var dlg = sel && document.querySelector(sel);
        if (dlg && typeof dlg.showModal === 'function') {
          e.preventDefault();
          dlg.showModal();
        }
        return;
      }
      var closer = e.target.closest && e.target.closest('[data-dialog-close]');
      if (closer) {
        var owner = closer.closest('dialog');
        if (owner) { e.preventDefault(); owner.close(); }
        return;
      }
      // A click whose target is the <dialog> element itself lands on the
      // backdrop (content sits in inner elements), so it closes the dialog.
      var open = e.target.closest && e.target.closest('dialog[open]');
      if (open && e.target === open) open.close();
    });
  }

  // The project switcher used to carry onchange="this.form.submit()", which
  // the CSP blocks: the switcher silently did nothing.
  function initAutoSubmit() {
    document.addEventListener('change', function (e) {
      var el = e.target.closest && e.target.closest('[data-auto-submit]');
      if (!el || !el.form) return;
      el.form.submit();
    });
  }

  // -- 6.5 project combobox (search switcher) ------------------------------

  // The project switcher needs a search-shaped picker once an installation
  // has more than ~20 projects: the native <select> dropdown the no-JS
  // path renders becomes a wall of text. initProjectCombobox promotes the
  // .project-combobox block alongside the form to a real input+cursor +
  // <ul role=listbox>, and hides the form so it never renders twice.
  //
  // Pairing: data-project-combobox-input / data-project-combobox-caret /
  // data-project-combobox-listbox are the wiring pins; app.js is the only
  // file that owns them, so they live without a global registry.

  var PROJECT_SEARCH_DEBOUNCE_MS = 150;
  var PROJECT_SEARCH_LIMIT = 20;

  function fmtProjectLabel(key, name) {
    if (!key) return 'All projects';
    return key + ' — ' + name;
  }

  function currentDisplay(c) {
    var key = c.getAttribute('data-current-key') || '';
    var name = c.getAttribute('data-current-name') || '';
    if (!key) return c.getAttribute('data-empty-label') || 'All projects';
    return fmtProjectLabel(key, name);
  }

  function setComboboxExpanded(c, open) {
    c.setAttribute('aria-expanded', open ? 'true' : 'false');
    var caret = c.querySelector('[data-project-combobox-caret]');
    if (caret) caret.setAttribute('aria-expanded', open ? 'true' : 'false');
    var list = c.querySelector('[data-project-combobox-listbox]');
    if (list) {
      if (open) list.removeAttribute('hidden');
      else list.setAttribute('hidden', '');
    }
  }

  function clearActiveOption(c) {
    c.removeAttribute('aria-activedescendant');
    var nodes = c.querySelectorAll('[role="option"][aria-selected="true"]');
    for (var i = 0; i < nodes.length; i++) nodes[i].removeAttribute('aria-selected');
  }

  function setActiveOption(c, opt) {
    if (!opt) { clearActiveOption(c); return; }
    if (opt.getAttribute('role') !== 'option') { clearActiveOption(c); return; }
    var nodes = c.querySelectorAll('[role="option"][aria-selected="true"]');
    for (var i = 0; i < nodes.length; i++) nodes[i].removeAttribute('aria-selected');
    opt.setAttribute('aria-selected', 'true');
    c.setAttribute('aria-activedescendant', opt.id || '');
  }

  function visibleOptionCount(c) {
    var out = 0;
    var opts = c.querySelectorAll('[role="option"]');
    for (var i = 0; i < opts.length; i++) {
      if (!opts[i].classList.contains('combobox-more')) out++;
    }
    return out;
  }

  function moveActive(c, dir) {
    var opts = c.querySelectorAll('[role="option"]');
    var pickable = [];
    for (var i = 0; i < opts.length; i++) {
      // aria-disabled covers both the "… type to narrow" hint and the "No
      // matches" row, so neither is ever the selection an arrow key lands on.
      if (opts[i].getAttribute('aria-disabled') !== 'true') pickable.push(opts[i]);
    }
    if (pickable.length === 0) return;
    var current = c.querySelector('[role="option"][aria-selected="true"]');
    var idx = -1;
    for (var i = 0; i < pickable.length; i++) {
      if (pickable[i] === current) { idx = i; break; }
    }
    var next = dir > 0 ? Math.min(pickable.length - 1, idx + 1) : Math.max(0, idx < 0 ? pickable.length - 1 : idx - 1);
    setActiveOption(c, pickable[next]);
    var list = c.querySelector('[data-project-combobox-listbox]');
    if (list && pickable[next].scrollIntoView) {
      pickable[next].scrollIntoView({ block: 'nearest' });
    }
  }

  function chooseProject(c, key) {
    window.location = '/?p=' + encodeURIComponent(key || '');
  }

  function renderProjects(c, items, hasMore, includeAll) {
    var list = c.querySelector('[data-project-combobox-listbox]');
    if (!list) return;
    while (list.firstChild) list.removeChild(list.firstChild);
    clearActiveOption(c);
    // When browsing (no query), the first option is always "All projects" so a
    // user on a board can navigate back to the overview from the switcher — the
    // search endpoint only knows about real projects, never this synthetic row.
    var rows = [];
    if (includeAll) rows.push({ key: '', name: '' });
    for (var k = 0; items && k < items.length; k++) rows.push(items[k]);
    if (rows.length === 0) {
      var empty = document.createElement('li');
      empty.className = 'combobox-empty';
      empty.setAttribute('role', 'option');
      empty.setAttribute('aria-disabled', 'true');
      empty.textContent = 'No matches';
      list.appendChild(empty);
      return;
    }
    for (var i = 0; i < rows.length; i++) {
      var li = document.createElement('li');
      li.id = 'project-combobox-opt-' + i;
      li.setAttribute('role', 'option');
      li.setAttribute('data-key', rows[i].key || '');
      li.textContent = fmtProjectLabel(rows[i].key, rows[i].name);
      list.appendChild(li);
    }
    if (hasMore) {
      var more = document.createElement('li');
      more.className = 'combobox-more';
      more.setAttribute('role', 'option');
      more.setAttribute('aria-disabled', 'true');
      more.textContent = '… type to narrow the results';
      list.appendChild(more);
    }
  }

  function fetchProjects(c, q, cb) {
    var url = '/projects/search?limit=' + encodeURIComponent(PROJECT_SEARCH_LIMIT);
    if (q) url += '&q=' + encodeURIComponent(q);
    fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
      .then(function (r) {
        if (!r.ok) throw new Error('status ' + r.status);
        return r.json();
      })
      .then(function (data) { cb(null, data && data.items ? data.items : [], !!(data && data.has_more)); })
      .catch(function () { cb(null, [], false); });
  }

  function debounceProjectSearch(c, q, cb) {
    if (c.__searchTimer) clearTimeout(c.__searchTimer);
    c.__searchTimer = setTimeout(function () {
      c.__searchTimer = null;
      fetchProjects(c, q, cb);
    }, PROJECT_SEARCH_DEBOUNCE_MS);
  }

  // comboQuery reads the effective search query from the input. Until the user
  // types, the field shows the current selection's label (e.g. "PROJ — Name"),
  // which is NOT a query: opening the list must browse from empty, not search
  // for the label (which no project name contains, so it returned "No matches").
  function comboQuery(c, input) {
    var raw = (input && input.value) || '';
    return raw === currentDisplay(c) ? '' : raw;
  }

  function openCombobox(c, preserveInput) {
    setComboboxExpanded(c, true);
    var input = c.querySelector('[data-project-combobox-input]');
    if (preserveInput && input) input.focus();
    var query = comboQuery(c, input);
    if (!c.__cache || c.__cache.q !== query) {
      fetchProjects(c, query, function (err, items, hasMore) {
        // Only paint if this combobox is still open and the query is the
        // one we issued with — a stale callback arriving after the user
        // already typed more must not clobber the live result list.
        if (c.getAttribute('aria-expanded') !== 'true') return;
        if (comboQuery(c, input) !== query) return;
        renderProjects(c, items, hasMore, query === '');
        c.__cache = { q: query, items: items, hasMore: hasMore };
      });
    }
  }

  function closeCombobox(c) {
    setComboboxExpanded(c, false);
    var input = c.querySelector('[data-project-combobox-input]');
    if (input) input.value = currentDisplay(c);
    clearActiveOption(c);
    if (input) input.blur();
  }

  function bindProjectCombobox(c) {
    var form = c.parentNode && c.parentNode.querySelector('[data-project-switcher-fallback]');
    var input = c.querySelector('[data-project-combobox-input]');
    var caret = c.querySelector('[data-project-combobox-caret]');
    var list = c.querySelector('[data-project-combobox-listbox]');
    if (!input || !caret || !list) return;

    // Hide the <select>-based fallback, reveal the combobox. The order
    // matters: a screen reader reading top-to-bottom must see the input
    // where the user expects it — the combobox sits in the same physical
    // spot on the page, so the visual swap is invisible to a seeing user.
    c.removeAttribute('hidden');
    if (form) form.setAttribute('hidden', '');

    input.value = currentDisplay(c);

    caret.addEventListener('click', function (e) {
      e.preventDefault();
      if (c.getAttribute('aria-expanded') === 'true') closeCombobox(c);
      else openCombobox(c, true);
    });

    input.addEventListener('focus', function () {
      input.select();
      openCombobox(c, true);
    });

    input.addEventListener('click', function () {
      if (c.getAttribute('aria-expanded') !== 'true') openCombobox(c, true);
      else input.select();
    });

    input.addEventListener('input', function () {
      var q = comboQuery(c, input);
      debounceProjectSearch(c, q, function (err, items, hasMore) {
        if (comboQuery(c, input) !== q) return;
        renderProjects(c, items, hasMore, q === '');
        c.__cache = { q: q, items: items, hasMore: hasMore };
      });
      if (c.getAttribute('aria-expanded') !== 'true') openCombobox(c, true);
    });

    input.addEventListener('keydown', function (e) {
      if (e.key === 'ArrowDown' || e.key === 'Down') {
        e.preventDefault();
        if (c.getAttribute('aria-expanded') !== 'true') openCombobox(c, false);
        moveActive(c, 1);
      } else if (e.key === 'ArrowUp' || e.key === 'Up') {
        e.preventDefault();
        if (c.getAttribute('aria-expanded') !== 'true') openCombobox(c, false);
        moveActive(c, -1);
      } else if (e.key === 'Enter') {
        // Enter picks the highlighted option, or — when the user typed and hit
        // Enter without arrowing — the first selectable match, the way a search
        // box is expected to behave. Disabled rows ("No matches", the "…"
        // hint) are never selectable, so this is a no-op on an empty result.
        var active = c.querySelector('[role="option"][aria-selected="true"]');
        if (!active || active.getAttribute('aria-disabled') === 'true') {
          active = c.querySelector('[role="option"]:not([aria-disabled="true"])');
        }
        if (active && active.getAttribute('aria-disabled') !== 'true') {
          e.preventDefault();
          chooseProject(c, active.getAttribute('data-key') || '');
        }
      } else if (e.key === 'Escape' || e.key === 'Esc') {
        if (c.getAttribute('aria-expanded') === 'true') {
          e.preventDefault();
          closeCombobox(c);
        }
      } else if (e.key === 'Tab') {
        // Tab is "close and let the focus continue": the user is moving
        // on, not escaping. We deliberately do NOT preventDefault; the
        // tab order keeps its natural shape.
        closeCombobox(c);
      }
    });

    list.addEventListener('mousedown', function (e) {
      // mousedown (not click) so the input loses focus before the list is
      // populated — choosing from a touch-click list would re-open the
      // dropdown on focus otherwise.
      var opt = e.target.closest && e.target.closest('[role="option"]');
      if (!opt || opt.getAttribute('aria-disabled') === 'true') {
        e.preventDefault();
        return;
      }
      e.preventDefault();
      chooseProject(c, opt.getAttribute('data-key') || '');
    });
  }

  function initProjectCombobox() {
    var nodes = document.querySelectorAll('[data-project-combobox]');
    for (var i = 0; i < nodes.length; i++) bindProjectCombobox(nodes[i]);

    // A click anywhere outside any open combobox closes every open one.
    // The handler checks every combobox rather than tracking which one is
    // open because a single page only ever has one project switcher; the
    // cost is well below what fetch costs.
    document.addEventListener('click', function (e) {
      var inside = e.target.closest && e.target.closest('[data-project-combobox]');
      var open = document.querySelector('[data-project-combobox][aria-expanded="true"]');
      if (open && open !== inside) closeCombobox(open);
    });
  }

  // -- 7. ai thoughts panel -----------------------------------------------
  //
  // The panel is a chat window, not a feed: newest message at the bottom
  // (the server now renders it that way — see view.ChatEntriesOldestFirst),
  // autoscroll follows the bottom only while the owner is already there,
  // and scrolling to the top loads older history from the server.

  var CHAT_STORAGE_KEY = 'kanban.chatPanel';
  // A few pixels of slack: browsers rarely land scrollTop at the exact
  // mathematical bottom (sub-pixel zoom, momentum scrolling), so an exact
  // equality check would flash the "new thoughts" marker on messages the
  // owner is already looking at.
  var CHAT_BOTTOM_TOLERANCE_PX = 6;
  // Loading older messages starts a little before the physical top so the
  // request is in flight before the owner's eye reaches the edge.
  var CHAT_LOAD_MORE_THRESHOLD_PX = 48;

  function chatFeedEl() {
    return document.querySelector('[data-chat-feed]');
  }

  function chatMarkerEl() {
    return document.querySelector('[data-chat-new-marker]');
  }

  // isChatPinnedToBottom reports whether the feed's scroll position is
  // (within tolerance) at its bottom edge. A feed with no overflow at all
  // (a short list, or not measurable while the panel is hidden) counts as
  // pinned — there is nothing to scroll past.
  function isChatPinnedToBottom(feed) {
    if (!feed) return true;
    var distance = feed.scrollHeight - feed.scrollTop - feed.clientHeight;
    return distance <= CHAT_BOTTOM_TOLERANCE_PX;
  }

  function scrollChatToBottom(feed) {
    if (feed) feed.scrollTop = feed.scrollHeight;
  }

  function showChatNewMarker() {
    var marker = chatMarkerEl();
    if (marker) marker.hidden = false;
  }

  function hideChatNewMarker() {
    var marker = chatMarkerEl();
    if (marker) marker.hidden = true;
  }

  // revealChatFeed un-hides the feed <ol> (and hides the "No thoughts yet"
  // placeholder next to it) the first time a message actually lands on a
  // project that had none when the page rendered. The <ol> is always in the
  // DOM precisely so this moment does not need a reload — see pages.html's
  // comment on the chat-feed markup.
  function revealChatFeed(feed) {
    if (feed.hidden) feed.hidden = false;
    var empty = document.querySelector('[data-chat-empty]');
    if (empty && !empty.hidden) empty.hidden = true;
  }

  // appendChatEntry is the single path anything that adds a message to the
  // live end of the feed goes through — SSE arrival (KANB-12) and, above,
  // the initial reveal of a feed that started empty. It appends the node,
  // then either follows it to the bottom — if the owner was already there —
  // or leaves their scroll position alone and raises the unobtrusive marker
  // instead.
  function appendChatEntry(li) {
    var feed = chatFeedEl();
    if (!feed || !li) return;
    revealChatFeed(feed);
    var pinned = isChatPinnedToBottom(feed);
    feed.appendChild(li);
    if (pinned) {
      scrollChatToBottom(feed);
    } else {
      showChatNewMarker();
    }
  }

  var chatOlderCursor = '';
  var chatOlderInFlight = false;

  function chatProjectKey() {
    var split = document.querySelector('[data-chat-split]');
    return split ? split.getAttribute('data-chat-project') || '' : '';
  }

  // loadOlderChatMessages fetches one older page from GET
  // /p/{key}/chat/older?before=<cursor> and prepends it above the feed's
  // current first entry. The cursor is keyset pagination (see
  // domain.ChatCursor / service.ChatListInput.Before): the server's query is
  // a strict "older than this exact point", so re-requesting it can neither
  // duplicate nor skip a message at the page boundary.
  //
  // Scroll position must not jump: the container's scrollTop is nudged by
  // exactly the height the new content added, the same technique any
  // prepend-above-the-viewport list uses. The response's
  // X-Chat-Next-Cursor header becomes the next call's cursor; an empty
  // header (or an empty body — belt and braces, both mean the same thing)
  // clears the stored cursor, and the guard at the top of this function
  // then refuses to ask again.
  function loadOlderChatMessages() {
    var feed = chatFeedEl();
    if (!feed || chatOlderInFlight || !chatOlderCursor) return;
    var key = chatProjectKey();
    if (!key) return;
    chatOlderInFlight = true;
    var url = '/p/' + encodeURIComponent(key) + '/chat/older?before=' + encodeURIComponent(chatOlderCursor);
    fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'text/html' } })
      .then(function (r) {
        if (!r.ok) throw new Error('status ' + r.status);
        chatOlderCursor = r.headers.get('X-Chat-Next-Cursor') || '';
        return r.text();
      })
      .then(function (html) {
        if (!html) return;
        var beforeHeight = feed.scrollHeight;
        var beforeTop = feed.scrollTop;
        feed.insertAdjacentHTML('afterbegin', html);
        feed.scrollTop = beforeTop + (feed.scrollHeight - beforeHeight);
      })
      .catch(function () { /* a failed page is not worth a toast; the next scroll retries with the same cursor */ })
      .then(function () { chatOlderInFlight = false; });
  }

  function onChatFeedScroll(e) {
    if (e.currentTarget.scrollTop <= CHAT_LOAD_MORE_THRESHOLD_PX) loadOlderChatMessages();
  }

  function setChatOpen(open) {
    var split = document.querySelector('[data-chat-split]');
    var panel = document.getElementById('chat-panel');
    var btn = document.querySelector('[data-chat-toggle]');
    if (!split || !panel || !btn) return;
    split.classList.toggle('is-open', open);
    panel.hidden = !open;
    btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    // Persistence is best effort: private mode or a blocked quota must not
    // break the toggle, it only means the state is not remembered.
    try { localStorage.setItem(CHAT_STORAGE_KEY, open ? 'open' : 'closed'); } catch (e) { /* ignore */ }
    // A hidden panel (display: none) cannot report a real scrollHeight, so
    // "first render" of the feed is really "the first time it becomes
    // visible" — whether that is right now (persisted open) or later, the
    // moment the owner clicks the toggle. Either way the newest message
    // must be there waiting, with no scrolling required.
    if (open) scrollChatToBottom(chatFeedEl());
  }

  function initChatPanel() {
    var btn = document.querySelector('[data-chat-toggle]');
    if (!btn) return;
    var cursorEl = document.querySelector('[data-chat-next-cursor]');
    chatOlderCursor = cursorEl ? cursorEl.getAttribute('data-chat-next-cursor') || '' : '';
    var feed = chatFeedEl();
    if (feed) feed.addEventListener('scroll', onChatFeedScroll);
    var marker = chatMarkerEl();
    if (marker) {
      marker.addEventListener('click', function () {
        scrollChatToBottom(feed);
        hideChatNewMarker();
      });
    }
    var saved = null;
    try { saved = localStorage.getItem(CHAT_STORAGE_KEY); } catch (e) { /* ignore */ }
    setChatOpen(saved === 'open');
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest && e.target.closest('[data-chat-toggle]');
    if (!btn) return;
    var split = document.querySelector('[data-chat-split]');
    if (!split) return;
    setChatOpen(!split.classList.contains('is-open'));
  });

  // -- 8. progress track delete --------------------------------------------
  //
  // Hovering an assessor's row under a progress metric reveals a small
  // cross (data-progress-delete-arm). Clicking it arms an in-place
  // confirmation (adds .is-confirming to the row) naming the assessor and
  // the point count — text already rendered server-side, nothing built
  // here. A second click (data-progress-delete-confirm) posts the delete
  // and swaps the whole metric (the bar and, if any tracks remain, the
  // list) for the fresh fragment the server renders afterwards: the mean
  // and the painted squares change along with the track list, so nothing
  // short of a real re-render is honest here. No modal at any point.
  //
  // Delegated on document throughout, deliberately: the metric fragment is
  // replaced wholesale on a successful delete, so anything bound to the old
  // nodes would be lost the moment it mattered.

  function armProgressDelete(li) {
    // Only one row confirms at a time; arming another cancels whichever was
    // armed before, so a stray confirmation never lingers.
    var open = document.querySelectorAll('.progress-track.is-confirming');
    for (var i = 0; i < open.length; i++) {
      if (open[i] !== li) open[i].classList.remove('is-confirming');
    }
    li.classList.add('is-confirming');
  }

  function cancelProgressDelete(li) {
    li.classList.remove('is-confirming');
  }

  // replaceProgressMetric swaps the ENTIRE metric — the bar, the optional
  // forecast badge, the optional chart container and the track list, all
  // together — for the freshly rendered fragment, rather than patching
  // pieces of it in place.
  //
  // This used to instead find "the old bar" by walking to the track list's
  // previousElementSibling and requiring it to carry the .pbar class —
  // correct only for as long as the bar and the track list stayed direct
  // siblings with nothing between them. KANB-13 (the history chart) then
  // inserted a chart container between them for every Clickable metric,
  // i.e. every metric this handler ever runs against (a metric with no
  // tracks has nothing to delete), so that lookup was silently wrong
  // (oldBar always null, the check on .pbar never true) from the day the
  // chart shipped: every delete left the stale bar, forecast badge and
  // chart container on screen and inserted a second, fresh metric right
  // next to them — two progress bars for the same metric, showing
  // different numbers. None of the Go tests caught it because they check
  // the HTTP response body, never a DOM mutation.
  //
  // The template now wraps the whole emission in one always-present
  // .progress-metric container per metric (data-progress-metric), so there
  // is nothing left to find by position: climb from wherever the delete
  // happened to that one container and replace it whole. This removes the
  // whole class of "which sibling is it now" bugs, not just this instance
  // of it — a future task inserting yet another element into the metric
  // cannot break this again, because nothing here depends on what the
  // metric's internals look like any more. When the deleted track was the
  // metric's last one the server responds with an empty fragment; there is
  // then nothing to insert, so the old container is simply removed and
  // nothing takes its place — the same "no data, no bar" rule the template
  // itself follows.
  function replaceProgressMetric(anchor, html) {
    if (!anchor) return;
    var oldMetric = anchor.closest('[data-progress-metric]');
    if (!oldMetric || !oldMetric.parentNode) return;
    if (html) oldMetric.insertAdjacentHTML('beforebegin', html);
    oldMetric.parentNode.removeChild(oldMetric);
  }

  function submitProgressDelete(li) {
    var list = li.closest('.progress-tracks');
    var fields = {
      project: li.getAttribute('data-project') || '',
      task: li.getAttribute('data-task') || '',
      assessor: li.getAttribute('data-assessor') || ''
    };
    postForm('/fragments/progress/delete', fields).then(function (res) {
      if (!res.ok) {
        return errorMessage(res, 'Could not delete that track.').then(function (msg) {
          toast(msg, 'error');
        });
      }
      return res.text().then(function (html) {
        replaceProgressMetric(list, html);
      });
    }).catch(function () {
      toast('Could not delete that track.', 'error');
    });
  }

  function initProgressTrackDelete() {
    document.addEventListener('click', function (e) {
      var el = e.target;
      var arm = el.closest && el.closest('[data-progress-delete-arm]');
      if (arm) {
        var armRow = arm.closest('[data-progress-track]');
        if (armRow) armProgressDelete(armRow);
        return;
      }
      var cancel = el.closest && el.closest('[data-progress-delete-cancel]');
      if (cancel) {
        var cancelRow = cancel.closest('[data-progress-track]');
        if (cancelRow) cancelProgressDelete(cancelRow);
        return;
      }
      var confirm = el.closest && el.closest('[data-progress-delete-confirm]');
      if (confirm) {
        var confirmRow = confirm.closest('[data-progress-track]');
        if (confirmRow) submitProgressDelete(confirmRow);
      }
    });

    // Escape dismisses whichever row is currently armed, from anywhere
    // inside it — a keyboard user must never be stuck with a confirmation
    // there is no mouse to cancel.
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape') return;
      var armed = e.target.closest && e.target.closest('.progress-track.is-confirming');
      if (armed) cancelProgressDelete(armed);
    });
  }

  // -- 9. progress chart toggle ---------------------------------------------
  //
  // Clicking a progress bar's .pbar span (data-progress-chart-toggle, see
  // partials.html's "progress-bar" define) opens its history chart in the
  // hidden <div class="progress-chart"> rendered right after it; clicking
  // again closes it. Works on a task card and in the project header alike —
  // both go through the same "progress-bar" define, so there is only one
  // place this is wired. The chart is fetched once, the first time a given
  // bar opens — GET /p/{key}/progress/chart?task=<key> (task omitted
  // selects the project-level scope) — and cached in the DOM afterwards, so
  // reopening the same bar just toggles `hidden` with no second request.
  // This mirrors loadOlderChatMessages's fetch-fragment shape (see that
  // function's own comment above) rather than inventing a second
  // convention; KANB-13's REPORT.md has the measured argument for fetching
  // instead of rendering every chart on every card up front.
  //
  // The toggle attribute lives ONLY on the .pbar span, never on anything
  // that also contains the delete-track control's <ul
  // class="progress-tracks">: the two are rendered as SIBLINGS (see
  // partials.html), so a click on the delete cross or its confirm/cancel
  // buttons never reaches closest('[data-progress-chart-toggle]') at all —
  // the DOM shape keeps them apart without this code needing to special-
  // case anything. A bar with no history (view.ProgressView.Clickable
  // false) carries no data-progress-chart-toggle attribute at all, so it is
  // simply never matched here: no dead click, and app.css paints the
  // pointer-cursor affordance only on .pbar-clickable.

  // progressChartContainer finds the empty (or already-filled) chart
  // container that belongs to one .pbar span: its next sibling, skipping
  // over the forecast badge when one is rendered between them (the
  // "progress-bar" define's fixed emission order is pbar, optional
  // forecast, optional chart container). Anything else in between means
  // this bar has no chart container — should not happen for a Clickable
  // bar, but a missing container is simply treated as nothing to open.
  function progressChartContainer(bar) {
    var el = bar.nextElementSibling;
    while (el) {
      if (el.hasAttribute('data-progress-chart')) return el;
      if (!el.classList.contains('forecast')) return null;
      el = el.nextElementSibling;
    }
    return null;
  }

  function setProgressChartOpen(bar, container, open) {
    bar.setAttribute('aria-expanded', open ? 'true' : 'false');
    container.hidden = !open;
  }

  // fetchProgressChart loads the fragment exactly once per bar: a filled
  // container (childElementCount > 0) is assumed current for the lifetime
  // of the page, the same "fetch once, toggle after" contract
  // loadOlderChatMessages's cursor guard gives the chat panel's pages.
  function fetchProgressChart(bar, container) {
    var project = bar.getAttribute('data-project') || '';
    if (!project) return;
    var task = bar.getAttribute('data-task') || '';
    var url = '/p/' + encodeURIComponent(project) + '/progress/chart' +
      (task ? '?task=' + encodeURIComponent(task) : '');
    fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'text/html' } })
      .then(function (r) {
        if (!r.ok) throw new Error('status ' + r.status);
        return r.text();
      })
      .then(function (html) {
        // An empty fragment means the history vanished between render and
        // fetch (e.g. a track delete raced this click) — nothing to show,
        // and the bar stays closed rather than opening on emptiness.
        if (!html) return;
        container.innerHTML = html;
        setProgressChartOpen(bar, container, true);
      })
      .catch(function () {
        toast('Could not load the progress chart.', 'error');
      });
  }

  function toggleProgressChart(bar) {
    var container = progressChartContainer(bar);
    if (!container) return;
    if (!container.hidden) {
      setProgressChartOpen(bar, container, false);
      return;
    }
    if (container.childElementCount > 0) {
      setProgressChartOpen(bar, container, true);
      return;
    }
    fetchProgressChart(bar, container);
  }

  function initProgressChart() {
    document.addEventListener('click', function (e) {
      var bar = e.target.closest && e.target.closest('[data-progress-chart-toggle]');
      if (!bar) return;
      toggleProgressChart(bar);
    });
    // role="button" on a <span> needs Enter/Space wired by hand — a real
    // <button> gets both for free, but .pbar cannot be one (it also paints
    // the ten square cells app.css positions as flex children).
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return;
      var bar = e.target.closest && e.target.closest('[data-progress-chart-toggle]');
      if (!bar) return;
      // Space must not also scroll the page, the way it would with no
      // handler at all on a focused, non-native "button".
      e.preventDefault();
      toggleProgressChart(bar);
    });
  }

  // -- boot ---------------------------------------------------------------

  function boot() {
    initSortables();
    initMoveMenu();
    initAcceptance();
    initCopy();
    initDialog();
    initAutoSubmit();
    initProjectCombobox();
    initChatPanel();
    initProgressTrackDelete();
    initProgressChart();
    initLiveRefresh();
    startLive();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
