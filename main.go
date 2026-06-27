package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/aymanhs/jeeves/systemd"
	"github.com/aymanhs/jeeves/tui"
	tea "github.com/charmbracelet/bubbletea"
)

// version is the release tag baked in at link time by the release workflow
// (`-ldflags="-X main.version=v0.1.0"`). Local builds keep "dev" so it's
// obvious when somebody pasted an un-versioned binary into a bug report.
var version = "dev"

func main() {
	// Flags
	userFlag := flag.Bool("user", false, "Connect to the user systemd manager")
	systemFlag := flag.Bool("system", false, "Connect to the system systemd manager (default)")
	noCacheFlag := flag.Bool("no-cache", false, "Skip the on-disk service-list cache for this run (no read, no write)")
	clearCacheFlag := flag.Bool("clear-cache", false, "Delete jeeves' on-disk service-list cache and exit")
	cacheInfoFlag := flag.Bool("cache-info", false, "Print where the on-disk cache lives and what's in it, then exit")
	versionFlag := flag.Bool("version", false, "Print version and exit")
	flag.BoolVar(versionFlag, "v", false, "Print version and exit")
	helpFlag := flag.Bool("help", false, "Show help information")
	flag.BoolVar(helpFlag, "h", false, "Show help information")

	flag.Parse()

	if *versionFlag {
		// One line, no decoration — easy to grep, easy to paste into a bug
		// report. GOOS/GOARCH on the same line tells us what build it is.
		fmt.Printf("jeeves %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	if *helpFlag {
		printHelp()
		os.Exit(0)
	}

	// Cache-only commands run before any D-Bus work so they're usable even
	// when systemd isn't reachable.
	if *cacheInfoFlag {
		printCacheInfo()
		os.Exit(0)
	}
	if *clearCacheFlag {
		removed, err := systemd.ClearCache()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error clearing cache: %v\n", err)
			os.Exit(1)
		}
		if len(removed) == 0 {
			fmt.Println("No cache files to remove.")
		} else {
			fmt.Println("Removed:")
			for _, p := range removed {
				fmt.Printf("  %s\n", p)
			}
		}
		os.Exit(0)
	}

	var mode *systemd.Mode
	if *userFlag {
		m := systemd.UserMode
		mode = &m
	} else if *systemFlag {
		m := systemd.SystemMode
		mode = &m
	}

	// D-Bus connection is deferred into the TUI's first command (see model.Init).
	// Doing it here would block before the alt-screen takes over — on a Pi 3B+
	// under sudo that handshake is enough of a hitch that the user sees a frozen
	// terminal before anything paints. Instead the model renders chrome with a
	// "Connecting..." status, then the connect + first fetch run in the
	// background.
	useCache := !*noCacheFlag
	model := tui.NewModel(mode, useCache)

	// Initialize Bubble Tea program. WithMouseCellMotion enables scroll-wheel
	// support inside viewports (logs, unit file).
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())

	finalModel, err := p.Run()
	if err != nil {
		log.Fatalf("Error running program: %v", err)
	}
	if fm, ok := finalModel.(tui.Model); ok {
		fm.Close()
	}
}

func printHelp() {
	fmt.Println("jeeves — your personal systemd butler (a TUI over D-Bus)")
	fmt.Println("\nUsage:")
	fmt.Println("  jeeves [flags]")
	fmt.Println("\nFlags:")
	fmt.Println("  --user         Connect to the user systemd manager (no root required)")
	fmt.Println("  --system       Force connect to the system systemd manager (requires root/sudo)")
	fmt.Println("  --no-cache     Skip the on-disk service-list cache for this run")
	fmt.Println("  --clear-cache  Delete jeeves' on-disk cache and exit")
	fmt.Println("  --cache-info   Show where the cache lives and what's in it, then exit")
	fmt.Println("  -v, --version  Print version and exit")
	fmt.Println("  -h, --help     Show this help information")
	fmt.Println("\nCache:")
	fmt.Println("  To make startup snappy on slow hardware, jeeves caches the merged")
	fmt.Println("  service list (names, states, descriptions, enable-states) to disk after")
	fmt.Println("  each run and paints it on the next start while the live data is fetched.")
	fmt.Println("  Nothing else is cached — no logs, no unit-file contents, no actions.")
	fmt.Println("  System and user buses get separate files (the lists differ).")
	if dir := systemd.CacheDir(); dir != "" {
		fmt.Printf("  Location: %s/\n", dir)
	}
	fmt.Println("  Cached data is shown for at most 7 days, then refetched from scratch.")
	fmt.Println("  Use --no-cache to disable for one run, or --clear-cache to wipe.")
}

func printCacheInfo() {
	dir := systemd.CacheDir()
	if dir == "" {
		fmt.Println("No usable cache directory available on this system.")
		return
	}
	fmt.Printf("Cache directory: %s\n", dir)
	fmt.Println()
	fmt.Println("Files:")
	entries := systemd.CacheInventory()
	any := false
	for _, e := range entries {
		fmt.Printf("  [%s bus]\n", e.Mode.String())
		fmt.Printf("    path:  %s\n", e.Path)
		if !e.Exists {
			fmt.Println("    state: (no cache file yet)")
			continue
		}
		any = true
		fmt.Printf("    size:  %d bytes\n", e.Size)
		if !e.SavedAt.IsZero() {
			age := time.Since(e.SavedAt).Round(time.Second)
			fmt.Printf("    saved: %s (%s ago)\n", e.SavedAt.Format(time.RFC3339), age)
		}
		switch {
		case e.SavedAt.IsZero():
			fmt.Println("    state: corrupt or wrong version (will be ignored)")
		case !e.Valid:
			fmt.Println("    state: expired (>7 days old, will be ignored)")
		default:
			fmt.Println("    state: valid")
		}
	}
	if !any {
		fmt.Println("\n(No cache files have been written yet. Run jeeves once to populate.)")
	}
	fmt.Println("\nWipe with: jeeves --clear-cache")
}
