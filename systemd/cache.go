package systemd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// cacheVersion bumps whenever ServiceInfo's wire shape changes — readers
// silently discard caches written by a different version rather than risk
// surfacing stale fields decoded into the wrong slots.
//
// v2 also invalidates older snapshots that may have been written from a
// filtered UI state in pre-fix builds.
const cacheVersion = 2

// cacheMaxAge is how long we'll still paint a cached list before treating it
// as too stale to show. Practically infinite (services don't churn between
// reboots), but bounded so a cache from an OS upgrade isn't shown forever.
const cacheMaxAge = 7 * 24 * time.Hour

type cacheFile struct {
	Version  int           `json:"version"`
	Mode     string        `json:"mode"`
	SavedAt  time.Time     `json:"saved_at"`
	Services []ServiceInfo `json:"services"`
}

// CacheDir returns the directory we read/write jeeves caches to. Lives under
// $XDG_CACHE_HOME/jeeves (or ~/.cache/jeeves on most setups, /root/.cache
// /jeeves under sudo). Returns "" if no usable cache dir is available — the
// CLI prints this verbatim so the user knows where the file lives.
func CacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "jeeves")
}

// CachePath returns the path of the service-list cache file for `mode`.
// System and user buses surface different units, so they get separate
// caches. Returns "" if no usable cache dir is available; callers treat that
// as "skip caching" rather than an error.
func CachePath(mode Mode) string {
	dir := CacheDir()
	if dir == "" {
		return ""
	}
	suffix := "system"
	if mode == UserMode {
		suffix = "user"
	}
	return filepath.Join(dir, "services-"+suffix+".json")
}

// CacheEntry is one file we know about — used by the CLI's --cache-info to
// print a human-readable inventory of what's on disk.
type CacheEntry struct {
	Mode    Mode
	Path    string
	Exists  bool
	Size    int64
	SavedAt time.Time
	Valid   bool // false if file exists but is corrupt / wrong version / expired
}

// CacheInventory returns the on-disk state of every cache file jeeves may
// have written. Always returns one entry per mode, even when the file is
// absent — callers print the path so the user can see where it would live.
func CacheInventory() []CacheEntry {
	entries := make([]CacheEntry, 0, 2)
	for _, mode := range []Mode{SystemMode, UserMode} {
		e := CacheEntry{Mode: mode, Path: CachePath(mode)}
		if e.Path == "" {
			entries = append(entries, e)
			continue
		}
		st, err := os.Stat(e.Path)
		if err == nil {
			e.Exists = true
			e.Size = st.Size()
			// Parse the saved-at + version so the CLI can flag stale or
			// corrupt files distinctly from "no cache yet".
			if data, err := os.ReadFile(e.Path); err == nil {
				var cf cacheFile
				if json.Unmarshal(data, &cf) == nil && cf.Version == cacheVersion {
					e.SavedAt = cf.SavedAt
					e.Valid = time.Since(cf.SavedAt) <= cacheMaxAge
				}
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// ClearCache removes every cache file jeeves has written. Reports the paths
// it removed so the CLI can echo them back to the user (transparency: we
// don't silently rm things). A missing file is not an error.
func ClearCache() ([]string, error) {
	removed := []string{}
	for _, mode := range []Mode{SystemMode, UserMode} {
		path := CachePath(mode)
		if path == "" {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed = append(removed, path)
		} else if !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove %s: %w", path, err)
		}
		// Also clean up any stray temp file from an aborted atomic write.
		_ = os.Remove(path + ".tmp")
	}
	// Try to remove the now-empty directory — silently ignore if it has
	// anything else in it (future caches, user files, etc.).
	if dir := CacheDir(); dir != "" {
		_ = os.Remove(dir)
	}
	return removed, nil
}

// LoadServiceCache returns the previously-cached service list for `mode`. The
// boolean reports whether a usable cache was loaded — false means "no cache,
// expired, version mismatch, or corrupt"; callers should just skip the
// optimistic paint and wait for the live fetch.
func LoadServiceCache(mode Mode) ([]ServiceInfo, bool) {
	path := CachePath(mode)
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, false
	}
	if cf.Version != cacheVersion {
		return nil, false
	}
	if time.Since(cf.SavedAt) > cacheMaxAge {
		return nil, false
	}
	if len(cf.Services) == 0 {
		return nil, false
	}
	// A usable startup cache must come from the merged loaded+unit-files list.
	// Loaded-only snapshots (from pre-fix builds) typically have no
	// UnitFileState at all and collapse the UI to just currently-loaded units.
	if !hasUnitFileStates(cf.Services) {
		return nil, false
	}
	return cf.Services, true
}

// SaveServiceCache persists the merged service list so the next run can paint
// it instantly. Best-effort: errors are returned but callers typically just
// log-and-continue — a broken cache write isn't worth surfacing to the user.
func SaveServiceCache(mode Mode, services []ServiceInfo) error {
	path := CachePath(mode)
	if path == "" {
		return errors.New("no cache dir available")
	}
	if len(services) == 0 {
		return errors.New("refusing to cache empty service list")
	}
	if !hasUnitFileStates(services) {
		return errors.New("refusing to cache loaded-only snapshot")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir cache dir: %w", err)
	}
	cf := cacheFile{
		Version:  cacheVersion,
		Mode:     mode.String(),
		SavedAt:  time.Now(),
		Services: services,
	}
	data, err := json.Marshal(cf)
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	// Atomic-ish write: temp + rename so a crash mid-write doesn't leave a
	// half-written JSON that the next start has to discard.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename cache: %w", err)
	}
	return nil
}

func hasUnitFileStates(services []ServiceInfo) bool {
	for _, s := range services {
		if s.UnitFileState != "" {
			return true
		}
	}
	return false
}
