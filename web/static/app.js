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
    dragging: false
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
          var firstRow = current.querySelector('.row');
          var previousFirstRowText = firstRow ? firstRow.textContent.trim() : null;
          current.innerHTML = fresh.innerHTML;
          markNewRows(current, previousFirstRowText);
        }
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
    ['board-update', 'activity-feed'].forEach(function (name) {
      es.addEventListener(name, scheduleRefresh);
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

  // -- boot ---------------------------------------------------------------

  function boot() {
    initSortables();
    initMoveMenu();
    initAcceptance();
    initCopy();
    initDialog();
    initAutoSubmit();
    initProjectCombobox();
    startLive();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
