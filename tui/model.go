package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aymanhs/jeeves/systemd"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// logLineChoices is the cycle of "last N lines" sizes the user can toggle through with [ / ].
var logLineChoices = []int{100, 250, 500, 1000, 2500}

// actionPastTenseTable hard-codes the past tense for each action verb. Hand-rolled
// "+ed"/"+d" suffixing gave "stoped" — past-tense English doesn't suffix-rule cleanly.
var actionPastTenseTable = map[string]string{
	"start":   "started",
	"stop":    "stopped",
	"restart": "restarted",
	"enable":  "enabled",
	"disable": "disabled",
	"mask":    "masked",
	"unmask":  "unmasked",
}

func actionPastTense(action string) string {
	if p, ok := actionPastTenseTable[action]; ok {
		return p
	}
	return action + "ed"
}

type viewType int

const (
	listView viewType = iota
	detailView
	unitFileView
)

type showMode int

const (
	showAll showMode = iota
	showRunning
	showFailed
	showModeCount // sentinel — keep last for modulo cycling
)

func (s showMode) String() string {
	switch s {
	case showRunning:
		return "Running"
	case showFailed:
		return "Failed"
	default:
		return "All"
	}
}

type sortMode int

const (
	sortByName sortMode = iota
	sortByState
	sortByMemory
	sortByPID
	sortModeCount
)

func (s sortMode) String() string {
	switch s {
	case sortByState:
		return "state"
	case sortByMemory:
		return "memory"
	case sortByPID:
		return "PID"
	default:
		return "name"
	}
}

type focusSection int

const (
	focusInfo focusSection = iota
	focusLogs
)

// Messages
type servicesFetchedMsg struct {
	services []systemd.ServiceInfo
	err      error
}

// loadedServicesFetchedMsg carries the cheap "already-loaded units" half of
// the initial fetch. It paints the list immediately so the user isn't staring
// at "Loading..." while the slower on-disk scan runs.
type loadedServicesFetchedMsg struct {
	services []systemd.ServiceInfo
	err      error
}

// unitFilesFetchedMsg carries the slower on-disk unit-file scan. It merges
// enablement state into the already-displayed list and appends installed-
// but-not-loaded services.
type unitFilesFetchedMsg struct {
	files []systemd.ServiceInfo
	err   error
}

type detailsFetchedMsg struct {
	details  *systemd.ServiceInfo
	logs     string
	unitFile string
	err      error
}

type actionCompletedMsg struct {
	action      string
	serviceName string
	err         error
}

type statusTimeoutMsg struct {
	id uint32
}

// liveTickMsg fires every refreshTickInterval while the detail view is open and
// triggers a lightweight metrics-only refetch (no logs, no unit file).
type liveTickMsg struct {
	generation uint64
}

const refreshTickInterval = 2 * time.Second

// metricsFetchedMsg carries refreshed memory/CPU/Tasks/timestamps for the active
// service — strictly less than detailsFetchedMsg, used by the live ticker.
type metricsFetchedMsg struct {
	name string
	info *systemd.ServiceInfo
	err  error
}

// logsAppendedMsg arrives from the FollowLogs goroutine with a chunk of new lines.
// generation lets us discard messages from a previous follow session.
type logsAppendedMsg struct {
	generation uint64
	lines      string
	closed     bool // true when the journalctl process ended
}

// connectedMsg is delivered when the D-Bus connection has been established (or
// failed). Connection is deferred into the model's Init command so the
// alt-screen + chrome paint *before* polkit/sudo handshakes block.
type connectedMsg struct {
	client *systemd.Client
	err    error
}

// cachedServicesMsg carries the optimistic paint from the on-disk service
// cache (see systemd/cache.go). Lands before connectedMsg so the user sees a
// real list as soon as the alt-screen comes up; the live fetch overwrites it
// when it returns.
type cachedServicesMsg struct {
	services []systemd.ServiceInfo
	mode     systemd.Mode
}

// Model represents the bubbletea application state.
type Model struct {
	appName    string
	appVersion string

	client           *systemd.Client
	requestedMode    *systemd.Mode // honored when client is connected lazily
	useCache         bool          // false → skip both cache read on startup and cache write after fetch
	services         []systemd.ServiceInfo
	filteredServices []systemd.ServiceInfo
	selectedIndex    int
	scrollOffset     int

	// True while we're showing cached data and the live fetch hasn't returned
	// yet — used to dim the "Bus:" header into a "(cached)" hint so the user
	// can tell the on-screen state may be a few seconds out of date.
	showingCached bool
	cachedMode    systemd.Mode // bus the displayed cache was saved against

	// Search/Filter/Sort
	searchInput textinput.Model
	filtering   bool
	filterQuery string
	sortMode    sortMode

	// Confirmation prompt for destructive actions (stop/restart/disable).
	confirmAction      string // verb being confirmed ("" when not confirming)
	confirmServiceName string

	// Live ticker — re-fetches details every refreshTickInterval when on the
	// detail view, so memory/CPU/Tasks update without manual R.
	tickGeneration uint64

	// Follow-logs (F3): a background tail -f pumps lines into followLogsCh, the
	// model appends them to m.logs as logsAppendedMsg events arrive.
	followLogs       bool
	followCancel     context.CancelFunc
	followGeneration uint64 // bumped on stop so stale msgs are ignored
	followCh         <-chan string

	// Navigation & Views
	currentView          viewType
	showMode             showMode
	detailFocusedSection focusSection
	activeDetail         *systemd.ServiceInfo
	logs                 string // raw, unwrapped log text most recently fetched
	logViewport          viewport.Model
	logWrap              bool
	logLineLimitIdx      int // index into logLineChoices
	logRawLineCount      int // number of source log lines actually returned

	// Unit file contents (shown on its own full-screen view via the `u` key).
	unitFile         string
	unitFileViewport viewport.Model

	// True until the first services list arrives — drives the startup "Loading..."
	// message in place of "no services found".
	initialLoad bool

	// Status messages
	statusMsg   string
	statusIsErr bool
	statusMsgID uint32

	// Window dimensions
	width  int
	height int

	// Loading state. `loading` flips off when both halves of the two-phase
	// fetch have arrived (loaded units + unit-file scan); `pendingFetches`
	// counts them down.
	loading        bool
	pendingFetches int
	err            error
}

// NewModel initializes the Bubble Tea model. The D-Bus connection itself is
// deferred to Init so the alt-screen and chrome render before any blocking
// systemd/polkit work.
//
// useCache controls the optimistic-paint cache (see systemd/cache.go). When
// false, no cache file is read on startup and no cache is written after the
// fetch completes — wired up to the --no-cache CLI flag.
func NewModel(mode *systemd.Mode, useCache bool, appVersion string) Model {
	ti := textinput.New()
	ti.Placeholder = "Type to filter services..."
	ti.Prompt = "/ "
	ti.PromptStyle = SearchPromptStyle
	ti.TextStyle = SearchInputStyle
	ti.CharLimit = 128 // service names go up to ~70 chars; leave room for typos + paste

	vp := viewport.New(0, 0)
	vp.KeyMap = viewport.DefaultKeyMap()

	uvp := viewport.New(0, 0)
	uvp.KeyMap = viewport.DefaultKeyMap()

	return Model{
		appName:              "jeeves",
		appVersion:           appVersion,
		requestedMode:        mode,
		useCache:             useCache,
		searchInput:          ti,
		currentView:          listView,
		showMode:             showAll,
		detailFocusedSection: focusInfo,
		logViewport:          vp,
		unitFileViewport:     uvp,
		logWrap:              true,
		logLineLimitIdx:      1, // default 250
		initialLoad:          true,
		loading:              true, // chrome shows "Loading..." until connect+fetch land
	}
}

// Init triggers the lazy D-Bus connection. The first fetch is enqueued from
// the connectedMsg handler so we don't try to call into a nil client.
//
// In parallel we kick off a cache-load cmd: if we ran recently against the
// same bus we'll paint the previous service list as soon as the alt-screen
// comes up, so the user sees real data instead of "Loading services..."
// while the live ListUnits round-trip (~1s on a Pi 3B+) is in flight.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, connectCmd(m.requestedMode)}
	if m.useCache {
		cmds = append(cmds, loadCacheCmd(m.requestedMode))
	}
	return tea.Batch(cmds...)
}

// connectCmd opens the systemd D-Bus connection off the main render loop. On
// a Pi 3B+ under sudo this can take a noticeable fraction of a second; doing
// it here means the alt-screen + chrome paint first.
func connectCmd(mode *systemd.Mode) tea.Cmd {
	return func() tea.Msg {
		client, err := systemd.NewClient(mode)
		return connectedMsg{client: client, err: err}
	}
}

// loadCacheCmd reads the previous service list off disk. If the user didn't
// pin a mode with --user/--system we guess: euid 0 → system, otherwise system
// first then user (matches NewClient's autodetect order). The cmd returns nil
// (a no-op msg) when no usable cache exists.
func loadCacheCmd(requested *systemd.Mode) tea.Cmd {
	return func() tea.Msg {
		modes := guessCacheModes(requested)
		for _, mode := range modes {
			if svcs, ok := systemd.LoadServiceCache(mode); ok {
				return cachedServicesMsg{services: svcs, mode: mode}
			}
		}
		return nil
	}
}

// guessCacheModes returns the mode(s) to probe the cache for, ordered by
// likelihood. Matches NewClient's autodetect: system bus first unless the
// user explicitly asked for the user bus.
func guessCacheModes(requested *systemd.Mode) []systemd.Mode {
	if requested != nil {
		return []systemd.Mode{*requested}
	}
	return []systemd.Mode{systemd.SystemMode, systemd.UserMode}
}

// Close releases the D-Bus connection. Safe to call when the connection was
// never established. main() calls this after p.Run() returns; doing it here
// (instead of in main) keeps the lifecycle next to the connect logic.
func (m *Model) Close() {
	if m.client != nil {
		m.client.Close()
		m.client = nil
	}
}

// Commands
//
// fetchServicesCmd kicks off a two-phase fetch: the cheap "currently-loaded
// units" call runs first and paints the list immediately, then the slower
// on-disk unit-file scan merges in enablement state for already-shown rows
// and appends installed-but-not-loaded services. On a Pi 3B+ the first phase
// returns in tens of ms while the second can take 1–2s — so the user sees
// the list almost instantly instead of waiting on the slow half.
func (m *Model) fetchServicesCmd() tea.Cmd {
	m.loading = true
	m.pendingFetches = 2
	return tea.Batch(m.fetchLoadedServicesCmd(), m.fetchUnitFilesCmd())
}

func (m *Model) fetchLoadedServicesCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		services, err := m.client.ListLoadedServices(ctx)
		return loadedServicesFetchedMsg{services: services, err: err}
	}
}

func (m *Model) fetchUnitFilesCmd() tea.Cmd {
	return func() tea.Msg {
		// Bigger timeout — this is the slow call on flash storage.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		files, err := m.client.ListUnitFiles(ctx)
		return unitFilesFetchedMsg{files: files, err: err}
	}
}

// scheduleTickCmd returns a tea.Cmd that fires liveTickMsg after the configured
// interval. We use generation so a stale tick from a previous detail-view session
// (after the user went back to the list and reopened a different service) is
// ignored.
func (m *Model) scheduleTickCmd() tea.Cmd {
	gen := m.tickGeneration
	return tea.Tick(refreshTickInterval, func(time.Time) tea.Msg {
		return liveTickMsg{generation: gen}
	})
}

// fetchMetricsCmd is the lightweight refresh that fires on every tick. It only
// hits D-Bus (no journalctl, no file read), so it stays cheap enough to run
// every few seconds.
func (m *Model) fetchMetricsCmd(name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		info, err := m.client.GetServiceDetails(ctx, name)
		return metricsFetchedMsg{name: name, info: info, err: err}
	}
}

func (m *Model) fetchDetailsCmd(name string) tea.Cmd {
	m.loading = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()

		details, err := m.client.GetServiceDetails(ctx, name)
		if err != nil {
			return detailsFetchedMsg{err: err}
		}

		// Fetch the user-selected number of lines (default 250)
		logs, logsErr := m.client.GetLogs(ctx, name, m.logLineLimit())
		logsStr := logs
		if logsErr != nil {
			logsStr = fmt.Sprintf("Failed to fetch logs: %v", logsErr)
		}

		// Read the unit file body. Failures here are non-fatal — many units have no
		// FragmentPath (transient, generated), so we just show a friendly message.
		var unitFile string
		if details.FragmentPath == "" {
			unitFile = "(no unit file on disk — transient or generated unit)"
		} else if body, err := m.client.ReadUnitFile(details.FragmentPath); err != nil {
			unitFile = fmt.Sprintf("Failed to read %s: %v", details.FragmentPath, err)
		} else {
			unitFile = body
		}

		return detailsFetchedMsg{details: details, logs: logsStr, unitFile: unitFile}
	}
}

// destructiveActions are the verbs that require y/N confirmation. Pressing the
// key once arms a banner; pressing y/Y/enter confirms; anything else cancels.
var destructiveActions = map[string]bool{
	"stop":    true,
	"restart": true,
	"disable": true,
	"mask":    true,
}

// requestAction either fires the action immediately or arms a confirmation banner
// for destructive verbs. Returns the tea.Cmd to enqueue.
func (m *Model) requestAction(action, name string) tea.Cmd {
	if destructiveActions[action] {
		m.confirmAction = action
		m.confirmServiceName = name
		// No status timeout — the banner stays until the user answers.
		m.statusMsg = fmt.Sprintf("Confirm %s %s? [y/N]", action, name)
		m.statusIsErr = false
		m.statusMsgID++ // invalidate any in-flight timeout
		return nil
	}
	return m.runActionCmd(action, name)
}

// startFollowLogsCmd kicks off `journalctl -fn N` for the active service. The
// channel is stashed on the model so handler-level continuations can re-arm the
// wait without re-piping every closure. Returns the first wait cmd.
func (m *Model) startFollowLogsCmd(name string) tea.Cmd {
	m.stopFollowLogs() // make sure no previous tail is still running
	m.followGeneration++
	gen := m.followGeneration
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := m.client.FollowLogs(ctx, name, m.logLineLimit())
	if err != nil {
		cancel()
		return m.triggerStatus(fmt.Sprintf("Follow failed: %v", err), true)
	}
	m.followCancel = cancel
	m.followLogs = true
	m.followCh = ch
	return waitFollowCmd(gen, ch)
}

// waitFollowCmd is the continuation primitive: blocks on the channel for the next
// line, returns it as a logsAppendedMsg, and the message handler enqueues another
// waitFollowCmd to keep the loop going.
func waitFollowCmd(gen uint64, ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return logsAppendedMsg{generation: gen, closed: true}
		}
		// Batch any immediately-available lines into the same message so a noisy
		// service doesn't blow up the message queue with 1000s of tiny msgs.
		batch := line
		for done := false; !done; {
			select {
			case more, stillOpen := <-ch:
				if !stillOpen {
					return logsAppendedMsg{generation: gen, lines: batch, closed: true}
				}
				batch += "\n" + more
			default:
				done = true
			}
		}
		return logsAppendedMsg{generation: gen, lines: batch}
	}
}

// stopFollowLogs cancels the running tail goroutine (if any). Safe to call
// multiple times; bumping the generation prevents stale msgs from being
// processed.
func (m *Model) stopFollowLogs() {
	if m.followCancel != nil {
		m.followCancel()
		m.followCancel = nil
	}
	m.followLogs = false
	m.followCh = nil
	m.followGeneration++
}

// trimLeadingLines drops the first n lines of s. Used to cap the in-memory log
// buffer when follow-mode is running.
func trimLeadingLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	for i := 0; i < n; i++ {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			return ""
		}
		s = s[idx+1:]
	}
	return s
}

func (m *Model) runActionCmd(action, name string) tea.Cmd {
	m.loading = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var err error
		switch action {
		case "start":
			err = m.client.StartService(ctx, name)
		case "stop":
			err = m.client.StopService(ctx, name)
		case "restart":
			err = m.client.RestartService(ctx, name)
		case "enable":
			err = m.client.EnableService(ctx, name)
		case "disable":
			err = m.client.DisableService(ctx, name)
		}

		return actionCompletedMsg{action: action, serviceName: name, err: err}
	}
}

func (m *Model) triggerStatus(msg string, isErr bool) tea.Cmd {
	m.statusMsg = msg
	m.statusIsErr = isErr
	m.statusMsgID++
	id := m.statusMsgID
	return func() tea.Msg {
		time.Sleep(4 * time.Second)
		return statusTimeoutMsg{id: id}
	}
}

// Update handles UI events and messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalculateViewportSize()

	case connectedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.loading = false
			m.initialLoad = false
			break
		}
		m.client = msg.client
		if m.requestedMode == nil && m.client.Mode() == systemd.UserMode {
			cmds = append(cmds, m.triggerStatus(
				"Auto-fallback to user bus (run with sudo for system units).",
				false))
		}
		// If we showed a cache for the wrong bus (rare — only when no flag
		// was passed and the autodetect landed somewhere our guess didn't),
		// discard it so we don't blend buses on screen.
		if m.showingCached && m.cachedMode != msg.client.Mode() {
			m.services = nil
			m.filteredServices = nil
			m.showingCached = false
		}
		cmds = append(cmds, m.fetchServicesCmd())

	case cachedServicesMsg:
		// Only paint the cache if the live fetch hasn't already overwritten
		// it — otherwise a slow cache read could clobber fresh data.
		if !m.initialLoad {
			break
		}
		m.services = msg.services
		m.showingCached = true
		m.cachedMode = msg.mode
		m.initialLoad = false
		m.filterServices()
		// The header already shows a "(cached)" tag; avoid a long startup
		// banner here because it can wrap and temporarily crowd the list down
		// to only a couple visible rows on smaller terminals.

	case servicesFetchedMsg:
		// Retained for any future callers that want a single-shot refresh; the
		// startup/refresh path now uses the two-phase messages below.
		if m.pendingFetches > 0 {
			m.pendingFetches--
			if m.pendingFetches == 0 {
				m.loading = false
			}
		} else {
			m.loading = false
		}
		m.initialLoad = false
		if msg.err != nil {
			m.err = msg.err
			cmds = append(cmds, m.triggerStatus(fmt.Sprintf("Fetch failed: %v", msg.err), true))
		} else {
			m.services = msg.services
			m.filterServices()
		}

	case loadedServicesFetchedMsg:
		wasInitialLoad := m.initialLoad
		hadCachedPaint := m.showingCached
		if m.pendingFetches > 0 {
			m.pendingFetches--
			if m.pendingFetches == 0 {
				m.loading = false
			}
		}
		m.initialLoad = false
		if msg.err != nil {
			cmds = append(cmds, m.triggerStatus(fmt.Sprintf("Fetch failed: %v", msg.err), true))
			break
		}
		// Make this phase order-independent: if we already have a superset with
		// UnitFileState data (from cache or because unit-files phase landed
		// first), splice live loaded state onto that superset instead of
		// replacing it with the loaded-only subset.
		if hasAnyUnitFileState(m.services) && len(m.services) >= len(msg.services) {
			liveByName := make(map[string]systemd.ServiceInfo, len(msg.services))
			for _, ls := range msg.services {
				liveByName[ls.Name] = ls
			}

			merged := append([]systemd.ServiceInfo(nil), m.services...)
			for i := range merged {
				if ls, ok := liveByName[merged[i].Name]; ok {
					merged[i].Description = ls.Description
					merged[i].LoadState = ls.LoadState
					merged[i].ActiveState = ls.ActiveState
					merged[i].SubState = ls.SubState
					delete(liveByName, merged[i].Name)
				}
			}
			// If a live loaded unit wasn't present in cache, append it.
			for _, ls := range liveByName {
				merged = append(merged, ls)
			}
			m.services = merged
		} else {
			// Paint immediately: replace the loaded slice but preserve any
			// UnitFileState we already have for names that are still present, so a
			// refresh doesn't blink the Enable column back to "-" before the
			// slower unit-file scan returns.
			prevEnable := make(map[string]string, len(m.services))
			for _, s := range m.services {
				if s.UnitFileState != "" {
					prevEnable[s.Name] = s.UnitFileState
				}
			}
			for i := range msg.services {
				if st, ok := prevEnable[msg.services[i].Name]; ok {
					msg.services[i].UnitFileState = st
				}
			}
			m.services = msg.services
		}
		// On the very first load without cache, don't paint the loaded-only
		// subset (often just 1-2 active units). Wait for the unit-file phase
		// so startup doesn't appear to "randomly" show tiny lists.
		if !(wasInitialLoad && !hadCachedPaint) {
			m.filterServices()
		}

	case unitFilesFetchedMsg:
		if m.pendingFetches > 0 {
			m.pendingFetches--
			if m.pendingFetches == 0 {
				m.loading = false
			}
		}
		if msg.err != nil {
			// Non-fatal: the list still works, just without enablement info
			// for not-loaded services. Surface a quiet status.
			cmds = append(cmds, m.triggerStatus(fmt.Sprintf("Unit file scan failed: %v", msg.err), true))
			if len(m.filteredServices) == 0 && len(m.services) > 0 {
				// If we intentionally held back loaded-only paint during initial
				// startup, fall back to it when phase 2 fails so the user still
				// sees something useful.
				m.filterServices()
			}
			break
		}
		m.services = systemd.MergeUnitFiles(m.services, msg.files)
		// Full live merge has landed; clear the cached-data hint now.
		m.showingCached = false
		m.filterServices()
		// Persist the merged list so the next start can paint instantly. Fire
		// the write off a goroutine; the file IO isn't worth a tea.Cmd round
		// trip and any error is best-effort. Skipped under --no-cache.
		//
		// Always cache in name order regardless of the user's current sort.
		// The next start opens on sortByName (zero value) — if we wrote the
		// list in, say, memory order, the optimistic paint would briefly show
		// rows in the wrong order before filterServices re-sorts on the live
		// fetch. Cache-by-name keeps the two paints visually consistent.
		// Also skip writes while a filter is active (text or mode). m.services
		// should already be the canonical unfiltered backing slice, but this
		// guard avoids persisting a confusing subset if that invariant ever
		// regresses again.
		if m.useCache && m.client != nil && m.filterQuery == "" && m.showMode == showAll {
			services := append([]systemd.ServiceInfo(nil), m.services...)
			sortServices(services, sortByName)
			mode := m.client.Mode()
			go func() { _ = systemd.SaveServiceCache(mode, services) }()
		}

	case detailsFetchedMsg:
		m.loading = false
		if msg.err != nil {
			cmds = append(cmds, m.triggerStatus(fmt.Sprintf("Failed to get details: %v", msg.err), true))
		} else {
			m.activeDetail = msg.details
			m.logs = msg.logs
			m.unitFile = msg.unitFile
			m.logRawLineCount = countLines(m.logs)
			m.refreshLogViewport()
			m.logViewport.GotoBottom()
			m.refreshUnitFileViewport()
			m.unitFileViewport.GotoTop()
			// Kick off the live ticker for memory/CPU updates.
			if m.currentView == detailView || m.currentView == unitFileView {
				m.tickGeneration++
				cmds = append(cmds, m.scheduleTickCmd())
			}
		}

	case metricsFetchedMsg:
		// Drop stale metrics from a previous service or after a back-to-list.
		if m.activeDetail == nil || msg.name != m.activeDetail.Name {
			break
		}
		if msg.err == nil && msg.info != nil {
			// Splice in only the live-updating fields; leave Description, Logs,
			// FragmentPath etc. alone.
			d := m.activeDetail
			d.ActiveState = msg.info.ActiveState
			d.SubState = msg.info.SubState
			d.MainPID = msg.info.MainPID
			d.MemoryCurrent = msg.info.MemoryCurrent
			d.CPUUsageNSec = msg.info.CPUUsageNSec
			d.TasksCurrent = msg.info.TasksCurrent
			d.IPTrafficRxBytes = msg.info.IPTrafficRxBytes
			d.IPTrafficTxBytes = msg.info.IPTrafficTxBytes
			d.IOReadBytes = msg.info.IOReadBytes
			d.IOWriteBytes = msg.info.IOWriteBytes
			d.ActiveEnterTimestamp = msg.info.ActiveEnterTimestamp
			d.ActiveExitTimestamp = msg.info.ActiveExitTimestamp
		}

	case liveTickMsg:
		// Ignore ticks from a previous detail-view session.
		if msg.generation != m.tickGeneration {
			break
		}
		if (m.currentView == detailView || m.currentView == unitFileView) && m.activeDetail != nil {
			cmds = append(cmds, m.fetchMetricsCmd(m.activeDetail.Name))
			cmds = append(cmds, m.scheduleTickCmd())
		}

	case logsAppendedMsg:
		if msg.generation != m.followGeneration || !m.followLogs {
			break
		}
		if msg.lines != "" {
			if m.logs == "" {
				m.logs = msg.lines
			} else {
				m.logs += "\n" + msg.lines
			}
			m.logRawLineCount = countLines(m.logs)
			// Cap the in-memory buffer at 10× the visible limit so a runaway
			// process can't grow the model forever.
			maxBuf := m.logLineLimit() * 10
			if maxBuf < 500 {
				maxBuf = 500
			}
			if m.logRawLineCount > maxBuf {
				m.logs = trimLeadingLines(m.logs, m.logRawLineCount-maxBuf)
				m.logRawLineCount = countLines(m.logs)
			}
			// Auto-scroll only when the user was already at the bottom — preserve
			// their scroll position if they're reading historical lines.
			wasAtBottom := m.logViewport.AtBottom()
			m.refreshLogViewport()
			if wasAtBottom {
				m.logViewport.GotoBottom()
			}
		}
		if msg.closed {
			m.followLogs = false
			if m.followCancel != nil {
				m.followCancel()
				m.followCancel = nil
			}
			m.followCh = nil
			cmds = append(cmds, m.triggerStatus("Follow ended", false))
		} else if m.followCh != nil {
			// Re-arm: wait for the next batch.
			cmds = append(cmds, waitFollowCmd(m.followGeneration, m.followCh))
		}

	case actionCompletedMsg:
		m.loading = false
		if msg.err != nil {
			// Extract a cleaner error message if it's D-Bus permission denied
			errStr := msg.err.Error()
			if strings.Contains(errStr, "permission denied") || strings.Contains(errStr, "InteractiveAuthenticationRequired") {
				errStr = "permission denied (try running with sudo)"
			}
			cmds = append(cmds, m.triggerStatus(fmt.Sprintf("Error running %s on %s: %s", msg.action, msg.serviceName, errStr), true))
		} else {
			cmds = append(cmds, m.triggerStatus(fmt.Sprintf("Successfully %s %s", actionPastTense(msg.action), msg.serviceName), false))
			// Refresh the view the user is currently looking at. The detail and
			// unit-file views both rely on m.activeDetail being current.
			if m.activeDetail != nil && m.activeDetail.Name == msg.serviceName &&
				(m.currentView == detailView || m.currentView == unitFileView) {
				cmds = append(cmds, m.fetchDetailsCmd(msg.serviceName))
			} else {
				cmds = append(cmds, m.fetchServicesCmd())
			}
		}

	case statusTimeoutMsg:
		if msg.id == m.statusMsgID {
			m.statusMsg = ""
		}

	case tea.KeyMsg:
		// Global keys. `q` only quits when not typing into the filter input —
		// otherwise it would swallow the letter mid-search.
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if !m.filtering && m.confirmAction == "" {
				return m, tea.Quit
			}
		}

		// If a confirmation banner is up, every key answers it. y/Y/enter fires
		// the pending action; any other key cancels and falls through to nothing.
		if m.confirmAction != "" {
			action, name := m.confirmAction, m.confirmServiceName
			m.confirmAction, m.confirmServiceName = "", ""
			m.statusMsg = ""
			switch msg.String() {
			case "y", "Y", "enter":
				return m, m.runActionCmd(action, name)
			}
			cmds = append(cmds, m.triggerStatus("Canceled", false))
			return m, tea.Batch(cmds...)
		}

		if m.filtering {
			// Search Input handling
			switch msg.String() {
			case "enter":
				m.filtering = false
				m.searchInput.Blur()
				m.filterServices()
			case "esc":
				m.filterQuery = ""
				m.searchInput.SetValue("")
				m.filtering = false
				m.searchInput.Blur()
				m.filterServices()
			default:
				m.searchInput, cmd = m.searchInput.Update(msg)
				cmds = append(cmds, cmd)
				m.filterQuery = m.searchInput.Value()
				m.filterServices()
			}
			return m, tea.Batch(cmds...)
		}

		// View-specific keys
		switch m.currentView {
		case listView:
			cmds = append(cmds, m.handleListKey(msg))
		case detailView:
			cmds = append(cmds, m.handleDetailKey(msg))
		case unitFileView:
			cmds = append(cmds, m.handleUnitFileKey(msg))
		}
	}

	// Adjust scroll offset for ListView
	m.adjustScrollOffset(m.listMaxRows())

	return m, tea.Batch(cmds...)
}

// Vertical budget of everything around the panels in the detail view, in rows:
//
//	2  page header (title row + blank)
//	1  newline after the panels block
//	2  status banner area (banner with margin, or "\n\n" placeholder)
//	4  footer (MarginTop(1) + BorderTop(1) + PaddingTop(1) + 1 content row)
//
// = 9. The panels' OUTER height (border included) is therefore m.height - 9.
const detailChromeRows = 9

// detailLayout returns the outer width/height of each panel box (info + log) along
// with the inner viewport size for the log box. Kept in one place so renderer + model
// agree exactly on geometry — otherwise borders overflow off the bottom of the screen
// and the page header scrolls off the top.
//
// Returned dimensions are OUTER (border-inclusive). Callers pass `outer - 2` to
// lipgloss's .Width()/.Height() because those set the inner content size.
func (m Model) detailLayout() (infoBoxW, infoBoxH, logBoxW, logBoxH, vpW, vpH int) {
	panelsOuterH := m.height - detailChromeRows
	if panelsOuterH < 8 {
		panelsOuterH = 8
	}

	if m.width >= 100 {
		// Side-by-side: fixed-width info column, log column gets the rest.
		infoBoxW = 45
		logBoxW = m.width - infoBoxW - 2 // -2 for DocStyle frame / gap
		if logBoxW < 24 {
			logBoxW = 24
		}
		infoBoxH = panelsOuterH
		logBoxH = panelsOuterH
	} else {
		// Stacked: both panels full width, split the vertical budget.
		infoBoxW = m.width - 2
		if infoBoxW < 24 {
			infoBoxW = 24
		}
		logBoxW = infoBoxW
		infoBoxH = panelsOuterH / 2
		logBoxH = panelsOuterH - infoBoxH
	}

	// BoxStyle = rounded border (2 cols / 2 rows) + Padding(0,1) (2 cols). Inside the
	// log box we also reserve 1 row for the panel header and 1 row for the meta row.
	vpW = logBoxW - 4 // 2 border + 2 padding
	if vpW < 10 {
		vpW = 10
	}
	vpH = logBoxH - 2 /* border */ - 2 /* header + meta rows */
	if vpH < 3 {
		vpH = 3
	}
	return
}

func (m *Model) recalculateViewportSize() {
	if m.width == 0 || m.height == 0 {
		return
	}
	_, _, _, _, vpW, vpH := m.detailLayout()
	if vpW != m.logViewport.Width || vpH != m.logViewport.Height {
		m.logViewport.Width = vpW
		m.logViewport.Height = vpH
		m.refreshLogViewport()
	}

	uw, uh := m.unitFileViewSize()
	if uw != m.unitFileViewport.Width || uh != m.unitFileViewport.Height {
		m.unitFileViewport.Width = uw
		m.unitFileViewport.Height = uh
		m.refreshUnitFileViewport()
	}
}

// listMaxRows is the number of service rows the list view can show. Used by both
// the renderer and the scroll-offset clamp so the two never drift apart.
//
//	1 page header row (title+version + bus status)
//	1 search row
//	1 mode row
//	2 table header rows
//	2 status banner rows
//	5 footer rows (explicit leading newline + footer margin/border/padding/content)
//
// = 12 rows of chrome.
func (m Model) listMaxRows() int {
	if m.height <= 0 {
		// Before the first WindowSizeMsg lands, avoid clamping the list to the
		// minimum 2-3 visible rows (which looks like "randomly only 2 services").
		if n := len(m.filteredServices); n > 0 {
			return n
		}
		return 20
	}
	r := m.height - 12
	if r < 3 {
		r = 3
	}
	return r
}

// unitFileViewSize returns the (width, height) of the scrollable unit-file viewport
// on its own full-screen view. The view chrome above/below it is fixed at:
//
//	2 page header (title + blank)
//	1 path subtitle row
//	1 blank
//	4 footer (margin+border+padding+content)
//
// = 8 rows.
const unitFileChromeRows = 8

func (m Model) unitFileViewSize() (w, h int) {
	w = m.width - 2 // -2 for DocStyle frame
	if w < 20 {
		w = 20
	}
	h = m.height - unitFileChromeRows
	if h < 4 {
		h = 4
	}
	return
}

// refreshUnitFileViewport hard-wraps the cached unit file to the viewport width so
// long lines don't spill, then pushes the result into the viewport. No styling on
// the body — keeps mouse copy/paste clean.
func (m *Model) refreshUnitFileViewport() {
	if m.unitFile == "" {
		m.unitFileViewport.SetContent("")
		return
	}
	width := m.unitFileViewport.Width
	if width < 1 {
		width = 1
	}
	m.unitFileViewport.SetContent(ansi.Hardwrap(m.unitFile, width, false))
}

// refreshLogViewport re-applies the current wrap setting + viewport width to the cached
// raw logs and pushes the result into the viewport. Call after width changes, wrap toggles,
// or new log content arrives.
func (m *Model) refreshLogViewport() {
	if m.logs == "" {
		m.logViewport.SetContent("")
		return
	}
	width := m.logViewport.Width
	if width < 1 {
		width = 1
	}
	var content string
	if m.logWrap {
		// ansi.Wrap word-wraps at spaces, falling back to hard-wrap when a token is
		// longer than the column. Preserves ANSI sequences across line breaks.
		content = ansi.Wrap(m.logs, width, "")
	} else {
		// Truncate every line so nothing leaks past the right border of the box.
		content = truncateLines(m.logs, width)
	}
	m.logViewport.SetContent(content)
}

// logLineLimit is the current "tail -n" the user has selected.
func (m Model) logLineLimit() int {
	if m.logLineLimitIdx < 0 || m.logLineLimitIdx >= len(logLineChoices) {
		return logLineChoices[0]
	}
	return logLineChoices[m.logLineLimitIdx]
}

// countLines returns the number of newline-terminated lines in s (with a tolerant
// last-line-without-\n).
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// truncateLines hard-truncates each line of s to fit visually within width cells. Used
// when wrap is OFF so overly long lines don't spill the box.
func truncateLines(s string, width int) string {
	if width <= 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		// ansi.Truncate keeps ANSI sequences intact and respects cell width.
		b.WriteString(ansi.Truncate(line, width, ""))
	}
	return b.String()
}

func hasAnyUnitFileState(services []systemd.ServiceInfo) bool {
	for _, s := range services {
		if s.UnitFileState != "" {
			return true
		}
	}
	return false
}

func (m *Model) filterServices() {
	// 1. Text filter
	var working []systemd.ServiceInfo
	if m.filterQuery == "" {
		working = append(working, m.services...)
	} else {
		q := strings.ToLower(m.filterQuery)
		for _, s := range m.services {
			if strings.Contains(strings.ToLower(s.Name), q) ||
				strings.Contains(strings.ToLower(s.Description), q) {
				working = append(working, s)
			}
		}
	}

	// 2. Mode filter
	final := working[:0]
	for _, s := range working {
		switch m.showMode {
		case showRunning:
			if s.ActiveState == "active" && s.SubState == "running" {
				final = append(final, s)
			}
		case showFailed:
			if s.ActiveState == "failed" {
				final = append(final, s)
			}
		default:
			final = append(final, s)
		}
	}

	// 3. Sort
	sortServices(final, m.sortMode)
	m.filteredServices = final

	// 4. Bounds checking for selectedIndex
	if m.selectedIndex >= len(m.filteredServices) {
		m.selectedIndex = len(m.filteredServices) - 1
	}
	if m.selectedIndex < 0 {
		m.selectedIndex = 0
	}
}

// sortServices sorts in place by the chosen key. Name is the tiebreaker for all
// modes so the order is deterministic across re-fetches.
func sortServices(svcs []systemd.ServiceInfo, mode sortMode) {
	// activeRank orders active > activating/deactivating > inactive > failed > rest.
	activeRank := func(s systemd.ServiceInfo) int {
		switch s.ActiveState {
		case "failed":
			return 0 // surface failures first
		case "active":
			return 1
		case "activating", "deactivating", "reloading":
			return 2
		case "inactive":
			return 3
		default:
			return 4
		}
	}
	sort.SliceStable(svcs, func(i, j int) bool {
		a, b := svcs[i], svcs[j]
		switch mode {
		case sortByState:
			if ra, rb := activeRank(a), activeRank(b); ra != rb {
				return ra < rb
			}
		case sortByMemory:
			am, bm := a.MemoryCurrent, b.MemoryCurrent
			if systemd.IsUnset(am) {
				am = 0
			}
			if systemd.IsUnset(bm) {
				bm = 0
			}
			if am != bm {
				return am > bm // largest first
			}
		case sortByPID:
			if a.MainPID != b.MainPID {
				return a.MainPID > b.MainPID
			}
		}
		return a.Name < b.Name
	})
}

func (m *Model) adjustScrollOffset(maxRows int) {
	if len(m.filteredServices) == 0 {
		m.scrollOffset = 0
		return
	}

	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}

	// Scroll up if index is above viewport
	if m.selectedIndex < m.scrollOffset {
		m.scrollOffset = m.selectedIndex
	}

	// Scroll down if index is below viewport
	if m.selectedIndex >= m.scrollOffset+maxRows {
		m.scrollOffset = m.selectedIndex - maxRows + 1
	}

	// Clamp to maximum possible scroll offset
	if len(m.filteredServices) <= maxRows {
		m.scrollOffset = 0
	} else if m.scrollOffset > len(m.filteredServices)-maxRows {
		m.scrollOffset = len(m.filteredServices) - maxRows
	}
}

// View renders the screen based on the current view state.
func (m Model) View() string {
	if m.err != nil {
		// If we never connected, the error is almost always a missing system
		// bus / permission denied — surface the sudo + --user hints the
		// command-line flow used to print.
		hint := ""
		if m.client == nil && (m.requestedMode == nil || *m.requestedMode == systemd.SystemMode) {
			hint = "\n\n  Tip: Managing system units usually requires root. Try: sudo jeeves" +
				"\n  Or run against the user bus instead:           jeeves --user"
		}
		return fmt.Sprintf("\n  Error: %v%s\n\n  Press Ctrl+C to quit.\n", m.err, hint)
	}

	switch m.currentView {
	case listView:
		return m.renderListView()
	case unitFileView:
		return m.renderUnitFileView()
	default:
		return m.renderDetailView()
	}
}
