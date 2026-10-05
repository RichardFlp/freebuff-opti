// Freebuff Opti - reading the page's settings out of the browser.
//
// The panel runs inside Freebuff's renderer, so it can only persist through the
// one store that survives a launch: cookies. (Freebuff serves its UI from a
// fresh loopback port every launch, so localStorage - which is per-origin, and
// therefore per-port - is gone by the next start. That is the same constraint
// the sibling Theme Studio works under.)
//
// Cookies are not a file the daemon can read on its own: Chromium keeps its
// cookie jar in a SQLite database, and Go has no SQLite in its standard
// library. The install ships its own Bun, though, and Bun reads SQLite
// natively, so the daemon shells out to that Bun to read exactly the cookies it
// wrote - `fbop%` and nothing else - and never touches any other application's
// cookies or any of Freebuff's own.
//
// Values are usually plaintext here (measured on Windows 11: every cookie is in
// every Freebuff jar had an empty encrypted_value), but Chromium can also store
// them DPAPI- or AES-GCM-encrypted, so both are handled rather than assumed
// away.
//
// The jar is read-only, always. The panel's Uninstall tab asks to be removed by
// writing one more cookie (`fbop_uninstall`), and the daemon reads that request
// the same way it reads the settings - it never writes to the jar, because
// rewriting a live cookie database behind another process's back is not a risk
// worth taking with someone's Freebuff.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const cookiePrefix = "fbop"

// uninstallCookie carries the panel's request to be removed. It is deliberately
// outside the settings chunk series: the panel clears its settings and writes
// this in the same breath, and the guard has to see it after that has happened.
// The value is the epoch-millisecond timestamp of the click, which is what keeps
// a request from outliving the install it belongs to (see
// uninstallRequestPending) - and it is digits only, because a ':' in a cookie
// value is stored percent-encoded, so an ISO string would not survive the trip.
const uninstallCookie = "fbop_uninstall"

// maxSettingsChunks matches COOKIE_MAX_CHUNKS in assets/opti-engine.js. The two
// files are the two ends of one contract; changing one means changing both.
const maxSettingsChunks = 6

// optiSettings is the document the panel writes. Every field is optional, so a
// half-written or older document still applies what it does have.
type optiSettings struct {
	Version  int               `json:"v"`
	Preset   string            `json:"preset"`
	Memory   *memorySettings   `json:"memory"`
	CPU      *cpuSettings      `json:"cpu"`
	Windows  *windowSettings   `json:"windows"`
	Cleanup  *cleanupSettings  `json:"cleanup"`
	Watchdog *watchdogSettings `json:"watchdog"`
	Updated  string            `json:"updated"`
}

type memorySettings struct {
	Enabled bool `json:"enabled"`
	// CapMB is the working set each Freebuff process is held to. It is a hard
	// cap: SetProcessWorkingSetSizeEx with QUOTA_LIMITS_HARDWS_MAX_ENABLE, which
	// was measured to pin a 555 MB process at 219 MB against a 220 MB limit.
	CapMB int `json:"capMb"`
	// TrimOnApply evicts the pages above the cap immediately instead of waiting
	// for the next allocation to trip the cap.
	TrimOnApply bool `json:"trimOnApply"`
	// IdleTrimMB trims any process that drifts above this while the window is
	// hidden. 0 disables it.
	IdleTrimMB int `json:"idleTrimMb"`
}

type cpuSettings struct {
	Enabled bool `json:"enabled"`
	// Priority is "idle", "below-normal", "normal" or "high".
	Priority string `json:"priority"`
	// Cores pins the tree to this many processors. 0 means "leave it alone".
	Cores int `json:"cores"`
	// EcoQoS asks the scheduler for efficiency mode, which prefers E-cores on a
	// hybrid CPU. A hint, never the only thing holding a limit up.
	EcoQoS bool `json:"ecoQos"`
	// JobCapPercent is the hard CPU throttle for the browser process, as a
	// percentage of the whole machine. 0 leaves it off. It reaches only the
	// browser process; the renderers refuse to join a new job.
	JobCapPercent int `json:"jobCapPercent"`
}

type windowSettings struct {
	// Compact hides Freebuff's decorative chrome. Cosmetic, and reversible.
	Compact bool `json:"compact"`
	// ReduceMotion stops the app's animations.
	ReduceMotion bool `json:"reduceMotion"`
	// ReduceEffects drops the blur and shadow work, which is what a weak GPU
	// struggles with.
	ReduceEffects bool `json:"reduceEffects"`
	// ThinScrollbars narrows the scrollbars.
	ThinScrollbars bool `json:"thinScrollbars"`
}

type cleanupSettings struct {
	// CacheSweepMB removes the oldest files in Freebuff's HTTP cache while
	// leaving its cookies, storage and settings alone.
	CacheSweepMB int `json:"cacheSweepMb"`
	// KillOrphans closes stray Chromium helper processes that have lost their
	// browser process.
	KillOrphans bool `json:"killOrphans"`
	// ReapIdleThreads is a note for the panel, which is the only side that can
	// see which threads are idle.
	ReapIdleThreads bool `json:"reapIdleThreads"`
}

type watchdogSettings struct {
	// Enabled applies the limits to any Freebuff process that appears later.
	// Electron spawns renderers after the panel has loaded, so without this a
	// cap would only ever cover the processes that existed at apply time.
	Enabled bool `json:"enabled"`
	// IntervalSeconds is how often the daemon re-checks the tree.
	IntervalSeconds int `json:"intervalSeconds"`
}

// settingsPath is where the daemon keeps the last settings it applied, so that
// a launch which finds the cookies unreadable still has something to restore.
func settingsPath(dir string) string { return filepath.Join(dir, "settings.json") }

// readSettingsFromCookies pulls the panel's document out of every cookie jar in
// the Freebuff profile. A cookie jar the daemon cannot read is skipped rather
// than fatal: another profile may hold the same document.
func readSettingsFromCookies(install string) (*optiSettings, error) {
	bun := bunBinary(install)
	if bun == "" {
		return nil, fmt.Errorf("Freebuff's Bun runtime was not found")
	}
	var lastErr error
	for _, db := range profileCookieDBs() {
		doc, err := readSettingsFromJar(bun, db)
		if err != nil {
			lastErr = err
			continue
		}
		if doc != nil {
			return doc, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

// queryCookies runs one read-only query against a cookie jar and returns
// name -> value, decrypting anything Chromium stored encrypted. The predicate is
// assembled in Go from literals in this file, so no cookie name ever reaches the
// SQL as data and the read can never widen beyond the names the daemon wrote.
func queryCookies(bun, db, predicate string) (map[string]string, error) {
	script := `(async () => {
  const { Database } = await import('bun:sqlite')
  const db = new Database(process.env.FBOP_DB, { readonly: true })
  const rows = db.query("select name, value, hex(encrypted_value) as enc from cookies where " + process.env.FBOP_WHERE).all()
  console.log(JSON.stringify(rows))
})()`
	cmd := exec.Command(bun, "-e", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Env = append(os.Environ(), "FBOP_DB="+db, "FBOP_WHERE="+predicate)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[i+1:])
	}
	var rows []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Enc   string `json:"enc"`
	}
	if err := json.Unmarshal([]byte(line), &rows); err != nil {
		return nil, fmt.Errorf("could not read the settings cookies: %w", err)
	}
	parts := map[string]string{}
	for _, r := range rows {
		v := r.Value
		if v == "" && r.Enc != "" {
			if raw, hexErr := hex.DecodeString(r.Enc); hexErr == nil {
				if plain, decErr := decryptCookieValue(raw); decErr == nil {
					v = string(plain)
				}
			}
		}
		parts[r.Name] = v
	}
	return parts, nil
}

// readSettingsFromJar reads one cookie jar. The query is restricted to the
// `fbop%` prefix in SQL, so no other cookie is ever even loaded.
func readSettingsFromJar(bun, db string) (*optiSettings, error) {
	parts, err := queryCookies(bun, db, "name like '"+cookiePrefix+"%'")
	if err != nil {
		return nil, err
	}
	count := strings.TrimSpace(parts[cookiePrefix+"_n"])
	if count == "" {
		return nil, nil
	}
	gen, chunks := parseSeriesCount(count)
	if chunks <= 0 {
		return nil, nil
	}
	var payload strings.Builder
	for i := 0; i < chunks; i++ {
		name := fmt.Sprintf("%s_%d", cookiePrefix, i)
		if gen > 0 {
			name = fmt.Sprintf("%s_%d_%d", cookiePrefix, gen, i)
		}
		chunk, ok := parts[name]
		if !ok {
			return nil, fmt.Errorf("settings cookie %d of %d is missing", i, chunks)
		}
		payload.WriteString(chunk)
	}
	doc, err := decodeSettings(payload.String())
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// readUninstallRequest returns the panel's removal request, or "" when none is
// set. A jar that cannot be read is skipped rather than fatal, like the settings
// read, and a jar whose bytes do not mention the cookie is not opened at all -
// so the common case costs one file scan instead of a Bun launch.
func readUninstallRequest(install string) (string, error) {
	bun := bunBinary(install)
	if bun == "" {
		return "", fmt.Errorf("Freebuff's Bun runtime was not found")
	}
	var lastErr error
	found := false
	for _, db := range profileCookieDBs() {
		b, err := os.ReadFile(db)
		if err != nil || !bytes.Contains(b, []byte(uninstallCookie)) {
			continue
		}
		found = true
		parts, err := queryCookies(bun, db, "name = '"+uninstallCookie+"'")
		if err != nil {
			lastErr = err
			continue
		}
		if v := strings.TrimSpace(parts[uninstallCookie]); v != "" {
			return v, nil
		}
	}
	if lastErr != nil && found {
		return "", lastErr
	}
	return "", nil
}

// parseSeriesCount reads the "<generation>:<chunkCount>" value the panel
// publishes in fbop_n. A bare number is a series written by a build that had no
// generations, which is generation 0. Anything out of range reads as "nothing"
// rather than as an error: a corrupt count must not stop the guard.
func parseSeriesCount(raw string) (gen, count int) {
	if head, tail, ok := strings.Cut(strings.TrimSpace(raw), ":"); ok {
		gen, _ = strconv.Atoi(strings.TrimSpace(head))
		count, _ = strconv.Atoi(strings.TrimSpace(tail))
	} else {
		count, _ = strconv.Atoi(strings.TrimSpace(raw))
	}
	if gen < 0 {
		gen = 0
	}
	if count < 0 || count > maxSettingsChunks {
		count = 0
	}
	return gen, count
}

// decodeSettings accepts either the raw JSON document or its base64url form, so
// the panel is free to use whichever fits the cookie budget better.
func decodeSettings(payload string) (*optiSettings, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, nil
	}
	candidates := []string{payload}
	if b, err := base64URLDecode(payload); err == nil && len(b) > 0 {
		candidates = append(candidates, string(b))
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if !strings.HasPrefix(c, "{") {
			continue
		}
		var doc optiSettings
		if err := json.Unmarshal([]byte(c), &doc); err == nil {
			return &doc, nil
		}
	}
	return nil, fmt.Errorf("the settings cookie does not contain a JSON object")
}

func base64URLDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}

// ------------------------------------------------------- cookie decryption ---

var (
	crypt32           = syscall.NewLazyDLL("crypt32.dll")
	procUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree     = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func dpapiDecrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty blob")
	}
	in := dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
	var out dataBlob
	r, _, callErr := procUnprotectData.Call(uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, fmt.Errorf("%v", callErr)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	dec := make([]byte, out.cbData)
	copy(dec, unsafe.Slice(out.pbData, out.cbData))
	return dec, nil
}

// profileEncryptionKey unwraps the AES key Chromium keeps in `Local State`.
func profileEncryptionKey(profile string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(profile, "Local State"))
	if err != nil {
		return nil, err
	}
	var ls struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(ls.OsCrypt.EncryptedKey)
	if err != nil {
		return nil, err
	}
	if len(key) > 5 && strings.HasPrefix(string(key), "DPAPI") {
		key = key[5:]
	}
	return dpapiDecrypt(key)
}

// decryptCookieValue handles the three shapes a Chromium cookie can take.
func decryptCookieValue(blob []byte) ([]byte, error) {
	if len(blob) < 3 {
		return nil, fmt.Errorf("cookie value is too short to be encrypted")
	}
	switch prefix := string(blob[:3]); prefix {
	case "v10", "v11":
		key, err := profileEncryptionKey(freebuffProfileDir())
		if err != nil {
			return nil, err
		}
		body := blob[3:]
		if len(body) < 12+16 {
			return nil, fmt.Errorf("encrypted cookie value is truncated")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		return gcm.Open(nil, body[:12], body[12:], nil)
	case "v20":
		// App-bound encryption. Reading it needs the app's own elevation, which
		// this daemon deliberately never asks for.
		return nil, fmt.Errorf("cookie is app-bound encrypted (v20)")
	default:
		return dpapiDecrypt(blob)
	}
}
