package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/aymanhs/jeeves/systemd"
	"github.com/aymanhs/jeeves/tui"
	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	// Flags
	userFlag := flag.Bool("user", false, "Connect to the user systemd manager")
	systemFlag := flag.Bool("system", false, "Connect to the system systemd manager (default)")
	helpFlag := flag.Bool("help", false, "Show help information")
	flag.BoolVar(helpFlag, "h", false, "Show help information")

	flag.Parse()

	if *helpFlag {
		fmt.Println("jeeves — your personal systemd butler (a TUI over D-Bus)")
		fmt.Println("\nUsage:")
		fmt.Println("  jeeves [flags]")
		fmt.Println("\nFlags:")
		fmt.Println("  --user      Connect to the user systemd manager (no root required)")
		fmt.Println("  --system    Force connect to the system systemd manager (requires root/sudo)")
		fmt.Println("  -h, --help  Show this help information")
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

	// Connect to systemd D-Bus
	client, err := systemd.NewClient(mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to systemd: %v\n", err)
		if mode == nil || *mode == systemd.SystemMode {
			fmt.Fprintln(os.Stderr, "\nTip: Managing system units usually requires root. Try running: sudo ./jeeves")
			fmt.Fprintln(os.Stderr, "Alternatively, run with --user to manage user-level services: ./jeeves --user")
		}
		os.Exit(1)
	}
	defer client.Close()

	// Initialize Bubble Tea program. WithMouseCellMotion enables scroll-wheel
	// support inside viewports (logs, unit file).
	p := tea.NewProgram(tui.NewModel(client), tea.WithAltScreen(), tea.WithMouseCellMotion())

	if _, err := p.Run(); err != nil {
		log.Fatalf("Error running program: %v", err)
	}
}
