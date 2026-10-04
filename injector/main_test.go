package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestInstall builds a throwaway install tree that looks enough like
// Freebuff for inject() to accept it.
func newTestInstall(t *testing.T, indexHTML string) (install, ui string) {
	t.Helper()
	install = t.TempDir()
	ui = filepath.Join(install, "resources", "orchestrator", "ui")
	if err := os.MkdirAll(filepath.Join(ui, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(indexPath(ui), []byte(indexHTML), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return install, ui
}

const sampleIndex = `<!doctype html>
<html>
  <head><title>Freebuff</title></head>
  <body>
    <div id="root"></div>
    <div id="startup-recovery"></div>
  </body>
</html>
`

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestInjectIsIdempotent(t *testing.T) {
	_, ui := newTestInstall(t, sampleIndex)

	if err := inject(ui, true); err != nil {
		t.Fatalf("first inject: %v", err)
	}
	first := readFile(t, indexPath(ui))

	if err := inject(ui, true); err != nil {
		t.Fatalf("second inject: %v", err)
	}
	second := readFile(t, indexPath(ui))

	if first != second {
		t.Fatalf("inject is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if strings.Count(second, markerStart) != 1 || strings.Count(second, markerEnd) != 1 {
		t.Fatalf("expected exactly one marker pair, got %d/%d", strings.Count(second, markerStart), strings.Count(second, markerEnd))
	}
	if !strings.Contains(second, engineName) {
		t.Fatalf("engine script tag missing")
	}
	if _, err := os.Stat(backupPath(ui)); err != nil {
		t.Fatalf("backup not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(assetsDir(ui), engineName)); err != nil {
		t.Fatalf("engine file not written: %v", err)
	}
}

func TestMarkerBoundsRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"duplicate start": markerStart + "\n" + markerStart + "\n" + markerEnd,
		"missing end":     "x" + markerStart + "y",
		"out of order":    markerEnd + "x" + markerStart,
	}
	for name, html := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := markerBounds(html); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}

	if _, _, found, err := markerBounds("plain html"); err != nil || found {
		t.Fatalf("plain html should report no markers, got found=%v err=%v", found, err)
	}
}

func TestUninstallRestoresPristineBackup(t *testing.T) {
	_, ui := newTestInstall(t, sampleIndex)
	if err := inject(ui, true); err != nil {
		t.Fatalf("inject: %v", err)
	}
	// Stand in for the guard's status file, which uninstall must also clear.
	if err := os.WriteFile(filepath.Join(assetsDir(ui), statusName), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write status: %v", err)
	}
	if err := uninstall(ui, true); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := readFile(t, indexPath(ui)); got != sampleIndex {
		t.Fatalf("uninstall did not restore the original index:\n%s", got)
	}
	if _, err := os.Stat(backupPath(ui)); !os.IsNotExist(err) {
		t.Fatalf("backup should be consumed on a clean uninstall")
	}
	if _, err := os.Stat(filepath.Join(assetsDir(ui), engineName)); !os.IsNotExist(err) {
		t.Fatalf("engine file should be removed")
	}
	if _, err := os.Stat(filepath.Join(assetsDir(ui), statusName)); !os.IsNotExist(err) {
		t.Fatalf("status file should be removed")
	}
}

// A Freebuff update rewrites index.html after we injected. Uninstall must
// remove our block but must not put the old backup back over the new app.
func TestUninstallKeepsNewerIndex(t *testing.T) {
	_, ui := newTestInstall(t, sampleIndex)
	if err := inject(ui, true); err != nil {
		t.Fatalf("inject: %v", err)
	}

	updated := readFile(t, indexPath(ui)) + "\n<!-- freebuff 1.4.0 updated this file -->\n"
	if err := os.WriteFile(indexPath(ui), []byte(updated), 0o644); err != nil {
		t.Fatalf("simulate app update: %v", err)
	}

	if err := uninstall(ui, true); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	got := readFile(t, indexPath(ui))
	if strings.Contains(got, markerStart) {
		t.Fatalf("injection marker survived uninstall:\n%s", got)
	}
	if !strings.Contains(got, "freebuff 1.4.0 updated this file") {
		t.Fatalf("uninstall overwrote the newer index:\n%s", got)
	}
	if _, err := os.Stat(backupPath(ui)); err != nil {
		t.Fatalf("stale backup should be kept for the user, not silently discarded: %v", err)
	}
}

func TestRefusesNonFreebuffPage(t *testing.T) {
	_, ui := newTestInstall(t, "<html><body>not the app</body></html>")
	if err := inject(ui, true); err == nil {
		t.Fatalf("inject should refuse a page that is not the Freebuff UI")
	}
	if _, err := os.Stat(backupPath(ui)); !os.IsNotExist(err) {
		t.Fatalf("no backup should be written when the page is refused")
	}
}

func TestAtomicWriteReplacesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := atomicWriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if err := atomicWriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("write 2 (replace): %v", err)
	}
	if got := readFile(t, path); got != "second" {
		t.Fatalf("got %q, want %q", got, "second")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fbop-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestManifestChecksumMatchesBackup(t *testing.T) {
	_, ui := newTestInstall(t, sampleIndex)
	if err := inject(ui, true); err != nil {
		t.Fatalf("inject: %v", err)
	}
	m := readManifest(ui)
	if m == nil {
		t.Fatalf("manifest missing")
	}
	backup := readFile(t, backupPath(ui))
	if m.OriginalSHA != sha256Hex([]byte(backup)) {
		t.Fatalf("manifest checksum does not describe the backup")
	}
	if !m.HadBackup {
		t.Fatalf("manifest should record the backup")
	}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), "originalIndexSha256") {
		t.Fatalf("manifest JSON lost its checksum field")
	}
}

// The guard re-injects after an update, so needsInjection has to notice both
// ways the injection disappears without firing while an update is still being
// written.
func TestNeedsInjection(t *testing.T) {
	_, ui := newTestInstall(t, sampleIndex)
	// A file the guard has only just seen is left alone until it settles, so age
	// it the way a real install would be aged by the time the guard runs.
	old := time.Now().Add(-2 * watchSettle)
	if err := os.Chtimes(indexPath(ui), old, old); err != nil {
		t.Fatalf("age: %v", err)
	}
	if need, _ := needsInjection(ui); !need {
		t.Fatalf("a stock index.html needs the injection")
	}
	if err := inject(ui, true); err != nil {
		t.Fatalf("inject: %v", err)
	}
	// inject just wrote the file; the guard deliberately waits for an update to
	// settle before touching it.
	if need, _ := needsInjection(ui); need {
		t.Fatalf("a just-injected index.html should be left alone")
	}

	current := readFile(t, indexPath(ui))
	stripped, _, err := stripInjected(current)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if err := os.WriteFile(indexPath(ui), []byte(stripped), 0o644); err != nil {
		t.Fatalf("write stripped: %v", err)
	}
	// A Freebuff update is still writing when the guard first sees the file, so
	// needsInjection ignores anything touched within watchSettle. Age the file
	// past that before asking.
	if err := os.Chtimes(indexPath(ui), old, old); err != nil {
		t.Fatalf("age: %v", err)
	}
	if need, why := needsInjection(ui); !need || !strings.Contains(why, "removed") {
		t.Fatalf("a stripped index.html should need re-injecting, got need=%v why=%q", need, why)
	}

	// The other half: the marker is there but the panel file was deleted.
	if err := inject(ui, true); err != nil {
		t.Fatalf("re-inject: %v", err)
	}
	if err := os.Remove(filepath.Join(assetsDir(ui), engineName)); err != nil {
		t.Fatalf("remove engine: %v", err)
	}
	if err := os.Chtimes(indexPath(ui), old, old); err != nil {
		t.Fatalf("age: %v", err)
	}
	if need, why := needsInjection(ui); !need || !strings.Contains(why, "panel file") {
		t.Fatalf("a missing panel file should need re-injecting, got need=%v why=%q", need, why)
	}
}

// ------------------------------------------------------------- limits maths ---

func TestCPURateFor(t *testing.T) {
	cases := []struct {
		percent, cores int
		want           uint32
	}{
		{40, 4, 1000}, // 10% per core -> 10*100
		{50, 4, 1200}, // 12% per core (integer division)
		{100, 4, 0},   // 100% is not a cap
		{0, 4, 0},     // off
		{-5, 4, 0},    // nonsense
		{40, 0, 0},    // no core count to divide by
		{5, 20, 100},  // below one core rounds UP to one core, never to a hang
		{80, 4, 2000}, // 20% per core on 4 cores
	}
	for _, c := range cases {
		if got := cpuRateFor(c.percent, c.cores); got != c.want {
			t.Errorf("cpuRateFor(%d, %d) = %d, want %d", c.percent, c.cores, got, c.want)
		}
	}
}

func TestAffinityMaskFor(t *testing.T) {
	const four = uintptr(0b1111)

	if got := affinityMaskFor(2, four); got != 0b0011 {
		t.Errorf("affinityMaskFor(2, 1111) = %04b, want 0011", got)
	}
	if got := affinityMaskFor(4, four); got != four {
		t.Errorf("asking for every core should return the system mask, got %04b", got)
	}
	if got := affinityMaskFor(1, 0); got != 1 {
		t.Errorf("with no known system mask, affinityMaskFor(1, 0) = %04b, want 1", got)
	}
	// Every bit of the result must be a bit the process was already allowed.
	sparse := uintptr(0b0010101) // cores 0, 2, 4
	got := affinityMaskFor(2, sparse)
	if got&^sparse != 0 {
		t.Errorf("affinityMaskFor returned bits outside the system mask: %07b vs %07b", got, sparse)
	}
	if n := countBits(got); n != 2 {
		t.Errorf("affinityMaskFor(2, ...) selected %d cores, want 2", n)
	}
}

// --------------------------------------------------- the settings contract ---

func TestParseSeriesCount(t *testing.T) {
	cases := []struct {
		raw       string
		gen, want int
	}{
		{"3:2", 3, 2},
		{"1:1", 1, 1},
		{"2", 0, 2},   // a build with no generations
		{"0:0", 0, 0}, // a deliberate wipe
		{"99:99", 99, 0},
		{"nonsense", 0, 0},
		{"", 0, 0},
		{"-1:-1", 0, 0},
		{" 4 : 3 ", 4, 3},
	}
	for _, c := range cases {
		gen, count := parseSeriesCount(c.raw)
		if gen != c.gen || count != c.want {
			t.Errorf("parseSeriesCount(%q) = %d,%d, want %d,%d", c.raw, gen, count, c.gen, c.want)
		}
	}
}

func TestDecodeSettingsAcceptsBothEncodings(t *testing.T) {
	raw := `{"v":1,"preset":"potato","memory":{"enabled":true,"capMb":700}}`

	doc, err := decodeSettings(raw)
	if err != nil || doc == nil {
		t.Fatalf("raw JSON: err=%v doc=%v", err, doc)
	}
	if doc.Preset != "potato" || doc.Memory == nil || doc.Memory.CapMB != 700 {
		t.Fatalf("raw JSON decoded wrong: %+v", doc)
	}

	b64 := base64.RawURLEncoding.EncodeToString([]byte(raw))
	doc, err = decodeSettings(b64)
	if err != nil || doc == nil {
		t.Fatalf("base64url JSON: err=%v doc=%v", err, doc)
	}
	if doc.Preset != "potato" {
		t.Fatalf("base64url decoded wrong: %+v", doc)
	}

	if doc, err := decodeSettings(""); err != nil || doc != nil {
		t.Fatalf("an empty payload should be nil, not an error: err=%v doc=%v", err, doc)
	}
	if _, err := decodeSettings("!!!not json!!!"); err == nil {
		t.Fatalf("garbage should be an error rather than a silent default")
	}
	// A document with only some of the fields still applies what it has: the
	// guard must not require the whole schema to be present.
	partial, err := decodeSettings(`{"cpu":{"enabled":true,"priority":"idle"}}`)
	if err != nil || partial == nil || partial.CPU == nil || partial.CPU.Priority != "idle" {
		t.Fatalf("partial document rejected: err=%v doc=%+v", err, partial)
	}
}

// The panel writes the status file the page fetches. It is the only channel the
// page has for seeing what was actually applied, so it must be valid JSON with
// the version stamped on it, and wiped on uninstall.
func TestWriteStatusFile(t *testing.T) {
	install, ui := newTestInstall(t, sampleIndex)
	st := applyState{At: "2026-01-01T00:00:00Z", Processes: 9, Capped: 7, Note: "browser process only"}
	writeStatusFile(install, st)

	body := readFile(t, filepath.Join(assetsDir(ui), statusName))
	var got applyState
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("status file is not valid JSON: %v\n%s", err, body)
	}
	if got.Processes != 9 || got.Capped != 7 {
		t.Fatalf("status file lost the counters: %+v", got)
	}
	if got.Version != version {
		t.Fatalf("status file should carry the guard's version, got %q", got.Version)
	}
}

// The panel writes the settings; the daemon reads them back out of Chromium's
// SQLite cookie jar with the install's own Bun. That seam is the whole reason
// the two halves work at all, and it is invisible to both halves' unit tests - a
// renamed field or a changed chunk name would only show up as "no settings saved
// yet" in the app. So this runs the real thing: a real jar, built by the real
// Bun, read by the real reader.
func TestReadSettingsFromJarAgainstRealBun(t *testing.T) {
	bun := testBun(t)
	if bun == "" {
		t.Skip("no Bun runtime available; set FBOP_TEST_BUN to run this test")
	}

	// Exactly what the panel produced in a browser, so the field names are the
	// panel's and not a second guess at them.
	payload := `{"v":1,"preset":"battery","memory":{"enabled":true,"capMb":1200,"trimOnApply":false,"idleTrimMb":900},` +
		`"cpu":{"enabled":true,"priority":"below-normal","cores":0,"ecoQos":true,"jobCapPercent":0},` +
		`"windows":{"compact":false,"reduceMotion":true,"reduceEffects":false,"thinScrollbars":false},` +
		`"cleanup":{"cacheSweepMb":300,"killOrphans":true,"reapIdleThreads":false},` +
		`"watchdog":{"enabled":true,"intervalSeconds":20},"updated":"2026-10-04T07:44:13.819Z"}`
	b64 := base64.RawURLEncoding.EncodeToString([]byte(payload))

	db := filepath.Join(t.TempDir(), "Cookies")
	makeJar(t, bun, db, map[string]string{
		"fbop_n":      "7:1",
		"fbop_7_0":    b64,
		"fbop_7_1":    "", // a stale higher index, which the reader must ignore
		"SID":         "not-ours",
		"freebuff_id": "also-not-ours",
	})

	doc, err := readSettingsFromJar(bun, db)
	if err != nil {
		t.Fatalf("readSettingsFromJar: %v", err)
	}
	if doc == nil {
		t.Fatalf("the document was not read back")
	}
	if doc.Preset != "battery" || doc.Memory == nil || doc.Memory.CapMB != 1200 {
		t.Fatalf("memory settings came back wrong: %+v", doc.Memory)
	}
	if doc.CPU == nil || doc.CPU.Priority != "below-normal" || !doc.CPU.EcoQoS || doc.CPU.JobCapPercent != 0 {
		t.Fatalf("cpu settings came back wrong: %+v", doc.CPU)
	}
	if doc.Windows == nil || !doc.Windows.ReduceMotion || doc.Windows.Compact {
		t.Fatalf("window settings came back wrong: %+v", doc.Windows)
	}
	if doc.Cleanup == nil || doc.Cleanup.CacheSweepMB != 300 || !doc.Cleanup.KillOrphans {
		t.Fatalf("cleanup settings came back wrong: %+v", doc.Cleanup)
	}
	if doc.Watchdog == nil || doc.Watchdog.IntervalSeconds != 20 {
		t.Fatalf("watchdog settings came back wrong: %+v", doc.Watchdog)
	}

	// A legacy, ungated series: chunks are fbop_<i> and the count is a bare
	// number. It must still read, or an upgrade silently loses the settings.
	legacy := filepath.Join(t.TempDir(), "Cookies")
	makeJar(t, bun, legacy, map[string]string{"fbop_n": "1", "fbop_0": b64})
	doc, err = readSettingsFromJar(bun, legacy)
	if err != nil || doc == nil || doc.Preset != "battery" {
		t.Fatalf("legacy series: err=%v doc=%+v", err, doc)
	}

	// A jar with nothing of ours is not an error and not a document.
	empty := filepath.Join(t.TempDir(), "Cookies")
	makeJar(t, bun, empty, map[string]string{"SID": "not-ours"})
	if doc, err = readSettingsFromJar(bun, empty); err != nil || doc != nil {
		t.Fatalf("foreign jar: err=%v doc=%+v", err, doc)
	}
}

// testBun finds a Bun to build the fixture jar with: the one the Freebuff
// install ships, an explicit override, or anything on PATH.
func testBun(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("FBOP_TEST_BUN"); p != "" {
		return p
	}
	if install, err := findInstallDir(""); err == nil {
		if b := bunBinary(install); b != "" {
			return b
		}
	}
	if b, err := exec.LookPath("bun"); err == nil {
		return b
	}
	return ""
}

// makeJar writes a Chromium-shaped cookie jar with the given names and values.
func makeJar(t *testing.T, bun, path string, cookies map[string]string) {
	t.Helper()
	script := `(async () => {
  const { Database } = await import('bun:sqlite')
  const db = new Database(process.env.FBOP_TEST_DB)
  db.run("create table cookies (name text, value text, encrypted_value blob, host_key text, path text, expires_utc integer, is_secure integer, is_httponly integer)")
  const rows = JSON.parse(process.env.FBOP_TEST_ROWS)
  const ins = db.query("insert into cookies (name, value, encrypted_value, host_key, path) values (?, ?, ?, ?, ?)")
  for (const [name, value] of rows) ins.run(name, value, new Uint8Array(0), "127.0.0.1", "/")
  db.close()
})()`
	rows, _ := json.Marshal(flatten(cookies))
	cmd := exec.Command(bun, "-e", script)
	cmd.Env = append(os.Environ(), "FBOP_TEST_DB="+path, "FBOP_TEST_ROWS="+string(rows))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building the fixture jar: %v: %s", err, out)
	}
}

func flatten(cookies map[string]string) [][2]string {
	out := make([][2]string, 0, len(cookies))
	for name, value := range cookies {
		out = append(out, [2]string{name, value})
	}
	return out
}

// The status table must not mistake the parent pid for the process name: meta
// is {ppid, name, MB}, and the browser process is the only one with no Freebuff
// parent in the tree.
func TestDescribeKind(t *testing.T) {
	tree := map[int][]string{
		100: {"50", "Freebuff.exe", "120"}, // the browser process
		200: {"100", "Freebuff.exe", "90"}, // a renderer
		300: {"100", "bun.exe", "40"},      // the orchestrator
		400: {"200"},                       // truncated metadata
	}
	for pid, want := range map[int]string{100: "browser", 200: "child", 300: "bun", 400: "?"} {
		if got := describeKind(pid, tree[pid], tree); got != want {
			t.Errorf("describeKind(%d) = %q, want %q", pid, got, want)
		}
	}
}

func TestPriorityClassFor(t *testing.T) {
	for name, want := range map[string]uintptr{
		"idle":         priorityIdle,
		"below-normal": priorityBelowNormal,
		"Below-Normal": priorityBelowNormal,
		"normal":       priorityNormal,
	} {
		got, ok := priorityClassFor(name)
		if !ok || got != want {
			t.Errorf("priorityClassFor(%q) = %v,%v want %v", name, got, ok, want)
		}
	}
	if _, ok := priorityClassFor("realtime"); ok {
		t.Errorf("realtime is not a class this tool offers")
	}
}
