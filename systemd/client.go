package systemd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// DBusUint64Unset is the sentinel uint64 systemd returns when a property is
// "not set" (uint64 max). It's not a real value — treat it like 0.
const DBusUint64Unset = ^uint64(0)

// IsUnset reports whether v is missing data (zero or systemd's "unset" sentinel).
func IsUnset(v uint64) bool { return v == 0 || v == DBusUint64Unset }

// ErrPermissionDenied is returned by action methods when polkit refuses the call
// without prompting (the typed analogue of "permission denied" string-sniffing).
var ErrPermissionDenied = errors.New("permission denied")

func mapActionErr(err error) error {
	if err == nil {
		return nil
	}
	var dbusErr godbus.Error
	if errors.As(err, &dbusErr) {
		switch dbusErr.Name {
		case "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired",
			"org.freedesktop.DBus.Error.AccessDenied":
			return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
		}
	}
	// Some setups deliver a plain godbus.Error pointer.
	var dbusErrPtr *godbus.Error
	if errors.As(err, &dbusErrPtr) && dbusErrPtr != nil {
		switch dbusErrPtr.Name {
		case "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired",
			"org.freedesktop.DBus.Error.AccessDenied":
			return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
		}
	}
	return err
}

type Mode int

const (
	SystemMode Mode = iota
	UserMode
)

func (m Mode) String() string {
	if m == SystemMode {
		return "System"
	}
	return "User"
}

type ServiceInfo struct {
	Name                 string
	Description          string
	LoadState            string
	ActiveState          string
	SubState             string
	UnitFileState        string // enabled, disabled, static, masked, etc.
	FragmentPath         string // path to the .service unit file on disk
	MainPID              uint32
	ExecMainCode         int32
	ExecMainStatus       int32
	MemoryCurrent        uint64
	MemoryLimit          uint64
	CPUUsageNSec         uint64
	TasksCurrent         uint64
	TasksMax             uint64
	ActiveEnterTimestamp uint64
	ActiveExitTimestamp  uint64
	IPTrafficRxBytes     uint64
	IPTrafficTxBytes     uint64
	IOReadBytes          uint64
	IOWriteBytes         uint64
}

type Client struct {
	conn *dbus.Conn
	mode Mode
}

func NewClient(requestedMode *Mode) (*Client, error) {
	var conn *dbus.Conn
	var err error
	var finalMode Mode

	if requestedMode != nil {
		finalMode = *requestedMode
		if finalMode == SystemMode {
			conn, err = dbus.NewSystemdConnection()
		} else {
			conn, err = dbus.NewUserConnection()
		}
		if err != nil {
			return nil, err
		}
	} else {
		// Auto-detect
		conn, err = dbus.NewSystemdConnection()
		if err != nil {
			// Fallback to user bus
			conn, err = dbus.NewUserConnection()
			if err != nil {
				return nil, fmt.Errorf("failed to connect to system or user systemd dbus: %w", err)
			}
			finalMode = UserMode
		} else {
			finalMode = SystemMode
		}
	}

	return &Client{
		conn: conn,
		mode: finalMode,
	}, nil
}

func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

func (c *Client) Mode() Mode {
	return c.mode
}

// ListLoadedServices fetches only the *.service units systemd already has in
// memory — active, activating, failed, or recently-loaded. It's the cheap half
// of a full listing: no disk scan, no [Install]-section parsing. On a Pi 3B+
// this returns in tens of ms vs hundreds for the full list.
//
// UnitFileState is left empty on the returned entries; merge with
// ListUnitFiles (or call ListServices for both in one shot) to fill it in.
func (c *Client) ListLoadedServices(ctx context.Context) ([]ServiceInfo, error) {
	units, err := c.conn.ListUnitsByPatternsContext(ctx, nil, []string{"*.service"})
	if err != nil {
		return nil, fmt.Errorf("failed to list units: %w", err)
	}

	services := make([]ServiceInfo, 0, len(units))
	for _, u := range units {
		// systemd globs match against aliases too, so the pattern filter can
		// still let non-service entries through. Belt-and-braces.
		if !strings.HasSuffix(u.Name, ".service") {
			continue
		}
		services = append(services, ServiceInfo{
			Name:        u.Name,
			Description: u.Description,
			LoadState:   u.LoadState,
			ActiveState: u.ActiveState,
			SubState:    u.SubState,
		})
	}
	return services, nil
}

// ListUnitFiles fetches the on-disk enablement state for every installed
// service. This is the expensive half — systemd scans every unit directory
// and parses [Install] sections — and the dominant cost of a cold listing on
// slow flash storage.
func (c *Client) ListUnitFiles(ctx context.Context) ([]ServiceInfo, error) {
	unitFiles, err := c.conn.ListUnitFilesByPatternsContext(ctx, nil, []string{"*.service"})
	if err != nil {
		return nil, fmt.Errorf("failed to list unit files: %w", err)
	}

	out := make([]ServiceInfo, 0, len(unitFiles))
	for _, uf := range unitFiles {
		name := filepath.Base(uf.Path)
		if !strings.HasSuffix(name, ".service") {
			continue
		}
		out = append(out, ServiceInfo{
			Name:          name,
			LoadState:     "not-loaded",
			ActiveState:   "inactive",
			SubState:      "dead",
			UnitFileState: uf.Type,
		})
	}
	return out, nil
}

// MergeUnitFiles splices enablement state from a ListUnitFiles result into a
// loaded-services list, and appends any installed-but-not-loaded services
// that weren't in `loaded`. The returned slice is unsorted — callers re-sort
// through filterServices anyway.
func MergeUnitFiles(loaded, files []ServiceInfo) []ServiceInfo {
	byName := make(map[string]int, len(loaded))
	for i := range loaded {
		byName[loaded[i].Name] = i
	}
	for _, f := range files {
		if idx, ok := byName[f.Name]; ok {
			loaded[idx].UnitFileState = f.UnitFileState
		} else {
			loaded = append(loaded, f)
			byName[f.Name] = len(loaded) - 1
		}
	}
	return loaded
}

// GetServiceDetails fetches detailed properties for a specific service.
func (c *Client) GetServiceDetails(ctx context.Context, name string) (*ServiceInfo, error) {
	props, err := c.conn.GetUnitPropertiesContext(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get unit properties: %w", err)
	}

	info := &ServiceInfo{
		Name: name,
	}

	if val, ok := props["Description"].(string); ok {
		info.Description = val
	}
	if val, ok := props["LoadState"].(string); ok {
		info.LoadState = val
	}
	if val, ok := props["ActiveState"].(string); ok {
		info.ActiveState = val
	}
	if val, ok := props["SubState"].(string); ok {
		info.SubState = val
	}
	if val, ok := props["UnitFileState"].(string); ok {
		info.UnitFileState = val
	}
	if val, ok := props["FragmentPath"].(string); ok {
		info.FragmentPath = val
	}

	if val, ok := props["ActiveEnterTimestamp"].(uint64); ok {
		info.ActiveEnterTimestamp = val
	}
	if val, ok := props["ActiveExitTimestamp"].(uint64); ok {
		info.ActiveExitTimestamp = val
	}

	// Fetch service-specific properties
	sProps, err := c.conn.GetUnitTypePropertiesContext(ctx, name, "Service")
	if err == nil {
		if val, ok := sProps["MainPID"].(uint32); ok {
			info.MainPID = val
		}
		if val, ok := sProps["MemoryCurrent"].(uint64); ok {
			info.MemoryCurrent = val
		}
		if val, ok := sProps["MemoryLimit"].(uint64); ok {
			info.MemoryLimit = val
		}
		if val, ok := sProps["CPUUsageNSec"].(uint64); ok {
			info.CPUUsageNSec = val
		}
		if val, ok := sProps["TasksCurrent"].(uint64); ok {
			info.TasksCurrent = val
		}
		if val, ok := sProps["TasksMax"].(uint64); ok {
			info.TasksMax = val
		}
		if val, ok := sProps["ExecMainCode"].(int32); ok {
			info.ExecMainCode = val
		}
		if val, ok := sProps["ExecMainStatus"].(int32); ok {
			info.ExecMainStatus = val
		}
		if val, ok := sProps["IPTrafficRxBytes"].(uint64); ok {
			info.IPTrafficRxBytes = val
		}
		if val, ok := sProps["IPTrafficTxBytes"].(uint64); ok {
			info.IPTrafficTxBytes = val
		}
		if val, ok := sProps["IOReadBytes"].(uint64); ok {
			info.IOReadBytes = val
		}
		if val, ok := sProps["IOWriteBytes"].(uint64); ok {
			info.IOWriteBytes = val
		}
	}

	return info, nil
}

// Actions
func (c *Client) StartService(ctx context.Context, name string) error {
	ch := make(chan string, 1)
	_, err := c.conn.StartUnitContext(ctx, name, "replace", ch)
	return mapActionErr(err)
}

func (c *Client) StopService(ctx context.Context, name string) error {
	ch := make(chan string, 1)
	_, err := c.conn.StopUnitContext(ctx, name, "replace", ch)
	return mapActionErr(err)
}

func (c *Client) RestartService(ctx context.Context, name string) error {
	ch := make(chan string, 1)
	_, err := c.conn.RestartUnitContext(ctx, name, "replace", ch)
	return mapActionErr(err)
}

func (c *Client) EnableService(ctx context.Context, name string) error {
	_, _, err := c.conn.EnableUnitFilesContext(ctx, []string{name}, false, false)
	return mapActionErr(err)
}

func (c *Client) DisableService(ctx context.Context, name string) error {
	_, err := c.conn.DisableUnitFilesContext(ctx, []string{name}, false)
	return mapActionErr(err)
}

// ReadUnitFile returns the contents of the unit file at path. It refuses paths
// outside the conventional systemd unit directories so a malformed FragmentPath
// can't trick us into reading arbitrary files. Returns ("", nil) if path is empty
// (e.g. for transient or generated units that have no on-disk file).
func (c *Client) ReadUnitFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	clean := filepath.Clean(path)
	allowed := false
	for _, prefix := range []string{
		"/etc/systemd/",
		"/usr/lib/systemd/",
		"/lib/systemd/",
		"/run/systemd/",
		"/usr/local/lib/systemd/",
	} {
		if strings.HasPrefix(clean, prefix) {
			allowed = true
			break
		}
	}
	// User units live under XDG dirs inside $HOME. Limit to those two known
	// roots rather than "anywhere under $HOME" so a hostile FragmentPath can't
	// trick us into reading e.g. ~/.ssh/id_rsa.
	if !allowed {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			home = filepath.Clean(home)
			for _, sub := range []string{
				filepath.Join(home, ".config", "systemd") + string(os.PathSeparator),
				filepath.Join(home, ".local", "share", "systemd") + string(os.PathSeparator),
			} {
				if strings.HasPrefix(clean, sub) {
					allowed = true
					break
				}
			}
		}
	}
	if !allowed {
		return "", fmt.Errorf("refusing to read unit file outside systemd dirs: %s", clean)
	}

	data, err := os.ReadFile(clean)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// GetLogs fetches the recent logs of a service using journalctl.
func (c *Client) GetLogs(ctx context.Context, name string, limit int) (string, error) {
	args := []string{}
	if c.mode == UserMode {
		args = append(args, "--user")
	}
	args = append(args, "-u", name, "-n", strconv.Itoa(limit), "--no-pager")

	cmd := exec.CommandContext(ctx, "journalctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("journalctl failed: %w (output: %s)", err, string(output))
	}
	return string(output), nil
}

// FollowLogs starts `journalctl -fn <limit>` for the given unit and emits each
// stdout line on the returned channel. The channel is closed when ctx is canceled
// or the process exits. Errors during streaming are surfaced as a synthetic line
// prefixed with "[follow error]" rather than as a separate channel, to keep the
// caller's append-to-buffer loop uniform.
func (c *Client) FollowLogs(ctx context.Context, name string, limit int) (<-chan string, error) {
	args := []string{}
	if c.mode == UserMode {
		args = append(args, "--user")
	}
	args = append(args, "-u", name, "-n", strconv.Itoa(limit), "-f", "--no-pager")

	cmd := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("journalctl pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("journalctl start: %w", err)
	}

	out := make(chan string, 64)
	go func() {
		defer close(out)
		defer func() { _ = cmd.Wait() }()
		sc := bufio.NewScanner(stdout)
		// Allow long log lines (default scanner buffer is 64 KiB).
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			select {
			case <-ctx.Done():
				return
			case out <- sc.Text():
			}
		}
		if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			select {
			case out <- fmt.Sprintf("[follow error] %v", err):
			default:
			}
		}
	}()
	return out, nil
}
