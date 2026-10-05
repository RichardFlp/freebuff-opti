// Freebuff Opti - injector and limit daemon.
//
// Installs a resource panel into a local Freebuff Desktop install and then
// keeps the machine's limits applied to it.
//
// How it works: Freebuff Desktop's renderer is served by a Bun orchestrator
// bound to 127.0.0.1. That server serves `resources/orchestrator/ui` straight
// off disk on every request and computes its Content-Security-Policy from the
// document it just read - so an extra same-origin <script> is allowed and takes
// effect on the next page load. Nothing in app.asar is patched and no binary is
// modified; the only files touched are:
//
//	resources/orchestrator/ui/index.html           (+ injected <script> tag)
//	resources/orchestrator/ui/assets/freebuff-opti.js  (the panel)
//
// The limits themselves are not something a web page can set. A renderer cannot
// cap another process's working set, and Chromium's renderers are sandboxed, so
// the hard limits live in this process instead: it runs as a small background
// guard, reads the settings the panel saved, and applies them to every Freebuff
// process it can find - including the renderers that appear after the panel has
// loaded. The panel is the UI; this is the enforcement.
//
// Why the panel's settings come back through the cookie jar, and what each
// control is actually worth, is documented in store.go and limits.go.
package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed assets/opti-engine.js
var engineJS []byte

const (
	version      = "1.1.2"
	markerStart  = "<!-- freebuff-opti:start -->"
	markerEnd    = "<!-- freebuff-opti:end -->"
	engineName   = "freebuff-opti.js"
	backupSuffix = ".freebuff-opti-original.bak"
	manifestName = ".freebuff-opti.json"
	// statusName is the file the guard writes on every pass so the panel can
	// show what was actually applied. It lives in the ui/assets folder because
	// that is the one directory the page can read: the orchestrator serves it
	// off disk and the CSP allows a same-origin fetch.
	statusName = "freebuff-opti-status.json"

	// watchInterval is long enough that the process list is not re-read several
	// times a second, short enough that a renderer spawned a moment ago is
	// under its cap before the user notices the machine lagging.
	watchInterval = 20 * time.Second
	// watchSettle is how long a freshly written index.html is left alone: an
	// update is still writing when we first see the file.
	watchSettle = 1500 * time.Millisecond
	// detachedFlag starts the guard without a console.
	detachedFlag = 0x00000008
)

var (
	colBold  = ""
	colDim   = ""
	colGreen = ""
	colRed   = ""
	colCyan  = ""
	colReset = ""
)

// ---------------------------------------------------------------- console ---

func enableVT() bool {
	if runtime.GOOS != "windows" {
		return true
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getStdHandle := kernel32.NewProc("GetStdHandle")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	const stdOutputHandle = ^uintptr(10) // -11
	h, _, _ := getStdHandle.Call(stdOutputHandle)
	if h == 0 || h == uintptr(^uintptr(0)) {
		return false
	}
	var mode uint32
	if r, _, _ := getConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	const enableVirtualTerminalProcessing = 0x0004
	if r, _, _ := setConsoleMode.Call(h, uintptr(mode|enableVirtualTerminalProcessing)); r == 0 {
		return false
	}
	return true
}

func initColors() {
	if enableVT() {
		colBold = "\x1b[1m"
		colDim = "\x1b[2m"
		colGreen = "\x1b[32m"
		colRed = "\x1b[31m"
		colCyan = "\x1b[36m"
		colReset = "\x1b[0m"
	}
}

func banner() {
	fmt.Printf("%s\n", colBold+"Freebuff Opti"+colReset+" "+colDim+"v"+version+colReset)
	fmt.Printf("%s\n", colDim+"RAM, CPU and clutter limits for Freebuff Desktop."+colReset)
	fmt.Printf("%s\n\n", colDim+"Unofficial community extension - not made by Freebuff."+colReset)
}

func ok(msg string, a ...any) {
	fmt.Printf("  %s[ ok ]%s %s\n", colGreen, colReset, fmt.Sprintf(msg, a...))
}
func info(msg string, a ...any) {
	fmt.Printf("  %s[ .. ]%s %s\n", colCyan, colReset, fmt.Sprintf(msg, a...))
}
func warn(msg string, a ...any) {
	fmt.Printf("  %s[ !! ]%s %s\n", colRed, colReset, fmt.Sprintf(msg, a...))
}
func step(msg string, a ...any) { fmt.Printf("\n%s%s%s\n", colBold, fmt.Sprintf(msg, a...), colReset) }

// --------------------------------------------------------------- locating ---

func runningExecutable() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		`(Get-Process Freebuff -ErrorAction SilentlyContinue | Where-Object { $_.Path } | Select-Object -First 1).Path`)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isInstallDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "resources", "orchestrator", "ui", "index.html"))
	return err == nil
}

func candidateDirs() []string {
	var out []string
	if exe := runningExecutable(); exe != "" {
		out = append(out, filepath.Dir(exe))
	}
	local := os.Getenv("LOCALAPPDATA")
	roaming := os.Getenv("APPDATA")
	progFiles := os.Getenv("ProgramFiles")
	progFilesX86 := os.Getenv("ProgramFiles(x86)")
	names := []string{
		"@codebufffreebuff-desktop",
		"freebuff-desktop",
		"Freebuff",
		"Freebuff Desktop",
		"freebuff",
	}
	for _, root := range []string{local, roaming, progFiles, progFilesX86} {
		if root == "" {
			continue
		}
		for _, n := range names {
			out = append(out, filepath.Join(root, "Programs", n))
			out = append(out, filepath.Join(root, n))
		}
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, wd)
	}
	return out
}

func findInstallDir(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		if !isInstallDir(abs) {
			return "", fmt.Errorf("%s does not look like a Freebuff install (no resources/orchestrator/ui/index.html)", abs)
		}
		return abs, nil
	}
	seen := map[string]bool{}
	for _, c := range candidateDirs() {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if isInstallDir(c) {
			return c, nil
		}
	}
	return "", errors.New("could not find a Freebuff Desktop install; pass --path <install dir>")
}

// freebuffProfileDir is where Chromium keeps the app's profile: cookies, cache
// and Local State.
func freebuffProfileDir() string {
	for _, root := range []string{os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA")} {
		if root == "" {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := strings.ToLower(e.Name())
			if !strings.Contains(name, "freebuff") {
				continue
			}
			p := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(p, "Preferences")); err == nil {
				return p
			}
		}
	}
	return ""
}

// profileCookieDBs lists every cookie jar in the Freebuff profile. Only jars
// that actually contain our prefix are returned, so no other application's
// cookies are ever opened.
func profileCookieDBs() []string {
	base := freebuffProfileDir()
	if base == "" {
		return nil
	}
	candidates := []string{filepath.Join(base, "Network", "Cookies")}
	if parts, err := os.ReadDir(filepath.Join(base, "Partitions")); err == nil {
		for _, p := range parts {
			if !p.IsDir() {
				continue
			}
			candidates = append(candidates, filepath.Join(base, "Partitions", p.Name(), "Network", "Cookies"))
		}
	}
	var out []string
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(cookiePrefix+"_")) {
			out = append(out, p)
		}
	}
	return out
}

func bunBinary(install string) string {
	for _, p := range []string{
		filepath.Join(install, "resources", "bun", "bun.exe"),
		filepath.Join(install, "resources", "bun", "bun-baseline.exe"),
		filepath.Join(install, "resources", "orchestrator", "bun.exe"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("bun"); err == nil {
		return p
	}
	return ""
}

// ---------------------------------------------------------------- manifest ---

type manifest struct {
	Version     string `json:"version"`
	InstalledAt string `json:"installedAt"`
	OriginalSHA string `json:"originalIndexSha256"`
	HadBackup   bool   `json:"hadBackup"`
}

func manifestPath(ui string) string { return filepath.Join(ui, manifestName) }
func indexPath(ui string) string    { return filepath.Join(ui, "index.html") }
func assetsDir(ui string) string    { return filepath.Join(ui, "assets") }
func backupPath(ui string) string   { return indexPath(ui) + backupSuffix }

func readManifest(ui string) *manifest {
	b, err := os.ReadFile(manifestPath(ui))
	if err != nil {
		return nil
	}
	var m manifest
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".fbop-*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// Flush the contents before the rename, so the rename can never publish a
	// file with the new name and the old bytes.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Rename(temp, path)
	}
	src, err := syscall.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	dst, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	moveFileEx := syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
	const moveFileReplaceExisting = 0x1
	const moveFileWriteThrough = 0x8
	result, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(dst)),
		moveFileReplaceExisting|moveFileWriteThrough)
	if result == 0 {
		return fmt.Errorf("atomically replacing %s: %w", path, callErr)
	}
	return nil
}

// markerBounds rejects partial, duplicated and out-of-order injection markers.
func markerBounds(html string) (int, int, bool, error) {
	starts := strings.Count(html, markerStart)
	ends := strings.Count(html, markerEnd)
	if starts == 0 && ends == 0 {
		return -1, -1, false, nil
	}
	if starts != 1 || ends != 1 {
		return -1, -1, false, errors.New("index.html has duplicated or incomplete Freebuff Opti markers")
	}
	start := strings.Index(html, markerStart)
	end := strings.Index(html, markerEnd)
	if end < start+len(markerStart) {
		return -1, -1, false, errors.New("index.html has out-of-order Freebuff Opti markers")
	}
	return start, end + len(markerEnd), true, nil
}

func stripInjected(html string) (string, bool, error) {
	start, end, found, err := markerBounds(html)
	if err != nil || !found {
		return html, found, err
	}
	suffix := html[end:]
	if strings.HasPrefix(suffix, "\r\n") {
		suffix = suffix[2:]
	} else if strings.HasPrefix(suffix, "\n") {
		suffix = suffix[1:]
	}
	return html[:start] + suffix, true, nil
}

// ------------------------------------------------------------------ inject ---

func scriptBlock(ui string) string {
	nl := "\n"
	if raw, err := os.ReadFile(indexPath(ui)); err == nil && strings.Contains(string(raw), "\r\n") {
		nl = "\r\n"
	}
	var b strings.Builder
	b.WriteString(markerStart + nl)
	b.WriteString(`  <script src="./assets/freebuff-opti.js" data-freebuff-opti="` + version + `"></script>` + nl)
	b.WriteString(markerEnd)
	return b.String()
}

// inject is idempotent: re-running replaces the marker block rather than
// stacking a second one.
func inject(ui string, quiet bool) error {
	idx := indexPath(ui)
	original, err := os.ReadFile(idx)
	if err != nil {
		return fmt.Errorf("reading index.html: %w", err)
	}
	html := string(original)

	if !strings.Contains(strings.ToLower(html), "freebuff") && !strings.Contains(html, "startup-recovery") {
		return errors.New("index.html does not look like the Freebuff UI; refusing to modify it")
	}

	start, end, alreadyInjected, err := markerBounds(html)
	if err != nil {
		return err
	}

	pristine := original
	if b, readErr := os.ReadFile(backupPath(ui)); readErr == nil {
		pristine = b
	} else if alreadyInjected {
		stripped, _, err := stripInjected(html)
		if err != nil {
			return err
		}
		pristine = []byte(stripped)
	}
	if !alreadyInjected {
		if _, statErr := os.Stat(backupPath(ui)); os.IsNotExist(statErr) {
			if err := atomicWriteFile(backupPath(ui), original, 0o644); err != nil {
				return fmt.Errorf("writing backup: %w", err)
			}
			if !quiet {
				ok("Backed up the original index.html")
			}
		} else if statErr != nil {
			return fmt.Errorf("checking backup: %w", statErr)
		}
	}

	block := scriptBlock(ui)
	var updated string
	if alreadyInjected {
		updated = html[:start] + block + html[end:]
	} else if k := strings.LastIndex(html, "</body>"); k != -1 {
		updated = html[:k] + block + "\n" + html[k:]
	} else {
		updated = html + "\n" + block + "\n"
	}

	if err := os.MkdirAll(assetsDir(ui), 0o755); err != nil {
		return fmt.Errorf("creating assets dir: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(assetsDir(ui), engineName), engineJS, 0o644); err != nil {
		return fmt.Errorf("writing the panel: %w", err)
	}
	if err := writeManifest(ui, pristine); err != nil {
		return err
	}
	if err := atomicWriteFile(idx, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("writing index.html: %w", err)
	}
	return nil
}

func writeManifest(ui string, original []byte) error {
	hadBackup := true
	if _, err := os.Stat(backupPath(ui)); err != nil {
		hadBackup = false
	}
	// InstalledAt is the moment Freebuff Opti first went into this install, and
	// it is deliberately not refreshed by a re-inject: the panel's removal
	// request is only honoured while it is newer than the injection it belongs
	// to (see uninstallRequestPending), so an update that re-injects the panel
	// must not quietly invalidate a request the user just made. A genuine
	// reinstall starts from a manifest that uninstall() removed, so it gets a
	// fresh timestamp and the previous request dies with the previous install.
	installedAt := time.Now().UTC().Format(time.RFC3339)
	if prev := readManifest(ui); prev != nil && prev.InstalledAt != "" {
		installedAt = prev.InstalledAt
	}
	m := manifest{
		Version:     version,
		InstalledAt: installedAt,
		OriginalSHA: sha256Hex(original),
		HadBackup:   hadBackup,
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return atomicWriteFile(manifestPath(ui), b, 0o644)
}

// --------------------------------------------------------------- uninstall ---

func uninstall(ui string, quiet bool) error {
	idx := indexPath(ui)
	html, err := os.ReadFile(idx)
	if err != nil {
		return err
	}
	stripped, found, err := stripInjected(string(html))
	if err != nil {
		return err
	}
	backup, backupErr := os.ReadFile(backupPath(ui))
	m := readManifest(ui)
	restored := false
	if backupErr == nil && m != nil && m.OriginalSHA == sha256Hex(backup) {
		matchesBackup := stripped == string(backup) || stripped == string(backup)+"\n" || stripped == string(backup)+"\r\n"
		if matchesBackup {
			if err := atomicWriteFile(idx, backup, 0o644); err != nil {
				return err
			}
			if err := os.Remove(backupPath(ui)); err != nil && !os.IsNotExist(err) {
				return err
			}
			restored = true
		} else if found {
			if err := atomicWriteFile(idx, []byte(stripped), 0o644); err != nil {
				return err
			}
			if !quiet {
				warn("Removed Freebuff Opti without restoring the older backup; index.html contains newer changes")
			}
		}
	} else if found {
		if err := atomicWriteFile(idx, []byte(stripped), 0o644); err != nil {
			return err
		}
	}
	if !found && !quiet {
		info("No Freebuff Opti marker was present in index.html")
	}
	if err := os.Remove(filepath.Join(assetsDir(ui), engineName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", engineName, err)
	}
	_ = os.Remove(filepath.Join(assetsDir(ui), statusName))
	if err := os.Remove(manifestPath(ui)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if !quiet {
		ok("Removed the injected script and the panel")
		if restored {
			ok("Restored the original index.html")
		}
	}
	return nil
}

// ------------------------------------------------------------ remove it all ---

// uninstallRequestPending reports whether the panel has asked to be removed, and
// returns the timestamp of that request.
//
// The panel cannot delete anything: it is a sandboxed renderer with no filesystem
// access, so the Uninstall tab leaves a cookie instead and the guard - which is
// an ordinary process - does the deleting. See store.go for the channel.
//
// A request is honoured only while it is newer than the injection it belongs to.
// The cookie outlives the removal (nothing can safely write Chromium's live
// cookie jar from outside, so the daemon never tries), and without that
// comparison installing Freebuff Opti again would be undone by the previous
// install's dying request - a loop the user would have no way to escape.
func uninstallRequestPending(install string) (string, bool) {
	ui := filepath.Join(install, "resources", "orchestrator", "ui")
	m := readManifest(ui)
	if m == nil {
		// No manifest means no injection the guard owns; the next tick writes a
		// fresh one, and the request is older than that by definition.
		return "", false
	}
	raw, err := readUninstallRequest(install)
	if err != nil || raw == "" {
		return "", false
	}
	// Epoch milliseconds, which is what the panel writes: digits need no cookie
	// escaping, so what is read here is what was written there. (An ISO string
	// would arrive with its colons percent-encoded.)
	ms, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		// A damaged timestamp must not be able to trigger a removal, for the
		// same reason: it would fire again on every future install.
		return "", false
	}
	req := time.UnixMilli(ms)
	if installed, err := time.Parse(time.RFC3339, m.InstalledAt); err == nil && !req.After(installed) {
		return "", false
	}
	return strings.TrimSpace(raw), true
}

// scheduleSelfRemoval deletes this executable's folder a moment after this
// process exits. Windows will not delete a running image but will happily rename
// one, which is why removeWatch() leaves at most one file behind - the guard's
// own .exe - and why the leftover has to be cleaned up from outside. Nothing
// happens unless this process is the installed guard: an installer the user
// downloaded is their file, not ours to delete.
// scheduleDirRemoval arranges for the guard's folder to be taken away from
// outside, a moment from now. A process cannot delete its own image, and
// os.RemoveAll gives up at the first locked file, so anything still in there
// has to be waited out by something else. ping is the sleep: unlike `timeout`,
// it needs no console and no redirect.
func scheduleDirRemoval() {
	if runtime.GOOS != "windows" {
		return
	}
	dir := watchDir()
	if dir == "" {
		return
	}
	script := fmt.Sprintf(`ping -n 4 127.0.0.1 >nul & rmdir /s /q "%s" >nul 2>&1`, dir)
	cmd := exec.Command("cmd.exe", "/c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedFlag}
	if err := cmd.Start(); err != nil {
		return
	}
	_ = cmd.Process.Release()
}

// scheduleSelfRemoval is scheduleDirRemoval for the case where this process IS
// the installed guard. An installer the user downloaded is their file, and is
// left exactly where it is.
func scheduleSelfRemoval() {
	if selfImage == "" {
		return
	}
	dir := watchDir()
	if dir == "" || !strings.EqualFold(filepath.Clean(filepath.Dir(selfImage)), filepath.Clean(dir)) {
		return
	}
	scheduleDirRemoval()
}

// removeEverything is the one place that takes the tool back off the machine:
// release the limits, remove the panel and restore the UI file, then stop the
// guard and take its files away. --uninstall runs it, and it is what the guard
// runs when the panel's Uninstall tab asks.
func removeEverything(ui string, quiet bool) error {
	// Scheduled before anything else touches this image. removeWatch() renames a
	// running guard rather than deleting it (Windows allows a rename where it
	// refuses a delete), and once that has happened there is no way left to ask
	// where we are: os.Executable() has nothing to report.
	scheduleSelfRemoval()
	clearLimits(quiet)
	if err := uninstall(ui, quiet); err != nil {
		return err
	}
	removeWatch()
	return nil
}

// ------------------------------------------------------------------ limits ---

// applyState is what the last application pass actually achieved, which is what
// --status reports and what the panel's next read reflects.
type applyState struct {
	Version     string `json:"version"`
	At          string `json:"at"`
	Processes   int    `json:"processes"`
	Capped      int    `json:"capped"`
	CapFailed   int    `json:"capFailed"`
	Prioritised int    `json:"prioritised"`
	Affinity    int    `json:"affinity"`
	EcoQoS      int    `json:"ecoQos"`
	JobAssigned int    `json:"jobAssigned"`
	JobRefused  int    `json:"jobRefused"`
	Note        string `json:"note"`
	// Guarded says whether a background guard is installed to act on the next
	// removal request. It is how the panel's Uninstall tab can tell "the guard
	// has not looked yet" apart from "nothing is listening at all".
	Guarded bool `json:"guarded"`
	// UninstallPending mirrors the panel's removal request, so the tab can show
	// that the request arrived instead of guessing from a silent wait.
	UninstallPending bool `json:"uninstallPending"`
}

func statePath(dir string) string { return filepath.Join(dir, "applied.json") }

func writeApplyState(dir string, st applyState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = atomicWriteFile(statePath(dir), b, 0o644)
}

// writeStatusFile mirrors the last pass into the UI folder, where the panel reads
// it with a same-origin fetch. The page cannot read the guard's state directory,
// and writing a cookie from Go would mean writing Chromium's cookie database -
// this is the one channel that is both safe and already permitted by the app's
// Content-Security-Policy (`connect-src 'self'`).
func writeStatusFile(install string, st applyState) {
	dir := filepath.Join(install, "resources", "orchestrator", "ui", "assets")
	if _, err := os.Stat(dir); err != nil {
		return
	}
	st.Version = version
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = atomicWriteFile(filepath.Join(dir, statusName), b, 0o644)
}

func readApplyState(dir string) *applyState {
	b, err := os.ReadFile(statePath(dir))
	if err != nil {
		return nil
	}
	var st applyState
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

// clearLimits puts every Freebuff process back the way it was: no working-set
// maximum, normal priority, the full processor set, EcoQoS off. It is what
// uninstall and --clear run, and it is deliberately total rather than
// best-effort, because a cap that outlives the tool that set it is a trap.
func clearLimits(quiet bool) int {
	tree, err := processTree()
	if err != nil {
		if !quiet {
			warn("Could not list the Freebuff processes: %v", err)
		}
		return 0
	}
	system := systemMask()
	cleared := 0
	for pid := range tree {
		done := false
		if err := clearWorkingSetCap(pid); err == nil {
			done = true
		}
		if err := setPriority(pid, priorityNormal); err == nil {
			done = true
		}
		if system != 0 {
			if err := setAffinity(pid, system); err == nil {
				done = true
			}
		}
		if err := setEcoQoS(pid, false); err == nil {
			done = true
		}
		if done {
			cleared++
		}
	}
	if !quiet && cleared > 0 {
		ok("Released the limits on %d Freebuff processes", cleared)
	}
	return cleared
}

// applyLimits is the whole point of the daemon: read the settings the panel
// saved, then impose them on every Freebuff process, including the ones that
// did not exist when the panel loaded.
func applyLimits(install string, quiet bool) (applyState, error) {
	st := applyState{At: time.Now().UTC().Format(time.RFC3339)}
	st.Guarded = guardInstalled()
	if _, pending := uninstallRequestPending(install); pending {
		st.UninstallPending = true
	}

	doc, err := readSettingsFromCookies(install)
	if err != nil && !quiet {
		warn("Could not read the panel's settings: %v", err)
	}
	if doc == nil {
		// No saved document yet. Fall back to whatever the daemon applied last,
		// so a restart of the guard does not silently release a cap.
		if doc = readSavedSettings(); doc == nil {
			st.Note = "no settings saved yet - nothing applied"
			return st, nil
		}
	}
	if err := saveSettings(doc); err != nil && !quiet {
		warn("Could not cache the settings: %v", err)
	}

	memory := doc.Memory != nil && doc.Memory.Enabled
	cpu := doc.CPU != nil && doc.CPU.Enabled

	tree, err := processTree()
	if err != nil {
		return st, fmt.Errorf("listing the Freebuff processes: %w", err)
	}
	st.Processes = len(tree)

	cores := logicalCPUs()
	system := systemMask()

	// The job object is created once per pass. It is attached to the browser
	// process only: Electron's own job already owns every renderer, GPU and
	// utility process, and AssignProcessToJobObject answers "Access is denied"
	// for each of them. Closing the handle at the end of the pass releases it
	// again, which is why this is re-established rather than held open.
	var job syscall.Handle
	var browserPID int
	for pid := range tree {
		if isBrowserProcess(pid, tree) {
			browserPID = pid
			break
		}
	}
	if cpu && doc.CPU.JobCapPercent > 0 && browserPID != 0 {
		if j, err := createJob(); err == nil {
			job = j
			if err := setJobCPUCap(job, doc.CPU.JobCapPercent, cores); err != nil && !quiet {
				warn("Could not set the CPU throttle: %v", err)
			}
		}
	}

	for pid := range tree {
		if memory && doc.Memory.CapMB > 0 {
			if err := setWorkingSetCap(pid, doc.Memory.CapMB*1024*1024); err != nil {
				st.CapFailed++
			} else {
				st.Capped++
			}
			if doc.Memory.TrimOnApply {
				trimWorkingSet(pid)
			}
		}
		if cpu {
			if class, ok := priorityClassFor(doc.CPU.Priority); ok {
				if err := setPriority(pid, class); err == nil {
					st.Prioritised++
				}
			}
			if doc.CPU.Cores > 0 {
				mask := affinityMaskFor(doc.CPU.Cores, system)
				if err := setAffinity(pid, mask); err == nil {
					st.Affinity++
				}
			}
			if doc.CPU.EcoQoS {
				if err := setEcoQoS(pid, true); err == nil {
					st.EcoQoS++
				}
			}
			if job != 0 {
				if err := assignToJob(job, pid); err == nil {
					st.JobAssigned++
				} else if pid == browserPID {
					st.JobRefused++
				}
			}
		}
	}
	if job != 0 {
		closeHandle(job)
	}

	// The hard CPU throttle is real, but it only ever reaches the browser
	// process. Say so rather than implying the whole tree is capped.
	if doc.CPU != nil && doc.CPU.JobCapPercent > 0 && st.JobAssigned <= 1 {
		st.Note = "the CPU throttle reaches the browser process only; the renderers are inside Chromium's own job"
	}

	if doc.Watchdog != nil && doc.Watchdog.Enabled && doc.Memory != nil && doc.Memory.IdleTrimMB > 0 {
		trimIdle(tree, doc.Memory.IdleTrimMB)
	}

	writeApplyState(optiDir(), st)
	writeStatusFile(install, st)
	return st, nil
}

// describeKind is what --status prints in the process table. meta is
// {ppid, name, working MB} - see processTree. Every Electron helper shares the
// Freebuff.exe image name, so the name alone cannot tell the browser process
// from a renderer and isBrowserProcess has to.
func describeKind(pid int, meta []string, tree map[int][]string) string {
	if len(meta) < 2 {
		return "?"
	}
	name := strings.TrimSuffix(strings.ToLower(meta[1]), ".exe")
	if !strings.HasPrefix(name, "freebuff") {
		return name
	}
	if isBrowserProcess(pid, tree) {
		return "browser"
	}
	return "child"
}

// isBrowserProcess reports whether a pid is the Electron browser process: the
// one process in the tree with no `--type=` switch on its command line.
func isBrowserProcess(pid int, tree map[int][]string) bool {
	meta, ok := tree[pid]
	if !ok || len(meta) < 2 {
		return false
	}
	name := strings.ToLower(meta[1])
	if !strings.HasPrefix(name, "freebuff") {
		return false
	}
	// The parent of a browser process is not itself in the tree.
	if ppid, err := strconv.Atoi(meta[0]); err == nil {
		if _, parentIsFreebuff := tree[ppid]; parentIsFreebuff {
			return false
		}
	}
	return true
}

// trimIdle hands back the pages of any process that has drifted above the idle
// budget. It is the cheap half of the memory story: it does not stop a process
// allocating, it just stops idle pages from sitting in RAM.
func trimIdle(tree map[int][]string, budgetMB int) {
	for pid, meta := range tree {
		if len(meta) < 3 {
			continue
		}
		mb, err := strconv.Atoi(meta[2])
		if err != nil || mb <= budgetMB {
			continue
		}
		trimWorkingSet(pid)
	}
}

func priorityClassFor(name string) (uintptr, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "idle":
		return priorityIdle, true
	case "below-normal", "belownormal", "below":
		return priorityBelowNormal, true
	case "normal":
		return priorityNormal, true
	default:
		return 0, false
	}
}

// ----------------------------------------------------------------- cleanup ---

// cacheSizeMB reports how much Freebuff's HTTP cache is holding.
func cacheSizeMB() int {
	base := freebuffProfileDir()
	if base == "" {
		return 0
	}
	total := int64(0)
	for _, dir := range []string{"Cache", "Code Cache", "GPUCache", "DawnGraphiteCache", "DawnWebGPUCache"} {
		root := filepath.Join(base, dir)
		_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			total += info.Size()
			return nil
		})
	}
	return int(total / (1024 * 1024))
}

// sweepCache brings the cache back under budget by deleting the oldest files
// first. Chromium's cache is a pure cache - losing it costs a re-download, not
// data - so this is safe in a way that touching the profile's other
// directories would not be.
func sweepCache(budgetMB int, quiet bool) (int, error) {
	base := freebuffProfileDir()
	if base == "" {
		return 0, errors.New("Freebuff's profile folder was not found")
	}
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var files []entry
	var total int64
	for _, dir := range []string{"Cache", "Code Cache", "GPUCache", "DawnGraphiteCache", "DawnWebGPUCache"} {
		root := filepath.Join(base, dir)
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			files = append(files, entry{path: path, size: info.Size(), mod: info.ModTime()})
			total += info.Size()
			return nil
		})
	}
	if total <= int64(budgetMB)*1024*1024 {
		return 0, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	target := total - int64(budgetMB)*1024*1024
	var removed int64
	for _, f := range files {
		if removed >= target {
			break
		}
		if err := os.Remove(f.path); err == nil {
			removed += f.size
		}
	}
	if !quiet && removed > 0 {
		ok("Cleared %d MB from Freebuff's cache", int(removed/(1024*1024)))
	}
	return int(removed / (1024 * 1024)), nil
}

// killOrphans closes Chromium helper processes whose browser process is gone.
// They cannot be reused and they hold a working set, so they are the cheapest
// memory a potato PC can get back.
func killOrphans(quiet bool) int {
	tree, err := processTree()
	if err != nil {
		return 0
	}
	hasBrowser := false
	for pid := range tree {
		if isBrowserProcess(pid, tree) {
			hasBrowser = true
			break
		}
	}
	if hasBrowser {
		return 0
	}
	killed := 0
	for pid := range tree {
		h, err := openProcessFor(pid, processTerminate)
		if err != nil {
			continue
		}
		r, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("TerminateProcess").Call(uintptr(h), 1)
		closeHandle(h)
		if r != 0 {
			killed++
		}
	}
	if killed > 0 && !quiet {
		ok("Closed %d stray Freebuff helper processes", killed)
	}
	return killed
}

// --------------------------------------------------------- settings cache ---

func optiDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "FreebuffOpti")
}

func readSavedSettings() *optiSettings {
	b, err := os.ReadFile(settingsPath(optiDir()))
	if err != nil {
		return nil
	}
	var doc optiSettings
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	return &doc
}

func saveSettings(doc *optiSettings) error {
	if doc == nil {
		return nil
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return atomicWriteFile(settingsPath(optiDir()), b, 0o644)
}

// ------------------------------------------------------------- update guard ---

const watchRunName = "FreebuffOpti"

// selfImage is this process's own executable path, resolved once at startup -
// see scheduleSelfRemoval for why it cannot be asked for later.
var selfImage string

func watchDir() string     { return optiDir() }
func watchExePath() string { return filepath.Join(optiDir(), "FreebuffOpti.exe") }
func watchPidPath() string { return filepath.Join(optiDir(), "guard.pid") }
func watchLogPath() string { return filepath.Join(optiDir(), "guard.log") }
func guardPath() string    { return filepath.Join(optiDir(), "guard.json") }

func hideConsoleWindow() {
	if runtime.GOOS != "windows" {
		return
	}
	h, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	if h != 0 {
		_, _, _ = syscall.NewLazyDLL("user32.dll").NewProc("ShowWindow").Call(h, 0) // SW_HIDE
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

func processName(pid int) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	fields := strings.Split(string(out), ",")
	if len(fields) < 2 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(fields[0]), `"`)
}

func readWatchPid() int {
	b, err := os.ReadFile(watchPidPath())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

func stopWatcher() {
	pid := readWatchPid()
	if pid != 0 && pid != os.Getpid() && processAlive(pid) && strings.EqualFold(processName(pid), filepath.Base(os.Args[0])) {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
			_ = p.Release()
			time.Sleep(300 * time.Millisecond)
		}
	}
	_ = os.Remove(watchPidPath())
}

func guardLog(msg string) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + msg + "\r\n"
	f, err := os.OpenFile(watchLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 64*1024 {
		_ = f.Truncate(0)
	}
	_, _ = f.WriteString(line)
}

type guardRecord struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Install   string `json:"install"`
}

func readGuard() *guardRecord {
	b, err := os.ReadFile(guardPath())
	if err != nil {
		return nil
	}
	var g guardRecord
	if json.Unmarshal(b, &g) != nil || !g.Installed {
		return nil
	}
	return &g
}

func guardInstalled() bool { return readGuard() != nil }

func setGuard(install string) error {
	if err := os.MkdirAll(optiDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{
		"installed":   true,
		"version":     version,
		"install":     install,
		"installedAt": time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	return atomicWriteFile(guardPath(), b, 0o644)
}

func clearGuard() { _ = os.Remove(guardPath()) }

func runKey() string { return `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` }

func setRunEntry(command string) error {
	cmd := exec.Command("reg", "add", runKey(), "/v", watchRunName, "/t", "REG_SZ", "/d", command, "/f")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("registering the guard to start at logon: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func clearRunEntry() {
	cmd := exec.Command("reg", "delete", runKey(), "/v", watchRunName, "/f")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

func runEntryPresent() bool {
	cmd := exec.Command("reg", "query", runKey(), "/v", watchRunName)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	return err == nil && strings.Contains(string(out), watchRunName)
}

func startWatcher() {
	exe := watchExePath()
	if _, err := os.Stat(exe); err != nil {
		return
	}
	cmd := exec.Command(exe, "--watch", "--quiet")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedFlag}
	if err := cmd.Start(); err != nil {
		guardLog("could not start the guard: " + err.Error())
		return
	}
	_ = cmd.Process.Release()
}

func promoteWatcher() error {
	dir := optiDir()
	if dir == "" {
		return errors.New("LOCALAPPDATA is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	selfAbs, _ := filepath.Abs(self)
	dstAbs, _ := filepath.Abs(watchExePath())
	if strings.EqualFold(selfAbs, dstAbs) {
		return nil
	}
	stopWatcher()
	data, err := os.ReadFile(self)
	if err != nil {
		return fmt.Errorf("reading this executable: %w", err)
	}
	if _, err := os.Stat(watchExePath()); err == nil {
		if err := os.Rename(watchExePath(), watchExePath()+".old"); err != nil {
			return fmt.Errorf("replacing the running guard: %w", err)
		}
	}
	if err := atomicWriteFile(watchExePath(), data, 0o755); err != nil {
		return fmt.Errorf("writing the guard: %w", err)
	}
	_ = os.Remove(watchExePath() + ".old")
	return nil
}

func installWatch(install string) error {
	if err := promoteWatcher(); err != nil {
		return err
	}
	if err := setGuard(install); err != nil {
		return err
	}
	// No --path on purpose: the guard re-detects the install every tick, so it
	// still finds Freebuff if it is ever reinstalled somewhere else.
	if err := setRunEntry(`"` + watchExePath() + `" --watch --quiet`); err != nil {
		return err
	}
	startWatcher()
	return nil
}

func removeWatch() {
	clearRunEntry()
	stopWatcher()
	clearGuard()
	for _, f := range []string{watchPidPath(), watchExePath(), watchLogPath(), watchExePath() + ".old"} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			_ = os.Rename(f, f+".old")
		}
	}
	_ = os.RemoveAll(watchDir())
	if _, err := os.Stat(watchDir()); err == nil {
		// Something in there is still locked - a guard image that is only now
		// exiting, usually, or this one. RemoveAll stops at the first such file,
		// so the rest is left to a detached rmdir once we are gone.
		scheduleDirRemoval()
	}
}

// needsInjection says whether index.html should be written again, and why.
func needsInjection(ui string) (bool, string) {
	idx := indexPath(ui)
	st, err := os.Stat(idx)
	if err != nil {
		return false, ""
	}
	if time.Since(st.ModTime()) < watchSettle {
		return false, ""
	}
	html, err := os.ReadFile(idx)
	if err != nil {
		return false, ""
	}
	_, _, injected, err := markerBounds(string(html))
	if err != nil {
		return false, ""
	}
	if !injected {
		return true, "the injection was removed by a Freebuff update"
	}
	if _, err := os.Stat(filepath.Join(assetsDir(ui), engineName)); err != nil {
		return true, "the injection is there but the panel file was removed"
	}
	return false, ""
}

// guardInstall is the install the guard is responsible for: the one it was
// installed into, or - if that is gone - whatever install it can find now, so
// that a Freebuff update or a move does not leave the limits unenforced.
func guardInstall() string {
	g := readGuard()
	if g == nil {
		return ""
	}
	install := g.Install
	if install == "" || !isInstallDir(install) {
		found, err := findInstallDir("")
		if err != nil {
			return ""
		}
		install = found
	}
	return install
}

// watchTick is one pass of the guard: keep the panel in place, and keep the
// limits on.
func watchTick() {
	install := guardInstall()
	if install == "" {
		return
	}
	ui := filepath.Join(install, "resources", "orchestrator", "ui")
	if need, why := needsInjection(ui); need {
		if err := inject(ui, true); err != nil {
			guardLog("re-inject after update failed: " + err.Error())
		} else {
			guardLog("re-injected: " + why)
		}
	}
	if _, err := applyLimits(install, true); err != nil {
		guardLog("applying limits failed: " + err.Error())
	}
	doc, _ := readSettingsFromCookies(install)
	if doc != nil && doc.Cleanup != nil {
		if doc.Cleanup.KillOrphans {
			killOrphans(true)
		}
		if doc.Cleanup.CacheSweepMB > 0 && cacheSizeMB() > doc.Cleanup.CacheSweepMB {
			if _, err := sweepCache(doc.Cleanup.CacheSweepMB, true); err != nil {
				guardLog("cache sweep failed: " + err.Error())
			}
		}
	}
}

func runWatch() {
	hideConsoleWindow()
	if watchDir() == "" {
		return
	}
	if pid := readWatchPid(); pid != 0 && pid != os.Getpid() && processAlive(pid) &&
		strings.EqualFold(processName(pid), filepath.Base(os.Args[0])) {
		return // a guard is already on duty
	}
	_ = os.MkdirAll(watchDir(), 0o755)
	_ = os.WriteFile(watchPidPath(), []byte(strconv.Itoa(os.Getpid())), 0o644)
	defer func() {
		if readWatchPid() == os.Getpid() {
			_ = os.Remove(watchPidPath())
		}
	}()
	for {
		// The panel's Uninstall tab cannot delete anything - it is a sandboxed
		// renderer - so it leaves a request behind, and this process is what
		// answers it. It is checked before anything else so that a user who has
		// asked to be removed is not first re-capped on the way out.
		if install := guardInstall(); install != "" {
			if _, pending := uninstallRequestPending(install); pending {
				guardLog("removing Freebuff Opti at the panel's request")
				if err := removeEverything(filepath.Join(install, "resources", "orchestrator", "ui"), true); err != nil {
					guardLog("the requested removal failed: " + err.Error())
				}
				return
			}
		}
		watchTick()
		time.Sleep(watchInterval)
		if readWatchPid() != os.Getpid() {
			return
		}
	}
}

// ------------------------------------------------------------------ status ---

func status(ui, install string) {
	m := readManifest(ui)
	html, _ := os.ReadFile(indexPath(ui))
	injected := strings.Contains(string(html), markerStart)
	_, engineErr := os.Stat(filepath.Join(assetsDir(ui), engineName))

	fmt.Printf("  install   %s\n", ui)
	if injected && engineErr == nil {
		ok("Freebuff Opti is installed")
	} else {
		warn("Freebuff Opti is NOT installed")
	}
	if m != nil {
		fmt.Printf("  version   %s\n", m.Version)
		fmt.Printf("  installed %s\n", m.InstalledAt)
	}
	if guardInstalled() {
		if runEntryPresent() {
			ok("Background guard is installed (re-applies the limits and the panel)")
		} else {
			warn("Background guard is installed but not registered to start at logon")
		}
	} else {
		fmt.Printf("  guard     not installed (limits are applied once, not maintained) \n")
	}

	doc := readSavedSettings()
	if doc == nil {
		fmt.Printf("  settings  none saved yet - open the panel in Freebuff once\n")
	} else {
		fmt.Printf("  settings  preset=%s memory=%s cpu=%s\n", orDash(doc.Preset),
			describeMemory(doc.Memory), describeCPU(doc.CPU))
	}
	if st := readApplyState(optiDir()); st != nil {
		fmt.Printf("  last pass %s: %d processes, %d capped, %d prioritised, %d affinity, %d eco\n",
			st.At, st.Processes, st.Capped, st.Prioritised, st.Affinity, st.EcoQoS)
		if st.Note != "" {
			fmt.Printf("            %s\n", st.Note)
		}
	}
	showTree()
	if mb := cacheSizeMB(); mb > 0 {
		fmt.Printf("  cache     %d MB in Freebuff's cache folders\n", mb)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func describeMemory(m *memorySettings) string {
	if m == nil || !m.Enabled {
		return "off"
	}
	return fmt.Sprintf("%d MB per process", m.CapMB)
}

func describeCPU(c *cpuSettings) string {
	if c == nil || !c.Enabled {
		return "off"
	}
	bits := []string{"priority=" + orDash(c.Priority)}
	if c.Cores > 0 {
		bits = append(bits, fmt.Sprintf("cores=%d", c.Cores))
	}
	if c.EcoQoS {
		bits = append(bits, "eco")
	}
	if c.JobCapPercent > 0 {
		bits = append(bits, fmt.Sprintf("throttle=%d%%", c.JobCapPercent))
	}
	return strings.Join(bits, " ")
}

func showTree() {
	tree, err := processTree()
	if err != nil || len(tree) == 0 {
		fmt.Printf("  Freebuff is not running\n")
		return
	}
	type row struct {
		pid  int
		kind string
		mb   int
	}
	var rows []row
	total := 0
	for pid, meta := range tree {
		// meta is {ppid, name, working MB} - see processTree. Every Electron
		// helper shares the Freebuff.exe image name, so the name alone cannot
		// tell the browser process from a renderer; isBrowserProcess can.
		kind := describeKind(pid, meta, tree)
		mb := 0
		if len(meta) >= 3 {
			mb, _ = strconv.Atoi(meta[2])
		}
		total += mb
		rows = append(rows, row{pid: pid, kind: kind, mb: mb})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].mb > rows[j].mb })
	fmt.Printf("  %d Freebuff processes, %d MB resident\n", len(rows), total)
	for _, r := range rows {
		fmt.Printf("    pid %-6d %-14s %d MB\n", r.pid, r.kind, r.mb)
	}
}

// -------------------------------------------------------------------- main ---

func relaunch(install string) {
	exe := filepath.Join(install, "Freebuff.exe")
	if _, err := os.Stat(exe); err != nil {
		warn("Could not find %s to relaunch", exe)
		return
	}
	info("Launching Freebuff\u2026")
	cmd := exec.Command(exe)
	cmd.Dir = install
	if err := cmd.Start(); err != nil {
		warn("Could not launch Freebuff: %v", err)
	}
}

func freebuffRunning() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq Freebuff.exe", "/NH")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "freebuff.exe")
}

func main() {
	initColors()
	if self, err := os.Executable(); err == nil {
		selfImage = self
	}

	var (
		pathFlag        = flag.String("path", "", "Freebuff install directory (auto-detected by default)")
		uninstallFlag   = flag.Bool("uninstall", false, "remove the panel, release the limits and restore index.html")
		statusFlag      = flag.Bool("status", false, "show what is installed, what is applied and what is running")
		clearFlag       = flag.Bool("clear", false, "release every limit this tool has set, and leave the panel in place")
		applyFlag       = flag.Bool("apply", false, "read the saved settings once and apply them, then exit")
		repairFlag      = flag.Bool("repair", false, "rewrite the panel files without touching any setting")
		cleanFlag       = flag.Bool("clean", false, "clear the cache and close stray helper processes, then exit")
		restartFlag     = flag.Bool("restart", false, "close Freebuff if it is running and start it again")
		openFlag        = flag.Bool("open", false, "open the UI folder in Explorer")
		quietFlag       = flag.Bool("quiet", false, "less output")
		watchFlag       = flag.Bool("watch", false, "run quietly in the background, re-applying the limits and the panel")
		removeWatchFlag = flag.Bool("remove-watch", false, "stop the background guard and remove it from logon")
		noGuardFlag     = flag.Bool("no-guard", false, "install the panel only: no background guard, no logon entry, no settings applied")
	)
	flag.Usage = func() {
		banner()
		fmt.Printf("Usage: %s [options]\n\n", filepath.Base(os.Args[0]))
		fmt.Println("  Run with no options to install the optimisation panel into Freebuff Desktop.")
		fmt.Println("  Then open Freebuff and click the gauge icon in its sidebar rail.")
		fmt.Println("  Its Uninstall tab removes Freebuff Opti again, panel and guard and all.")
		fmt.Println()
		flag.PrintDefaults()
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  FreebuffOpti.exe")
		fmt.Println("  FreebuffOpti.exe --status")
		fmt.Println("  FreebuffOpti.exe --clear")
		fmt.Println("  FreebuffOpti.exe --clean")
		fmt.Println("  FreebuffOpti.exe --repair --restart")
		fmt.Println("  FreebuffOpti.exe --remove-watch")
		fmt.Println("  FreebuffOpti.exe --no-guard")
		fmt.Println("  FreebuffOpti.exe --uninstall")
	}
	flag.Parse()

	quiet := *quietFlag

	if *watchFlag {
		runWatch()
		return
	}
	if *removeWatchFlag {
		removeWatch()
		if !quiet {
			banner()
			ok("Background guard stopped and removed (logon entry, process and files)")
		}
		return
	}

	if !quiet {
		banner()
	}

	install, err := findInstallDir(*pathFlag)
	if err != nil {
		// --clear works without an install: it only touches running processes.
		if *clearFlag {
			clearLimits(quiet)
			return
		}
		warn("%v", err)
		os.Exit(1)
	}
	ui := filepath.Join(install, "resources", "orchestrator", "ui")

	switch {
	case *statusFlag:
		status(ui, install)
		return

	case *clearFlag:
		if !quiet {
			step("Releasing every limit, and re-checking")
		}
		clearLimits(quiet)
		doc := readSavedSettings()
		if doc != nil {
			if doc.Memory != nil {
				doc.Memory.Enabled = false
			}
			if doc.CPU != nil {
				doc.CPU.Enabled = false
			}
			_ = saveSettings(doc)
		}
		_ = os.Remove(statePath(optiDir()))
		fmt.Println()
		ok("Freebuff is back to its own defaults. Turn limits on again from the panel.")
		return

	case *cleanFlag:
		if !quiet {
			step("Clearing clutter")
		}
		budget := 250
		if doc := readSavedSettings(); doc != nil && doc.Cleanup != nil && doc.Cleanup.CacheSweepMB > 0 {
			budget = doc.Cleanup.CacheSweepMB
		}
		if before := cacheSizeMB(); before > 0 {
			if _, err := sweepCache(budget, quiet); err != nil {
				warn("%v", err)
			} else if before <= budget {
				info("Freebuff's cache is already inside the %d MB budget", budget)
			}
		}
		killOrphans(quiet)
		return

	case *applyFlag:
		if !quiet {
			step("Applying the saved settings")
		}
		st, err := applyLimits(install, quiet)
		if err != nil {
			warn("%v", err)
			os.Exit(1)
		}
		if !quiet {
			ok("%d processes, %d capped, %d prioritised, %d affinity, %d eco, %d in the CPU job",
				st.Processes, st.Capped, st.Prioritised, st.Affinity, st.EcoQoS, st.JobAssigned)
			if st.Note != "" {
				info("%s", st.Note)
			}
		}
		return

	case *uninstallFlag:
		if !quiet {
			step("Removing Freebuff Opti")
		}
		if err := removeEverything(ui, quiet); err != nil {
			warn("%v", err)
			os.Exit(1)
		}
		fmt.Println()
		ok("Freebuff is back to stock. Restart it if it is running.")
		if *restartFlag {
			relaunch(install)
		}
		return
	}

	if !quiet {
		step("Found Freebuff at %s", install)
	}
	if *repairFlag && !quiet {
		step("Repairing the installed files")
	}
	if !quiet {
		step("Installing the optimisation panel")
	}
	if err := inject(ui, quiet); err != nil {
		warn("%v", err)
		os.Exit(1)
	}
	if !quiet {
		ok("Injected into resources/orchestrator/ui/index.html")
		ok("Wrote assets/%s (%d bytes)", engineName, len(engineJS))
	}

	if *noGuardFlag {
		if !quiet {
			info("Installed without the background guard (--no-guard): nothing is applied and nothing starts at logon.")
		}
	} else {
		if err := installWatch(install); err != nil {
			warn("Could not set up the background guard: %v", err)
			info("       The limits are applied once; run this again to re-apply them.")
		} else if !quiet {
			ok("Guarding in the background (%s)", watchDir())
			info("   Limits are re-applied every %s; --remove-watch takes this away.", watchInterval)
		}

		// Apply whatever is already saved, so a fresh install or a repair takes
		// effect without waiting for the guard's first tick.
		st, err := applyLimits(install, quiet)
		if err != nil && !quiet {
			warn("%v", err)
		}
		if !quiet && st.Processes > 0 {
			ok("%d processes, %d capped, %d prioritised", st.Processes, st.Capped, st.Prioritised)
		}
	}

	if *openFlag {
		_ = exec.Command("explorer.exe", ui).Start()
	}

	if quiet {
		fmt.Printf("installed: %s\n", ui)
	} else {
		fmt.Println()
		fmt.Printf("%sDone.%s\n", colBold+colGreen, colReset)
		fmt.Println()
		fmt.Println("  Open Freebuff - a " + colBold + "gauge icon" + colReset + " appears in the sidebar rail.")
		fmt.Println("  Click it for the RAM cap, the CPU cap, and the one-click potato presets.")
		fmt.Println()
		fmt.Printf("  %sThe limits are enforced by a small background guard, not by the page,%s\n", colDim, colReset)
		fmt.Printf("  %ssince a renderer cannot cap another process's memory or CPU.%s\n", colDim, colReset)
		fmt.Printf("  %sUnofficial extension: not made by, or endorsed by, Freebuff.%s\n", colDim, colReset)
		fmt.Println()
	}

	if freebuffRunning() {
		if !quiet {
			warn("Freebuff is already running.")
			fmt.Println("       Press Ctrl+R in Freebuff to load the panel, or run this again with --restart.")
		}
		if *restartFlag {
			if !quiet {
				step("Relaunching Freebuff")
			}
			_ = exec.Command("taskkill", "/IM", "Freebuff.exe").Run()
			for i := 0; i < 60 && freebuffRunning(); i++ {
				time.Sleep(250 * time.Millisecond)
			}
			relaunch(install)
		}
	} else if *restartFlag {
		if !quiet {
			step("Starting Freebuff")
		}
		relaunch(install)
	}
}
