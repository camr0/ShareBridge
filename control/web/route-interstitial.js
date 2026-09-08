/* route-interstitial.js — ShareBridge control-owned interstitial (§9.3).
 *
 * Control-authored, served same-origin under a per-response CSP nonce. The
 * page flow: one bounded same-origin preparation call, then a CORS reach-
 * ability check against the direct agent origin, then navigation by the
 * exact §9.3 rules. Every URL handled here is control-derived (rendered
 * into the page); request input is never consulted.
 */
(function () {
  'use strict';

  var root = document.getElementById('sb-interstitial');
  if (!root) {
    return;
  }

  var statusEl = document.getElementById('sb-status');
  var unavailableEl = document.getElementById('sb-unavailable');

  // §4.4 recipient-path budget for the direct GET /s/<code>/connect check;
  // the server echoes it as direct_timeout_ms. The preparation call itself
  // is bounded by the server's own four-second context — the page guard for
  // it is bounded just above so the server's relay/unavailable JSON can
  // always land.
  var DEFAULT_CHECK_BUDGET_MS = 4000;
  var PREPARE_GUARD_MS = DEFAULT_CHECK_BUDGET_MS + 1000;

  // One master cancellation for the page: the immediate "Use relay now"
  // action aborts the in-flight preparation/direct fetch before navigating.
  var master = new AbortController();

  function relayURLFromPage() {
    var btn = document.getElementById('sb-use-relay');
    return btn ? btn.getAttribute('data-relay-url') : null;
  }

  function navigate(url) {
    window.location.replace(url);
  }

  function showUnavailable() {
    if (statusEl) {
      statusEl.setAttribute('hidden', '');
    }
    if (unavailableEl) {
      unavailableEl.removeAttribute('hidden');
    }
  }

  // Terminal rule: the already-returned relay URL when one exists, else the
  // unavailable UI with the canonical retry link (§9.3).
  function fallbackOrUnavailable(preferredRelay) {
    var relay = preferredRelay || relayURLFromPage();
    if (relay) {
      navigate(relay);
    } else {
      showUnavailable();
    }
  }

  // fetchBounded: fetch that aborts after ms and when the master aborts.
  function fetchBounded(url, options, ms) {
    var stage = new AbortController();
    var timer = setTimeout(function () {
      stage.abort();
    }, ms);
    var onMaster = function () {
      stage.abort();
    };
    if (master.signal.aborted) {
      stage.abort();
    } else {
      master.signal.addEventListener('abort', onMaster);
    }
    var opts = options || {};
    opts.signal = stage.signal;
    return fetch(url, opts).then(
      function (resp) {
        clearTimeout(timer);
        master.signal.removeEventListener('abort', onMaster);
        return resp;
      },
      function (err) {
        clearTimeout(timer);
        master.signal.removeEventListener('abort', onMaster);
        throw err;
      }
    );
  }

  // Immediate manual action (§4.4): cancel the direct attempt and use the
  // already-returned relay URL — never a new route fetch.
  var useRelayBtn = document.getElementById('sb-use-relay');
  if (useRelayBtn) {
    useRelayBtn.addEventListener('click', function () {
      master.abort();
      var relay = relayURLFromPage();
      if (relay) {
        navigate(relay);
      }
    });
  }

  function checkDirect(directURL, budgetMs) {
    // CORS check against the direct agent origin: GET <direct>/s/<code>/
    // connect answers 204 only after the agent's own SNI/Host/route/code
    // authorization (§9.3). A 204 completes the direct check; a timeout,
    // TLS, or network failure within the budget is a miss. Credential-free
    // and content-free by design.
    var connectURL = directURL.replace(/\/+$/, '') + '/connect';
    return fetchBounded(
      connectURL,
      { method: 'GET', credentials: 'omit', cache: 'no-store', mode: 'cors' },
      budgetMs
    ).then(function (resp) {
      if (resp.status === 204) {
        navigate(directURL);
        return;
      }
      throw new Error('direct check status ' + resp.status);
    });
  }

  function prepare() {
    return fetchBounded(
      root.getAttribute('data-prepare-url'),
      { method: 'POST', credentials: 'omit', cache: 'no-store' },
      PREPARE_GUARD_MS
    ).then(function (resp) {
      if (!resp.ok) {
        throw new Error('prepare status ' + resp.status);
      }
      return resp.json();
    });
  }

  prepare()
    .then(function (data) {
      if (master.signal.aborted) {
        return;
      }
      if (data && data.status === 'direct' && data.direct_url) {
        // Direct success navigates direct WITHOUT touching relay.
        var budget =
          data.direct_timeout_ms > 0 ? data.direct_timeout_ms : DEFAULT_CHECK_BUDGET_MS;
        return checkDirect(data.direct_url, budget).catch(function (err) {
          if (master.signal.aborted) {
            return;
          }
          // Check miss within the §4.4 budget ⇒ relay when the preparation
          // returned one, else unavailable + canonical retry.
          fallbackOrUnavailable(data.relay_url);
        });
      }
      if (data && data.status === 'relay' && data.relay_url) {
        navigate(data.relay_url);
        return;
      }
      showUnavailable();
    })
    .catch(function () {
      if (master.signal.aborted) {
        return;
      }
      fallbackOrUnavailable(null);
    });
})();
