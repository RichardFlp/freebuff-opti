/*
 * Freebuff Opti - the panel that runs inside Freebuff Desktop.
 *
 * This file is injected as a same-origin <script> by FreebuffOpti.exe. It adds
 * one button to Freebuff's own sidebar rail and one page, fitted to the app's
 * workspace frame, holding the RAM, CPU, display and cleanup controls the
 * injector's background guard enforces.
 *
 * Division of labour, because it is not obvious from the outside:
 *
 *   the panel   owns the settings, draws them, and can only change things a
 *               web page can change - its own CSS.
 *   the daemon  owns enforcement. A renderer cannot cap another process's
 *               working set or CPU, so the panel saves the settings and the Go
 *               guard, running outside the app, applies them to every Freebuff
 *               process it finds.
 *
 * The settings therefore have to survive a launch, which rules out localStorage:
 * Freebuff serves its UI from a fresh loopback port every start, and
 * localStorage is keyed by origin (scheme + host + port). Cookies are the only
 * store that comes back, and are what the daemon reads. See store.go for the
 * other end of that contract - the two files must agree on the cookie names.
 */
;(function () {
  'use strict'

  var VERSION = '1.0.0'

  /* ------------------------------------------------- persistence contract ---
   * The settings are one JSON document, base64url-encoded, split across
   * `fbop_<gen>_<i>` cookies. `fbop_n` holds "<generation>:<chunkCount>" and is
   * written LAST, so a save that is interrupted halfway leaves the previous
   * document intact and readable instead of a half-written one.
   *
   * The generation counter exists for exactly that reason: rewriting the chunks
   * in place and publishing the count first would mean a kill mid-save erases
   * the settings, and the next launch would then save the defaults over them.
   *
   * The daemon reads a chunk series the same way (store.go). Keep the names and
   * the count format in step with it.
   */
  var COOKIE_PREFIX = 'fbop'
  var COOKIE_COUNT = 'fbop_n'
  var COOKIE_CHUNK = 3000
  var COOKIE_MAX_CHUNKS = 6
  var COOKIE_DAYS = 3650
  var LS_OPEN = 'fbop_open'

  /* The daemon drops its last pass here, and the orchestrator serves this
   * directory straight off disk - so a same-origin fetch reads it. Freebuff's
   * CSP allows `connect-src 'self'`, which is what makes this the one honest way
   * for the page to show what was actually applied. */
  var STATE_URL = './assets/freebuff-opti-status.json'

  /* Where the save timer sits: a change is written after a short pause, and at
   * most this often while a slider is being dragged. */
  var SAVE_DEBOUNCE = 220
  var SAVE_AT_MOST = 1400

  /* ------------------------------------------------------------------ state ---
   * The document shape on the wire. Every field is optional on the daemon side,
   * so a partial or older document still applies whatever it does have - but the
   * panel always writes the whole thing.
   */
  var DEFAULTS = {
    v: 1,
    preset: 'custom',
    memory: { enabled: false, capMb: 1200, trimOnApply: true, idleTrimMb: 0 },
    cpu: { enabled: false, priority: 'below-normal', cores: 0, ecoQos: false, jobCapPercent: 0 },
    windows: { compact: false, reduceMotion: false, reduceEffects: false, thinScrollbars: false },
    cleanup: { cacheSweepMb: 0, killOrphans: false, reapIdleThreads: false },
    watchdog: { enabled: true, intervalSeconds: 20 },
    updated: '',
  }

  /* Fractions of the work the app does that a potato PC simply does not need.
   * The presets are deliberately explicit rather than computed, so that "what
   * does Potato actually set?" has one answer, and it is written down. */
  var PRESETS = {
    off: {
      label: 'Off',
      blurb: 'Nothing applied. Freebuff runs exactly as it ships.',
      settings: {
        memory: { enabled: false, capMb: 1200, trimOnApply: false, idleTrimMb: 0 },
        cpu: { enabled: false, priority: 'normal', cores: 0, ecoQos: false, jobCapPercent: 0 },
        windows: { compact: false, reduceMotion: false, reduceEffects: false, thinScrollbars: false },
        cleanup: { cacheSweepMb: 0, killOrphans: false, reapIdleThreads: false },
      },
    },
    low: {
      label: 'Low-end',
      blurb: 'A first, light pass: trims idle pages and steps the app down a priority notch.',
      settings: {
        memory: { enabled: true, capMb: 1536, trimOnApply: true, idleTrimMb: 900 },
        cpu: { enabled: true, priority: 'below-normal', cores: 0, ecoQos: true, jobCapPercent: 0 },
        windows: { compact: false, reduceMotion: true, reduceEffects: false, thinScrollbars: false },
        cleanup: { cacheSweepMb: 400, killOrphans: true, reapIdleThreads: false },
      },
    },
    potato: {
      label: 'Potato',
      blurb: 'The hard one. Real caps, a small CPU footprint, and the cosmetics turned down.',
      settings: {
        memory: { enabled: true, capMb: 700, trimOnApply: true, idleTrimMb: 450 },
        cpu: { enabled: true, priority: 'idle', cores: 0, ecoQos: true, jobCapPercent: 0 },
        windows: { compact: true, reduceMotion: true, reduceEffects: true, thinScrollbars: true },
        cleanup: { cacheSweepMb: 200, killOrphans: true, reapIdleThreads: true },
      },
    },
    battery: {
      label: 'Battery',
      blurb: 'For a laptop on battery: gentle memory, aggressive CPU economy, no cosmetic changes.',
      settings: {
        memory: { enabled: true, capMb: 1200, trimOnApply: false, idleTrimMb: 900 },
        cpu: { enabled: true, priority: 'below-normal', cores: 0, ecoQos: true, jobCapPercent: 0 },
        windows: { compact: false, reduceMotion: true, reduceEffects: false, thinScrollbars: false },
        cleanup: { cacheSweepMb: 300, killOrphans: true, reapIdleThreads: false },
      },
    },
  }

  /* --------------------------------------------------------------- helpers --- */

  function clone(v) {
    return JSON.parse(JSON.stringify(v))
  }

  function isObj(v) {
    return v !== null && typeof v === 'object' && !Array.isArray(v)
  }

  /** Deep-merge a saved or partial document onto the defaults, discarding
   *  anything whose type is wrong rather than trusting its shape. */
  function normalize(input) {
    var out = clone(DEFAULTS)
    if (!isObj(input)) return out
    if (typeof input.v === 'number') out.v = input.v
    if (typeof input.preset === 'string') out.preset = input.preset
    if (typeof input.updated === 'string') out.updated = input.updated
    var sections = ['memory', 'cpu', 'windows', 'cleanup', 'watchdog']
    for (var s = 0; s < sections.length; s++) {
      var key = sections[s]
      if (!isObj(input[key])) continue
      for (var f in out[key]) {
        if (!Object.prototype.hasOwnProperty.call(out[key], f)) continue
        var value = input[key][f]
        if (typeof value === typeof out[key][f]) out[key][f] = value
      }
    }
    // Clamp anything numeric to the range the controls can actually express,
    // so a hand-edited cookie cannot put the daemon out of bounds.
    out.memory.capMb = clampInt(out.memory.capMb, 128, 8192)
    out.memory.idleTrimMb = clampInt(out.memory.idleTrimMb, 0, 8192)
    out.cleanup.cacheSweepMb = clampInt(out.cleanup.cacheSweepMb, 0, 4096)
    out.cpu.jobCapPercent = clampInt(out.cpu.jobCapPercent, 0, 100)
    out.watchdog.intervalSeconds = clampInt(out.watchdog.intervalSeconds, 5, 300)
    out.cpu.cores = clampInt(out.cpu.cores, 0, cores())
    if (['idle', 'below-normal', 'normal'].indexOf(out.cpu.priority) < 0) out.cpu.priority = 'normal'
    return out
  }

  function clampInt(v, lo, hi) {
    var n = Math.round(Number(v))
    if (!isFinite(n)) return lo
    return Math.max(lo, Math.min(hi, n))
  }

  function cores() {
    var n = parseInt(navigator.hardwareConcurrency, 10)
    return isFinite(n) && n > 0 ? n : 8
  }

  function deviceMemoryGB() {
    var gb = Number(navigator.deviceMemory)
    return isFinite(gb) && gb > 0 ? gb : 0
  }

  function getPath(doc, path) {
    var parts = path.split('.')
    var node = doc
    for (var i = 0; i < parts.length; i++) {
      if (node == null) return undefined
      node = node[parts[i]]
    }
    return node
  }

  function setPath(doc, path, value) {
    var parts = path.split('.')
    var node = doc
    for (var i = 0; i < parts.length - 1; i++) node = node[parts[i]]
    node[parts[parts.length - 1]] = value
  }

  function toBase64Url(text) {
    // btoa is latin-1 only, and JSON is not.
    var bytes = new TextEncoder().encode(text)
    var bin = ''
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i])
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  }

  function fromBase64Url(b64) {
    var s = String(b64).replace(/-/g, '+').replace(/_/g, '/')
    while (s.length % 4) s += '='
    var bin = atob(s)
    var bytes = new Uint8Array(bin.length)
    for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
    return new TextDecoder().decode(bytes)
  }

  /* ---------------------------------------------------------------- cookies --- */

  var cookiesUsable = true

  function readCookie(name) {
    var m = document.cookie.match(
      new RegExp('(?:^|; )' + name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '=([^;]*)'),
    )
    if (!m) return null
    try {
      return decodeURIComponent(m[1])
    } catch (e) {
      return m[1]
    }
  }

  function writeCookie(name, value, days) {
    var exp = new Date(Date.now() + days * 86400000).toUTCString()
    document.cookie = name + '=' + encodeURIComponent(value) + '; expires=' + exp + '; path=/; SameSite=Lax'
    cookiesUsable = readCookie(name) === String(value)
    return cookiesUsable
  }

  function eraseCookie(name) {
    document.cookie = name + '=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/; SameSite=Lax'
  }

  /** "<gen>:<count>". A bare number is a series written by a build that had no
   *  generations, which is generation 0. */
  function parseCount(raw) {
    var m = /^(\d+):(\d+)$/.exec(raw || '')
    if (m) {
      var gen = Number(m[1])
      var count = Number(m[2])
      return {
        gen: isFinite(gen) ? gen : 0,
        count: isFinite(count) && count >= 0 && count <= COOKIE_MAX_CHUNKS ? count : 0,
      }
    }
    if (/^\d+$/.test(raw || '')) {
      var n = Number(raw)
      return { gen: 0, count: n > 0 && n <= COOKIE_MAX_CHUNKS ? n : 0 }
    }
    return { gen: 0, count: 0 }
  }

  function chunkName(gen, i) {
    return COOKIE_PREFIX + '_' + (gen ? gen + '_' + i : i)
  }

  function readSeries() {
    var s = parseCount(readCookie(COOKIE_COUNT))
    if (!s.count) return null
    var parts = []
    for (var i = 0; i < s.count; i++) {
      var v = readCookie(chunkName(s.gen, i))
      if (v === null) return null
      parts.push(v)
    }
    return parts.join('')
  }

  function clearSeries() {
    var withSeparator = COOKIE_PREFIX + '_'
    document.cookie.split(';').forEach(function (entry) {
      var name = entry.trim().split('=')[0]
      if (name === COOKIE_PREFIX || name.indexOf(withSeparator) === 0) eraseCookie(name)
    })
  }

  function writeSeries(b64) {
    var chunks = []
    // An empty payload is a deliberate wipe rather than a zero-chunk save that
    // a reader would then treat as corrupt.
    if (!b64.length) {
      clearSeries()
      return true
    }
    for (var p = 0; p < b64.length; p += COOKIE_CHUNK) chunks.push(b64.slice(p, p + COOKIE_CHUNK))
    if (chunks.length > COOKIE_MAX_CHUNKS) return false

    var prev = parseCount(readCookie(COOKIE_COUNT))
    var gen = prev.gen + 1
    for (var i = 0; i < chunks.length; i++) {
      if (!writeCookie(chunkName(gen, i), chunks[i], COOKIE_DAYS)) {
        for (var c = 0; c < COOKIE_MAX_CHUNKS; c++) eraseCookie(chunkName(gen, c))
        return false
      }
    }
    // A killed earlier save may have left higher indices for this generation.
    // Clear them before publishing, or stale chunks keep riding every request.
    for (var stale = chunks.length; stale < COOKIE_MAX_CHUNKS; stale++) eraseCookie(chunkName(gen, stale))
    if (!writeCookie(COOKIE_COUNT, gen + ':' + chunks.length, COOKIE_DAYS)) {
      for (var c2 = 0; c2 < COOKIE_MAX_CHUNKS; c2++) eraseCookie(chunkName(gen, c2))
      return false
    }
    // Only now is the previous generation dead weight. Prune abandoned
    // generations too, so interrupted writes cannot grow the request header for
    // ever.
    var active = {}
    for (var keep = 0; keep < chunks.length; keep++) active[chunkName(gen, keep)] = true
    var prefixWithSeparator = COOKIE_PREFIX + '_'
    document.cookie.split(';').forEach(function (entry) {
      var name = entry.trim().split('=')[0]
      if (name.indexOf(prefixWithSeparator) !== 0) return
      var rest = name.slice(prefixWithSeparator.length)
      var isChunk = /^\d+$/.test(rest) || /^\d+_\d+$/.test(rest)
      if (isChunk && !active[name]) eraseCookie(name)
    })
    return true
  }

  function loadSettings() {
    var b64 = null
    try {
      b64 = readSeries()
    } catch (e) {}
    if (b64 !== null) {
      try {
        return normalize(JSON.parse(fromBase64Url(b64)))
      } catch (e) {}
    }
    // A count cookie with no readable body means a save is there but damaged.
    // Start from the defaults but never write over it until the user changes
    // something - loadSettings() is followed by a deliberate no-save boot.
    return null
  }

  function savedDocumentExists() {
    return readCookie(COOKIE_COUNT) !== null
  }

  /* ------------------------------------------------------------------ saving --- */

  var state = null
  var saveTimer = null
  var saveFirstAt = 0
  var notifySave = function () {}

  function saveNow(immediate) {
    if (immediate) {
      clearTimeout(saveTimer)
      saveTimer = null
    }
    var doc = clone(state)
    doc.updated = new Date().toISOString()
    var ok = false
    try {
      ok = writeSeries(toBase64Url(JSON.stringify(doc)))
    } catch (e) {
      ok = false
    }
    if (!ok) notifySave('Could not save the settings to the cookie jar')
    else if (!cookiesUsable) notifySave('Cookies are blocked - the settings will not survive a restart')
    return ok
  }

  /** A throttle with a trailing edge, not a plain debounce: a dragged slider
   *  fires continuously, and a deadline that keeps being pushed back never
   *  arrives while the user is still moving the mouse. */
  function queueSave() {
    var now = Date.now()
    if (!saveFirstAt) saveFirstAt = now
    if (saveTimer && now - saveFirstAt >= SAVE_AT_MOST) return saveNow(true)
    if (saveTimer) clearTimeout(saveTimer)
    saveTimer = setTimeout(function () {
      saveTimer = null
      saveFirstAt = 0
      saveNow(true)
    }, SAVE_DEBOUNCE)
  }

  function changed() {
    state.preset = 'custom'
    state.updated = new Date().toISOString()
    queueSave()
  }

  /* -------------------------------------------------------------- cosmetics ---
   * The display controls are the only limits the panel can apply itself: they
   * are pure CSS, they affect this document only, and removing the style
   * element puts everything back. Everything else is the daemon's job.
   */
  var COSMETIC_ID = 'freebuff-opti-cosmetics'
  var MOTION_CSS =
    '*, *::before, *::after { animation-duration: .001ms !important; animation-delay: 0s !important;' +
    ' transition-duration: .001ms !important; transition-delay: 0s !important; scroll-behavior: auto !important; }'
  var EFFECTS_CSS =
    '*, *::before, *::after { backdrop-filter: none !important; -webkit-backdrop-filter: none !important;' +
    ' filter: none !important; }' +
    '.workspace-frame, .settings-frame, aside, nav { box-shadow: none !important; }'
  var SCROLLBAR_CSS =
    '* { scrollbar-width: thin !important; }' +
    '*::-webkit-scrollbar { width: 8px !important; height: 8px !important; }'
  var COMPACT_CSS =
    '.desktop-shell { --shell-inset: 4px !important; --shell-rail-width: 46px !important; }' +
    '.workspace-frame, .settings-frame { border-radius: 12px !important; }'

  function cosmeticCSS(s) {
    var w = s.windows || {}
    var out = []
    if (w.reduceMotion) out.push(MOTION_CSS)
    if (w.reduceEffects) out.push(EFFECTS_CSS)
    if (w.thinScrollbars) out.push(SCROLLBAR_CSS)
    if (w.compact) out.push(COMPACT_CSS)
    return out.join('\n')
  }

  function applyCosmetics() {
    var css = cosmeticCSS(state)
    var el = document.getElementById(COSMETIC_ID)
    if (!css) {
      if (el && el.parentNode) el.parentNode.removeChild(el)
      return
    }
    if (!el) {
      el = document.createElement('style')
      el.id = COSMETIC_ID
      el.setAttribute('data-freebuff-opti', '1')
      ;(document.head || document.documentElement).appendChild(el)
    }
    if (el.textContent !== css) el.textContent = css
  }

  /* ------------------------------------------------------------------- DOM --- */

  function el(tag, props, children) {
    var node = document.createElement(tag)
    if (props)
      for (var k in props) {
        if (k === 'class') node.className = props[k]
        else if (k === 'text') node.textContent = props[k]
        else if (k === 'html') node.innerHTML = props[k]
        else if (k.indexOf('on') === 0 && typeof props[k] === 'function') node.addEventListener(k.slice(2), props[k])
        else if (k === 'style') node.style.cssText = props[k]
        else if (props[k] != null) node.setAttribute(k, props[k])
      }
    ;(children || []).forEach(function (c) {
      if (c == null) return
      node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c)
    })
    return node
  }

  var GAUGE_ICON =
    '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" ' +
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="M3.6 17a9 9 0 1 1 16.8 0"/><path d="M12 12.6 16.2 8.4"/>' +
    '<circle cx="12" cy="17" r="1.6" fill="currentColor" stroke="none"/></svg>'

  var STYLES = `
:host {
  --fbop-font: var(--font-sans, ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif);
  --fbop-mono: var(--font-mono, ui-monospace, Menlo, Consolas, monospace);
  --fbop-bg: var(--bg, #16171b);
  --fbop-panel: var(--bg, #16171b);
  --fbop-panel2: var(--chrome, var(--surface, #1d1f24));
  --fbop-line: var(--border, #2a2d35);
  --fbop-ink: var(--text, #e7e9ee);
  --fbop-mute: var(--muted, #9aa1ad);
  --fbop-faint: var(--faint, #6d7480);
  --fbop-accent: var(--brand, #5b8cff);
  --fbop-good: #4ecb8d;
  --fbop-warn: #e0b055;
  --fbop-danger: var(--danger, #ff6b6b);
  --fbop-ui: var(--font-size-ui, 12px);
  --fbop-label: var(--font-size-label, 11px);
  --fbop-head: var(--font-size-heading, 18px);
}
* { box-sizing: border-box; margin: 0; padding: 0; font-family: var(--fbop-font); }

.fbop-panel {
  position: fixed;
  top: 48px; left: 52px; right: 8px; bottom: 8px;
  z-index: 1;
  /*
   * The shadow host is pointer-events: none so that it cannot swallow clicks on
   * the app underneath it while the panel is closed. Every visible box in the
   * shadow root therefore has to opt back in - without this the panel renders
   * perfectly and passes every click straight through to the page behind it.
   */
  pointer-events: auto;
  display: none;
  flex-direction: column;
  overflow: hidden;
  background: var(--fbop-bg);
  border: 1px solid var(--fbop-line);
  border-radius: 18px;
  color: var(--fbop-ink);
}
.fbop-panel.open { display: flex; }

.fbop-titlebar {
  display: flex; align-items: center; gap: 11px;
  padding: 13px 16px 12px;
  background: var(--fbop-panel2);
  border-bottom: 1px solid var(--fbop-line);
  flex: none;
}
.fbop-titlebar svg { color: var(--fbop-accent); flex: none; }
.fbop-titles { flex: 1; min-width: 0; }
.fbop-title { font-size: var(--fbop-head); font-weight: 600; line-height: 1.2; }
.fbop-sub { font-size: var(--fbop-label); color: var(--fbop-mute); margin-top: 2px; }
.fbop-x {
  flex: none; width: 30px; height: 30px; border: 0; border-radius: 7px; background: transparent;
  color: var(--fbop-mute); font-size: 17px; line-height: 1; cursor: pointer;
}
.fbop-x:hover { background: color-mix(in srgb, var(--fbop-ink) 9%, transparent); color: var(--fbop-ink); }

.fbop-tabs { display: flex; gap: 2px; padding: 0 12px; background: var(--fbop-panel2); border-bottom: 1px solid var(--fbop-line); flex: none; }
.fbop-tab {
  padding: 8px 14px 7px; border: 0; border-bottom: 2px solid transparent; border-radius: 7px 7px 0 0;
  background: transparent; color: var(--fbop-mute); font-size: var(--fbop-ui); font-weight: 500; cursor: pointer;
}
.fbop-tab:hover { color: var(--fbop-ink); }
.fbop-tab.active { color: var(--fbop-ink); background: var(--fbop-bg); border-bottom-color: var(--fbop-accent); }

.fbop-body { flex: 1; overflow: auto; padding: 14px 16px 22px; }
.fbop-pane { display: none; }
.fbop-pane.active { display: block; }

.fbop-group { border: 1px solid var(--fbop-line); border-radius: 11px; margin-bottom: 12px; overflow: hidden; }
.fbop-group-head {
  padding: 7px 11px; font-size: var(--fbop-label); font-weight: 600; letter-spacing: .04em;
  text-transform: uppercase; color: var(--fbop-mute); background: var(--fbop-panel2);
  border-bottom: 1px solid var(--fbop-line);
}
.fbop-group-body { padding: 11px 12px 12px; }

.fbop-row { display: flex; align-items: center; gap: 12px; padding: 7px 0; }
.fbop-row + .fbop-row { border-top: 1px solid color-mix(in srgb, var(--fbop-line) 70%, transparent); }
.fbop-row-label { flex: 1; min-width: 0; font-size: var(--fbop-ui); }
.fbop-row-hint { display: block; font-size: var(--fbop-label); color: var(--fbop-faint); margin-top: 2px; line-height: 1.45; }
.fbop-row-control { flex: none; display: flex; align-items: center; gap: 8px; }
.fbop-row.stack { display: block; }
.fbop-row.stack .fbop-row-control { margin-top: 8px; justify-content: flex-start; }

.fbop-switch { position: relative; width: 36px; height: 20px; flex: none; }
.fbop-switch input { position: absolute; opacity: 0; width: 100%; height: 100%; margin: 0; cursor: pointer; }
.fbop-switch span {
  position: absolute; inset: 0; border-radius: 999px; background: color-mix(in srgb, var(--fbop-ink) 14%, transparent);
  border: 1px solid var(--fbop-line); transition: background .14s;
}
.fbop-switch span::after {
  content: ''; position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%;
  background: var(--fbop-mute); transition: transform .14s, background .14s;
}
.fbop-switch input:checked + span { background: color-mix(in srgb, var(--fbop-accent) 55%, transparent); }
.fbop-switch input:checked + span::after { transform: translateX(16px); background: #fff; }
.fbop-switch input:focus-visible + span { outline: 2px solid var(--fbop-accent); outline-offset: 2px; }

.fbop-slider { width: 150px; accent-color: var(--fbop-accent); }
.fbop-num {
  width: 74px; padding: 5px 7px; border: 1px solid var(--fbop-line); border-radius: 6px;
  background: var(--fbop-bg); color: var(--fbop-ink); font-family: var(--fbop-mono); font-size: var(--fbop-ui);
  text-align: right; outline: none;
}
.fbop-num:focus { border-color: var(--fbop-accent); }
.fbop-unit { font-size: var(--fbop-label); color: var(--fbop-faint); }
.fbop-select {
  padding: 5px 7px; border: 1px solid var(--fbop-line); border-radius: 6px; background: var(--fbop-bg);
  color: var(--fbop-ink); font-size: var(--fbop-ui); outline: none;
}

.fbop-cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(196px, 1fr)); gap: 10px; }
.fbop-card {
  text-align: left; padding: 12px 13px; border: 1px solid var(--fbop-line); border-radius: 11px;
  background: var(--fbop-bg); color: var(--fbop-ink); cursor: pointer;
}
.fbop-card:hover { border-color: color-mix(in srgb, var(--fbop-accent) 60%, var(--fbop-line)); }
.fbop-card.active { border-color: var(--fbop-accent); background: color-mix(in srgb, var(--fbop-accent) 12%, var(--fbop-bg)); }
.fbop-card-name { font-size: var(--fbop-ui); font-weight: 600; margin-bottom: 4px; }
.fbop-card-blurb { font-size: var(--fbop-label); color: var(--fbop-mute); line-height: 1.5; }

.fbop-detect {
  font-size: var(--fbop-label); color: var(--fbop-mute); line-height: 1.6;
  padding: 9px 11px; border: 1px solid var(--fbop-line); border-radius: 9px; margin-bottom: 12px;
  background: color-mix(in srgb, var(--fbop-ink) 3%, transparent);
}
.fbop-detect b { color: var(--fbop-ink); font-weight: 600; }

.fbop-footer {
  flex: none; display: flex; align-items: center; gap: 9px; flex-wrap: wrap;
  padding: 9px 14px; background: var(--fbop-panel2); border-top: 1px solid var(--fbop-line);
}
.fbop-status { flex: 1; min-width: 140px; font-size: var(--fbop-label); color: var(--fbop-mute); line-height: 1.5; }
.fbop-status b { color: var(--fbop-ink); font-weight: 600; }
.fbop-dot { display: inline-block; width: 7px; height: 7px; border-radius: 50%; margin-right: 6px; background: var(--fbop-faint); vertical-align: middle; }
.fbop-dot.on { background: var(--fbop-good); }
.fbop-dot.warn { background: var(--fbop-warn); }
.fbop-btn {
  padding: 6px 12px; border: 1px solid var(--fbop-line); border-radius: 7px; background: var(--fbop-bg);
  color: var(--fbop-ink); font-size: var(--fbop-ui); font-weight: 600; cursor: pointer;
}
.fbop-btn:hover { border-color: var(--fbop-accent); }
.fbop-btn.primary { background: var(--fbop-accent); border-color: var(--fbop-accent); color: #0a0c10; }
.fbop-btn.danger:hover { border-color: var(--fbop-danger); color: var(--fbop-danger); }

.fbop-note { font-size: var(--fbop-label); color: var(--fbop-faint); line-height: 1.55; margin: 8px 2px 0; }
.fbop-note code { font-family: var(--fbop-mono); color: var(--fbop-mute); }

.fbop-toast {
  position: fixed; left: 50%; bottom: 26px; transform: translate(-50%, 8px);
  opacity: 0; pointer-events: none; transition: opacity .18s, transform .18s;
  padding: 8px 14px; border: 1px solid var(--fbop-line); border-radius: 9px;
  background: var(--fbop-panel2); color: var(--fbop-ink); font-size: var(--fbop-ui); font-weight: 600;
  z-index: 5; max-width: min(560px, calc(100vw - 60px));
}
.fbop-toast.show { opacity: 1; transform: translate(-50%, 0); }
`

  /* ------------------------------------------------------------------ build --- */

  var host = null
  var shadow = null
  var panel = null
  var root = null
  var railButton = null
  var toastEl = null
  var ui = null

  function toggleEl(path, hint) {
    var input = el('input', { type: 'checkbox', 'data-field': path })
    input.addEventListener('change', function () {
      setPath(state, path, input.checked)
      onField(null, path)
    })
    return el(
      'div',
      { class: 'fbop-row' },
      [
        el('div', { class: 'fbop-row-label' }, [document.createTextNode(labelFor(path)), hint ? el('span', { class: 'fbop-row-hint', text: hint }) : null]),
        el('div', { class: 'fbop-row-control' }, [el('label', { class: 'fbop-switch' }, [input, el('span')])]),
      ],
    )
  }

  function numberEl(path, min, max, step, unit, hint) {
    var range = el('input', { class: 'fbop-slider', type: 'range', min: min, max: max, step: step, 'data-field': path })
    var box = el('input', { class: 'fbop-num', type: 'number', min: min, max: max, step: step, 'data-field': path })
    var sync = function (value, from) {
      var v = clampInt(value, min, max)
      setPath(state, path, v)
      if (from !== range) range.value = String(v)
      if (from !== box) box.value = String(v)
      onField(null, path)
    }
    range.addEventListener('input', function () { sync(range.value, range) })
    box.addEventListener('change', function () { sync(box.value, box) })
    return el(
      'div',
      { class: 'fbop-row' },
      [
        el('div', { class: 'fbop-row-label' }, [document.createTextNode(labelFor(path)), hint ? el('span', { class: 'fbop-row-hint', text: hint }) : null]),
        el('div', { class: 'fbop-row-control' }, [range, box, unit ? el('span', { class: 'fbop-unit', text: unit }) : null]),
      ],
    )
  }

  function selectEl(path, options, hint) {
    var sel = el('select', { class: 'fbop-select', 'data-field': path })
    options.forEach(function (o) {
      sel.appendChild(el('option', { value: o.value, text: o.label }))
    })
    sel.addEventListener('change', function () {
      setPath(state, path, sel.value)
      onField(null, path)
    })
    return el(
      'div',
      { class: 'fbop-row' },
      [
        el('div', { class: 'fbop-row-label' }, [document.createTextNode(labelFor(path)), hint ? el('span', { class: 'fbop-row-hint', text: hint }) : null]),
        el('div', { class: 'fbop-row-control' }, [sel]),
      ],
    )
  }

  var LABELS = {
    'memory.enabled': ['Hold Freebuff to a memory cap', 'A hard working-set cap, applied to every Freebuff process.'],
    'memory.capMb': ['Cap', 'Megabytes per process.'],
    'memory.trimOnApply': ['Trim pages when the cap is applied', 'Evicts what is already over the cap instead of waiting for the next allocation.'],
    'memory.idleTrimMb': ['Idle trim', 'Hand back the pages of any process that drifts above this. 0 turns it off.'],
    'cpu.enabled': ['Limit how much CPU Freebuff can take', 'Priority, processor count, efficiency mode and the hard throttle.'],
    'cpu.priority': ['Scheduling priority', 'Idle is the strongest step down, and is what makes a busy machine stay responsive.'],
    'cpu.cores': ['Processors', 'Pins every Freebuff process to this many cores. 0 leaves the whole machine available.'],
    'cpu.ecoQos': ['Efficiency mode (EcoQoS)', 'Asks Windows to prefer the efficient cores and lower clocks. A hint, not a guarantee.'],
    'cpu.jobCapPercent': ['Hard CPU throttle', 'Percent of the WHOLE machine given to the browser process. Measured to work, but the renderers sit inside Chromium\u2019s own job and refuse to join ours, so this reaches one process.'],
    'windows.compact': ['Compact layout', 'Tightens the shell insets and the workspace corners.'],
    'windows.reduceMotion': ['Reduce motion', 'Stops animations and transitions.'],
    'windows.reduceEffects': ['Reduce visual effects', 'Drops backdrop blur and shadows - the work a weak GPU struggles with.'],
    'windows.thinScrollbars': ['Thin scrollbars', 'Narrower scrollbars, less repainting.'],
    'cleanup.cacheSweepMb': ['Cache budget', 'The guard deletes the oldest cache files whenever Freebuff\u2019s HTTP cache grows past this. 0 disables it.'],
    'cleanup.killOrphans': ['Close stray helper processes', 'Chromium helpers left behind without a browser process - memory nobody can use.'],
    'cleanup.reapIdleThreads': ['Reap idle threads', 'Pooled threads from finished work are closed where the platform allows it.'],
    'watchdog.enabled': ['Keep the limits applied', 'Electron spawns renderers after this page loads. Without the guard they would escape the cap.'],
    'watchdog.intervalSeconds': ['Re-check every', 'Seconds between passes.'],
  }

  function labelFor(path) {
    var l = LABELS[path]
    return l ? l[0] : path
  }
  function hintFor(path) {
    var l = LABELS[path]
    return l ? l[1] : ''
  }

  function group(title, rows) {
    return el('div', { class: 'fbop-group' }, [
      el('div', { class: 'fbop-group-head', text: title }),
      el('div', { class: 'fbop-group-body' }, rows),
    ])
  }

  /** One entry point for every control: the display settings are re-applied
   *  immediately, everything else is written out and picked up by the guard on
   *  its next tick. */
  function onField(section, path) {
    if (path && path.indexOf('windows.') === 0) applyCosmetics()
    changed()
    if (root) {
      root.refreshActive()
      root.refreshFooter()
    }
  }

  function buildPanel(showToast) {
    var panePresets = el('div', { class: 'fbop-pane active', 'data-pane': 'presets' })
    var paneMemory = el('div', { class: 'fbop-pane', 'data-pane': 'memory' })
    var paneCPU = el('div', { class: 'fbop-pane', 'data-pane': 'cpu' })
    var paneDisplay = el('div', { class: 'fbop-pane', 'data-pane': 'display' })
    var paneCleanup = el('div', { class: 'fbop-pane', 'data-pane': 'cleanup' })

    var detect = el('div', { class: 'fbop-detect' })
    var cards = el('div', { class: 'fbop-cards' })
    panePresets.appendChild(detect)
    panePresets.appendChild(cards)
    panePresets.appendChild(
      el('p', {
        class: 'fbop-note',
        html:
          'Presets only set the controls below - nothing is hidden. Everything is applied by a small background guard running outside Freebuff, ' +
          'because a page cannot cap another process\u2019s memory or CPU. It re-checks every <code>' +
          DEFAULTS.watchdog.intervalSeconds +
          '</code> seconds.',
      }),
    )

    paneMemory.appendChild(
      group('Memory', [
        toggleEl('memory.enabled', hintFor('memory.enabled')),
        // The step has to divide every value the presets use from the minimum,
        // or the range input silently snaps to a neighbouring value the user
        // never picked and the number field is marked invalid.
        numberEl('memory.capMb', 128, 8192, 4, 'MB', hintFor('memory.capMb')),
        toggleEl('memory.trimOnApply', hintFor('memory.trimOnApply')),
        numberEl('memory.idleTrimMb', 0, 8192, 10, 'MB', hintFor('memory.idleTrimMb')),
      ]),
    )

    paneCPU.appendChild(
      group('CPU', [
        toggleEl('cpu.enabled', hintFor('cpu.enabled')),
        selectEl(
          'cpu.priority',
          [
            { value: 'normal', label: 'Normal' },
            { value: 'below-normal', label: 'Below normal' },
            { value: 'idle', label: 'Idle' },
          ],
          hintFor('cpu.priority'),
        ),
        numberEl('cpu.cores', 0, cores(), 1, cores() > 0 ? 'of ' + cores() : '', hintFor('cpu.cores')),
        toggleEl('cpu.ecoQos', hintFor('cpu.ecoQos')),
        numberEl('cpu.jobCapPercent', 0, 100, 5, '%', hintFor('cpu.jobCapPercent')),
      ]),
    )

    paneDisplay.appendChild(
      group('Display', [
        toggleEl('windows.compact', hintFor('windows.compact')),
        toggleEl('windows.reduceMotion', hintFor('windows.reduceMotion')),
        toggleEl('windows.reduceEffects', hintFor('windows.reduceEffects')),
        toggleEl('windows.thinScrollbars', hintFor('windows.thinScrollbars')),
      ]),
    )
    paneDisplay.appendChild(
      el('p', {
        class: 'fbop-note',
        text: 'These four are the only settings this panel applies by itself. They are plain CSS in the page, so they take effect at once and come straight back off when you turn them off.',
      }),
    )

    paneCleanup.appendChild(
      group('Cleanup', [
        numberEl('cleanup.cacheSweepMb', 0, 4096, 50, 'MB', hintFor('cleanup.cacheSweepMb')),
        toggleEl('cleanup.killOrphans', hintFor('cleanup.killOrphans')),
        toggleEl('cleanup.reapIdleThreads', hintFor('cleanup.reapIdleThreads')),
      ]),
    )
    paneCleanup.appendChild(
      group('Guard', [
        toggleEl('watchdog.enabled', hintFor('watchdog.enabled')),
        numberEl('watchdog.intervalSeconds', 5, 300, 5, 's', hintFor('watchdog.intervalSeconds')),
      ]),
    )
    paneCleanup.appendChild(
      el('p', {
        class: 'fbop-note',
        text: 'The cache sweep only ever touches Freebuff\u2019s HTTP cache folders. Cookies, storage and settings are left alone.',
      }),
    )

    var panes = { presets: panePresets, memory: paneMemory, cpu: paneCPU, display: paneDisplay, cleanup: paneCleanup }

    var tabbar = el('div', { class: 'fbop-tabs' })
    var TABS = [
      ['presets', 'Presets'],
      ['memory', 'Memory'],
      ['cpu', 'CPU'],
      ['display', 'Display'],
      ['cleanup', 'Cleanup'],
    ]
    var tabButtons = {}
    TABS.forEach(function (t) {
      var b = el('button', { class: 'fbop-tab', type: 'button', text: t[1] })
      b.addEventListener('click', function () {
        selectTab(t[0])
      })
      tabButtons[t[0]] = b
      tabbar.appendChild(b)
    })

    var statusEl = el('div', { class: 'fbop-status' })
    var applyBtn = el('button', { class: 'fbop-btn primary', type: 'button', text: 'Apply now' })
    var offBtn = el('button', { class: 'fbop-btn danger', type: 'button', text: 'Turn everything off' })
    var footer = el('div', { class: 'fbop-footer' }, [statusEl, offBtn, applyBtn])

    applyBtn.addEventListener('click', function () {
      saveNow(true)
      showToast('Saved - the guard applies this within about ' + state.watchdog.intervalSeconds + ' seconds')
      refreshActive()
      refreshFooter()
    })
    offBtn.addEventListener('click', function () {
      state = normalize(clone(DEFAULTS))
      state.watchdog = { enabled: true, intervalSeconds: DEFAULTS.watchdog.intervalSeconds }
      state.preset = 'off'
      applyCosmetics()
      saveNow(true)
      refreshControls()
      refreshActive()
      refreshFooter()
      showToast('Everything turned off and saved')
    })

    panel = el('div', { class: 'fbop-panel' }, [
      el('div', { class: 'fbop-titlebar' }, [
        el('span', { html: GAUGE_ICON }),
        el('div', { class: 'fbop-titles' }, [
          el('div', { class: 'fbop-title', text: 'Freebuff Opti' }),
          el('div', { class: 'fbop-sub', text: 'RAM, CPU and clutter limits for slower machines' }),
        ]),
        (function () {
          var x = el('button', { class: 'fbop-x', type: 'button', 'aria-label': 'Close', text: '\u00d7' })
          x.addEventListener('click', function () {
            togglePage(false)
          })
          return x
        })(),
      ]),
      tabbar,
      el('div', { class: 'fbop-body' }, [panePresets, paneMemory, paneCPU, paneDisplay, paneCleanup]),
      footer,
    ])

    function selectTab(key) {
      for (var k in panes) {
        panes[k].classList.toggle('active', k === key)
        tabButtons[k].classList.toggle('active', k === key)
      }
    }

    function refreshControls() {
      var nodes = panel.querySelectorAll('[data-field]')
      for (var i = 0; i < nodes.length; i++) {
        var node = nodes[i]
        var value = getPath(state, node.dataset.field)
        if (node.type === 'checkbox') node.checked = !!value
        else node.value = String(value)
      }
    }

    function refreshActive() {
      Array.prototype.forEach.call(cards.children, function (card) {
        card.classList.toggle('active', card.dataset.preset === state.preset)
      })
    }

    function refreshFooter() {
      var on = []
      if (state.memory.enabled) on.push('memory \u2264 ' + state.memory.capMb + ' MB')
      if (state.cpu.enabled) {
        var c = state.cpu.priority
        if (state.cpu.cores > 0) c += ' on ' + state.cpu.cores + ' cores'
        if (state.cpu.ecoQos) c += ' + eco'
        if (state.cpu.jobCapPercent > 0) c += ' + ' + state.cpu.jobCapPercent + '% throttle'
        on.push('CPU ' + c)
      }
      var w = state.windows
      var cosmetic = []
      if (w.compact) cosmetic.push('compact')
      if (w.reduceMotion) cosmetic.push('no motion')
      if (w.reduceEffects) cosmetic.push('no effects')
      if (w.thinScrollbars) cosmetic.push('thin scrollbars')
      if (cosmetic.length) on.push(cosmetic.join(', '))

      if (!on.length) {
        statusEl.innerHTML = '<span class="fbop-dot"></span>Nothing limited. Freebuff is running at its own defaults.'
      } else {
        statusEl.innerHTML = '<span class="fbop-dot on"></span><b>' + escapeHTML(on.join(' \u00b7 ')) + '</b>'
      }
      if (!state.watchdog.enabled) {
        statusEl.innerHTML += ' <span class="fbop-dot warn"></span>the guard is off, so this applies only to processes that exist right now'
      }
    }

    function refreshDetect() {
      var nc = cores()
      var gb = deviceMemoryGB()
      var bits = ['<b>' + nc + '</b> logical processors']
      if (gb) bits.push('<b>' + gb + ' GB</b> of memory')
      var suggested = nc <= 4 || (gb && gb <= 8) ? 'potato' : nc <= 8 || (gb && gb <= 16) ? 'low' : 'battery'
      detect.innerHTML =
        'This machine reports ' +
        bits.join(' and ') +
        '. A machine like this is usually best served by the ' +
        '<b>' +
        PRESETS[suggested].label +
        '</b> preset.'
    }

    function refreshPresetCards() {
      cards.innerHTML = ''
      Object.keys(PRESETS).forEach(function (key) {
        var p = PRESETS[key]
        var card = el('button', { class: 'fbop-card', type: 'button', 'data-preset': key }, [
          el('div', { class: 'fbop-card-name', text: p.label }),
          el('div', { class: 'fbop-card-blurb', text: p.blurb }),
        ])
        card.addEventListener('click', function () {
          applyPreset(key)
        })
        cards.appendChild(card)
      })
    }

    function applyPreset(key) {
      var p = PRESETS[key]
      if (!p) return
      var watchdog = clone(state.watchdog)
      var enabledCpu = state.cpu.enabled || p.settings.cpu.enabled
      state = normalize(clone(p.settings))
      // A preset describes the limits, not whether the guard is running: keep
      // whatever interval the user chose.
      state.watchdog = watchdog
      if (key !== 'off') state.cpu.enabled = enabledCpu
      state.preset = key
      applyCosmetics()
      saveNow(true)
      refreshControls()
      refreshActive()
      refreshFooter()
      showToast(p.label + ' applied' + (key === 'off' ? '' : ' - the guard picks it up within about ' + state.watchdog.intervalSeconds + ' seconds'))
    }

    root = { selectTab: selectTab, refreshControls: refreshControls, refreshActive: refreshActive, refreshFooter: refreshFooter, refreshDetect: refreshDetect, refreshPresetCards: refreshPresetCards }
    return panel
  }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    })
  }

  /* --------------------------------------------------------------- the shell --- */

  function installRailButton() {
    if (railButton && railButton.isConnected) return true
    var rail = document.querySelector('.shell-navigation-top') || document.querySelector('.shell-navigation')
    if (!rail) return false
    var btn = document.createElement('button')
    btn.className = 'shell-nav-button'
    btn.setAttribute('data-fbop-rail', '1')
    btn.setAttribute('aria-label', 'Freebuff Opti')
    btn.setAttribute('title', 'Freebuff Opti')
    btn.innerHTML = GAUGE_ICON
    btn.addEventListener('click', function (e) {
      e.preventDefault()
      e.stopPropagation()
      if (ui) ui.toggle()
    })
    rail.appendChild(btn)
    railButton = btn
    return true
  }

  function watchRail() {
    var scheduled = false
    var recheck = function () {
      if (scheduled) return
      scheduled = true
      requestAnimationFrame(function () {
        scheduled = false
        installRailButton()
        fitPage()
      })
    }
    try {
      new MutationObserver(recheck).observe(document.body, { childList: true, subtree: true })
    } catch (e) {}
    // Belt and braces: React can replace the rail wholesale on navigation.
    setInterval(installRailButton, 1500)
  }

  /* Where the page belongs.
   *
   * Freebuff insets `.workspace-frame` by --shell-rail-width on the left and
   * --shell-inset on the right and bottom, below a --tabbar-height row. Those
   * live on `.desktop-shell`, not on :root, and this shadow host is a child of
   * <body> - so var() inside the shadow root silently takes its fallback.
   * Measuring the frame also tracks compact mode and a collapsed sidebar, which
   * recomputing the arithmetic would not. */
  function workspaceBox() {
    var best = null
    var nodes = document.querySelectorAll('.workspace-frame, .settings-frame')
    for (var i = 0; i < nodes.length; i++) {
      var r = nodes[i].getBoundingClientRect()
      if (r.width < 240 || r.height < 160) continue
      if (!best || r.width * r.height > best.width * best.height)
        best = { width: r.width, height: r.height, top: r.top, left: r.left, right: r.right, bottom: r.bottom }
    }
    return best
  }

  function measureWorkspace() {
    var frame = workspaceBox()
    if (frame) return frame
    var shell = document.querySelector('.desktop-shell') || document.documentElement
    var cs = getComputedStyle(shell)
    var num = function (name, fallback) {
      var v = parseFloat(cs.getPropertyValue(name))
      return isFinite(v) ? v : fallback
    }
    var box = shell.getBoundingClientRect()
    return {
      top: box.top + num('--tabbar-height', 48),
      left: box.left + num('--shell-rail-width', 52),
      right: box.right - num('--shell-inset', 8),
      bottom: box.bottom - num('--shell-inset', 8),
      corner: cs.getPropertyValue('--workspace-corner').trim(),
    }
  }

  function fitToWorkspace(node) {
    if (!node || !node.isConnected) return
    var box = measureWorkspace()
    var corner = box.corner
    if (!corner) {
      var shell = document.querySelector('.desktop-shell')
      if (shell) {
        try {
          corner = getComputedStyle(shell).getPropertyValue('--workspace-corner').trim()
        } catch (e) {}
      }
    }
    node.style.top = Math.round(box.top) + 'px'
    node.style.left = Math.round(box.left) + 'px'
    node.style.right = Math.max(0, Math.round(window.innerWidth - box.right)) + 'px'
    node.style.bottom = Math.max(0, Math.round(window.innerHeight - box.bottom)) + 'px'
    node.style.borderRadius = corner || '18px'
  }

  function fitPage() {
    fitToWorkspace(panel)
  }

  /** Clicking any other rail icon means the user left this page. */
  function closePageOnExternalNav() {
    document.addEventListener(
      'click',
      function (e) {
        var t = e.target
        var btn = t && t.closest ? t.closest('.shell-nav-button') : null
        if (!btn || btn === railButton) return
        if (ui) ui.toggle(false)
      },
      true,
    )
  }

  /* -------------------------------------------------------------- the report ---
   * What the guard last achieved, as the guard wrote it. The orchestrator
   * serves the ui directory off disk, so this is a same-origin read and the
   * CSP's `connect-src 'self'` allows it. A failed read is normal - the guard
   * may not have run yet - so it is never an error the user has to see.
   */
  var lastReport = null

  function fetchReport() {
    return fetch(STATE_URL + '?t=' + Date.now(), { cache: 'no-store' })
      .then(function (r) {
        if (!r.ok) throw new Error('status ' + r.status)
        return r.json()
      })
      .then(function (j) {
        lastReport = j
        return j
      })
      .catch(function () {
        return null
      })
  }

  function describeReport(doc) {
    if (!doc) return 'no report from the guard yet'
    var bits = [doc.processes + (doc.processes === 1 ? ' process' : ' processes')]
    if (doc.capped) bits.push(doc.capped + ' capped')
    if (doc.capFailed) bits.push(doc.capFailed + ' refused a cap')
    if (doc.prioritised) bits.push(doc.prioritised + ' reprioritised')
    if (doc.affinity) bits.push(doc.affinity + ' pinned')
    if (doc.ecoQos) bits.push(doc.ecoQos + ' in efficiency mode')
    if (doc.note) bits.push(doc.note)
    return bits.join(' \u00b7 ')
  }

  function mount(showToast) {
    if (document.querySelector('[data-fbop-host]')) return

    host = el('div', { 'data-fbop-host': '' })
    // Deliberately not `all: initial` - that would reset the --fbop-* tokens the
    // shadow stylesheet inherits from :host.
    host.style.cssText = 'position: fixed; z-index: 2147483600; right: 0; bottom: 0; width: 0; height: 0; pointer-events: none;'
    document.body.appendChild(host)
    shadow = host.attachShadow({ mode: 'open' })
    shadow.appendChild(el('style', { text: STYLES }))

    var toast = el('div', { class: 'fbop-toast' })
    toastEl = toast
    var toastTimer = null
    var say = function (msg) {
      toast.textContent = msg
      toast.classList.add('show')
      clearTimeout(toastTimer)
      toastTimer = setTimeout(function () {
        toast.classList.remove('show')
      }, 3600)
    }
    notifySave = say

    panel = buildPanel(say)
    shadow.appendChild(panel)
    shadow.appendChild(toast)

    root.refreshPresetCards()
    root.refreshControls()
    root.refreshActive()
    root.refreshDetect()
    root.refreshFooter()

    applyCosmetics()

    function togglePage(force) {
      var open = typeof force === 'boolean' ? force : !panel.classList.contains('open')
      if (open) fitToWorkspace(panel)
      panel.classList.toggle('open', open)
      if (railButton) {
        if (open) {
          // Only one destination should look selected: take the highlight off
          // the app's own rail entries. React restores its own on its next
          // navigation, so this never fights the shell.
          var others = document.querySelectorAll('.shell-nav-button[aria-current]')
          for (var i = 0; i < others.length; i++) {
            if (others[i] !== railButton) others[i].removeAttribute('aria-current')
          }
          railButton.setAttribute('aria-current', 'page')
        } else {
          railButton.removeAttribute('aria-current')
        }
      }
      try {
        localStorage.setItem(LS_OPEN, open ? '1' : '0')
      } catch (e) {}
      if (open) {
        fetchReport().then(function (r) {
          if (r) showToast('Guard: ' + describeReport(r))
        })
      }
    }

    ui = {
      toggle: togglePage,
      state: function () {
        return clone(state)
      },
      applyPreset: function (key) {
        var card = panel.querySelector('[data-preset="' + key + '"]')
        if (card) card.click()
      },
      report: function () {
        return lastReport
      },
      refreshReport: function () {
        return fetchReport()
      },
      cosmeticsCSS: function () {
        return cosmeticCSS(state)
      },
    }

    installRailButton()
    fitToWorkspace(panel)
    window.addEventListener('resize', fitPage)
    try {
      var frameEl = document.querySelector('.workspace-frame') || document.querySelector('.settings-frame')
      new ResizeObserver(fitPage).observe(frameEl || document.documentElement)
    } catch (e) {}

    var wantOpen = false
    try {
      wantOpen = localStorage.getItem(LS_OPEN) === '1'
    } catch (e) {}
    if (wantOpen) togglePage(true)

    // Flush any pending save if the window is closed mid-drag, and re-apply the
    // cosmetics if anything removed the style element.
    document.addEventListener('visibilitychange', function () {
      if (document.visibilityState === 'hidden' && saveTimer) saveNow(true)
    })
    window.addEventListener('beforeunload', function () {
      if (saveTimer) saveNow(true)
    })

    // Show what the guard last did, now and then after every guard interval.
    fetchReport()
    setInterval(function () {
      fetchReport().then(function () {
        if (panel.classList.contains('open')) root.refreshFooter()
      })
    }, Math.max(5, state.watchdog.intervalSeconds || 20) * 1000)
  }

  /* ------------------------------------------------------------------- boot --- */

  function boot() {
    if (!document.body) return
    try {
      // A count cookie with no readable body means a save is there but damaged.
      // Say so, and do not write the defaults over it until the user changes
      // something - loadSettings() never writes, and nothing below does either.
      var loaded = loadSettings()
      var damaged = loaded === null && savedDocumentExists()
      state = loaded || normalize(clone(DEFAULTS))
      mount()
      if (damaged) {
        notifySave('Your saved settings could not be read - nothing was overwritten')
      }
      closePageOnExternalNav()
      watchRail()
    } catch (err) {
      // Never let a UI failure take the host app down with it.
      console.error('[freebuff-opti] failed to mount', err)
    }
  }

  // Small surface for power users and for anything that wants to check the
  // panel without a renderer. Also what the sandbox exercises.
  window.__FREEBUFF_OPTI__ = {
    version: VERSION,
    defaults: function () {
      return clone(DEFAULTS)
    },
    presets: function () {
      return Object.keys(PRESETS)
    },
    open: function (force) {
      if (ui) ui.toggle(force)
    },
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot)
  } else {
    boot()
  }
})()
