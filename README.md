# Freebuff Opti

**RAM, CPU and clutter limits for Freebuff Desktop, on machines that do not have any to spare.**

Freebuff Opti adds one gauge icon to Freebuff's own sidebar rail. Clicking it opens a page in
the app's workspace where you can cap how much memory Freebuff may hold, how much CPU it may
take, and how much visual work it does - with one-click presets for a machine that is
genuinely struggling.

```
FreebuffOpti.exe
```

That is the whole install. It writes two files into your Freebuff install, starts a small
background guard, and gives you back an undo that restores everything.

> Unofficial community extension. Not made by, or endorsed by, Freebuff.

---

## Why the limits are not set by the page

The obvious way to build this would be a page script that calls the Win32 API. That cannot
work, and it is worth saying why, because it is the reason this project is shaped the way it
is:

- A Chromium **renderer is sandboxed**. It cannot open another process, let alone change its
  memory limits. Even if it could, `SetProcessWorkingSetSizeEx` on *itself* would only ever
  cover one of the nine or so processes Freebuff runs.
- The renderers that need capping **do not exist yet** when the page loads. Electron spawns
  them as you use the app.

So the panel does the two things a page can honestly do - it owns the settings and it applies
its own CSS - and a small Go guard, running outside the app, does everything else. The panel
saves to a cookie; the guard reads that cookie, applies the limits, and re-checks every 20
seconds so a renderer spawned a minute later is under the cap too.

The guard reads the cookie out of Chromium's own SQLite jar using **the Bun runtime that
Freebuff already ships**, and its query is restricted to `name like 'fbop%'`. It never touches
Freebuff's own cookies, and never touches another application's.

---

## What is actually enforced

Everything below was measured on Windows 11 against a real Freebuff install rather than
assumed, and the results are what shape the UI's wording.

| Control | Mechanism | What it really does |
| --- | --- | --- |
| **Memory cap** | `SetProcessWorkingSetSizeEx` with `QUOTA_LIMITS_HARDWS_MAX_ENABLE` | A **hard** cap, and it works on every process in the tree. A process holding 555 MB was pinned at 219 MB against a 220 MB limit and stayed pinned. |
| **Trim pages** | `EmptyWorkingSet` | Hands back the pages already over the cap, instead of waiting for the next allocation to trip it. |
| **Idle trim** | `EmptyWorkingSet` above a budget | The cheap half of the memory story: idle pages stop sitting in RAM. |
| **Priority** | `SetPriorityClass` | Verified to set *and* restore on every process, including the sandboxed ones. |
| **Processors** | `SetProcessAffinityMask` | Pins the tree to N cores, taken as a subset of the mask the daemon is already allowed to use. |
| **Efficiency mode** | `SetProcessInformation(ProcessPowerThrottling)` | EcoQoS. A hint, not a guarantee, and never the only thing holding a limit up. |
| **CPU throttle** | Job object, `JobObjectCpuRateControlInformation` | Real, but see the limitation below. |
| **Display** | Plain CSS in the page | The one thing the panel applies itself, and it comes straight back off. |
| **Cleanup** | Cache sweep + orphan reap | Deletes the oldest files in Freebuff's HTTP cache only. Cookies, storage and settings are untouched. |

### The one honest limitation

The CPU throttle reaches **the browser process only**.

Freebuff's renderers, GPU and utility processes are already inside Chromium's own job object,
and `AssignProcessToJobObject` answers *"Access is denied"* for every one of them - measured,
not guessed. The throttle therefore caps one process out of nine, and the panel's own copy
says so rather than implying otherwise.

What actually limits CPU for the whole tree is the priority class, the processor affinity and
EcoQoS, all of which are per-process calls and do apply everywhere.

---

## The panel

Five tabs, and nothing hidden behind a preset:

- **Presets** - Off, Low-end, **Potato**, Battery. The panel reads `navigator.hardwareConcurrency`
  and `navigator.deviceMemory` and tells you which one it would pick, and why.
- **Memory** - the cap, whether to trim on apply, and the idle-trim budget.
- **CPU** - priority, processor count, efficiency mode, and the hard throttle.
- **Display** - compact layout, reduce motion, reduce effects, thin scrollbars.
- **Cleanup** - cache budget, stray helper processes, idle threads, and the guard's own
  re-check interval.

The footer always says what is currently set, in words. `Apply now` writes immediately; the
guard picks the change up within about 20 seconds. `Turn everything off` returns Freebuff to
its own defaults and saves that.

Settings live in cookies named `fbop_*` - not `localStorage`, which is keyed by origin and
therefore by port, and Freebuff takes a new port on every launch. A save is written as a new
generation and published only when it is complete, so closing Freebuff mid-drag cannot lose
the previous settings.

---

## Command line

```
FreebuffOpti.exe                 Install the panel and start the guard
FreebuffOpti.exe --status        What is installed, what is applied, what is running
FreebuffOpti.exe --apply         Apply the saved settings once, then exit
FreebuffOpti.exe --clear         Release every limit, leave the panel installed
FreebuffOpti.exe --clean         Clear the cache and close stray helpers, then exit
FreebuffOpti.exe --repair        Rewrite the panel files without touching a setting
FreebuffOpti.exe --restart       Relaunch Freebuff so the panel loads
FreebuffOpti.exe --no-guard      Install the panel only: no guard, no logon entry
FreebuffOpti.exe --remove-watch  Stop the background guard and remove it from logon
FreebuffOpti.exe --uninstall     Remove everything and restore index.html
```

Plus `--path <dir>` to point at an install that is not auto-detected, `--open` to open the UI
folder, and `--quiet`.

`--status` reads the live process tree, so it is also the quickest way to see what Freebuff is
actually costing you:

```
  9 Freebuff processes, 1153 MB resident
    pid 16528  bun            246 MB
    pid 32336  child          221 MB
    pid 9384   child          215 MB
    ...
    pid 32520  browser         99 MB
```

---

## What it touches, and how to undo it

Nothing in `app.asar` is patched and no binary is modified. Only these are written:

```
<install>/resources/orchestrator/ui/index.html              (+ two marker comments)
<install>/resources/orchestrator/ui/assets/freebuff-opti.js  (the panel)
<install>/resources/orchestrator/ui/assets/freebuff-opti-status.json
<install>/resources/orchestrator/ui/.freebuff-opti.json      (manifest)
<install>/resources/orchestrator/ui/index.html.freebuff-opti-original.bak
%LOCALAPPDATA%\FreebuffOpti\                                 (guard, log, settings cache)
```

`index.html` is edited atomically, between two markers that keep a re-install idempotent, and
the original is backed up first. An automatic update that rewrites `index.html` is noticed
within a tick and the panel is put back.

`--uninstall` restores the backup, releases every limit, stops the guard and removes it from
`HKCU\...\CurrentVersion\Run`. If a Freebuff update has rewritten `index.html` since the
backup was taken, the backup is *kept* rather than put back over the newer file - the panel
only ever removes its own block.

---

## Auto-detection

The injector finds Freebuff by looking at the running process, then at the usual locations:
`%LOCALAPPDATA%\Programs\@codebufffreebuff-desktop`, the same under `%APPDATA%`, `Program
Files` and `Program Files (x86)`, under a few spellings. `--path` overrides all of it.

---

## Building

```bash
./build.sh          # node --check on the panel, go test, then dist/FreebuffOpti.exe
```

The panel source is a single file, `injector/assets/opti-engine.js`, and it is embedded into
the executable with `go:embed` - the `.exe` is self-contained.

The two ends of the panel-to-guard contract live in `injector/assets/opti-engine.js` and
`injector/store.go`, and they must agree on the cookie names and the count format.
`TestReadSettingsFromJarAgainstRealBun` runs the real reader against a real SQLite jar built
by a real Bun, so a rename on one side and not the other fails the build rather than quietly
reverting to "no settings saved yet" in the app.

### Trying it without touching Freebuff

`sandbox/fake-install` is a stub install tree, and `sandbox/demo.html` is a stand-in for
Freebuff's shell:

```bash
python -m http.server 8791          # then open http://127.0.0.1:8791/sandbox/demo.html
./dist/FreebuffOpti.exe --path sandbox/fake-install --no-guard
./dist/FreebuffOpti.exe --path sandbox/fake-install --status
./dist/FreebuffOpti.exe --path sandbox/fake-install --uninstall
```

`--no-guard` is there for exactly this: the panel is installed and nothing is applied and
nothing starts at logon.

---

## Licence

MIT. See [LICENSE](LICENSE).
