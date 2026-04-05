package slogor

import (
	"os"

	"golang.org/x/sys/windows"
)

// init initializes the Windows console mode to add colors support to it.
func init() {
	// Get the file descriptor for the standard output (stdout).
	stdout := windows.Handle(os.Stdout.Fd())

	// Declare a variable to store the original console mode.
	var currentMode uint32

	// Retrieve the current console mode for the standard output.
	// The retrieved mode will be stored in the currentMode variable.
	windows.GetConsoleMode(stdout, &currentMode)

	// Calculate the new console mode by combining the original mode with various
	// flags to enhance the terminal's capabilities for better logging.
	// Here, ENABLE_PROCESSED_OUTPUT ensures that the output is processed before being written to the console.
	// ENABLE_WRAP_AT_EOL_OUTPUT enables automatic wrapping at the end of the line.
	// ENABLE_VIRTUAL_TERMINAL_PROCESSING enables processing of virtual terminal sequences for colors and formatting.
	// More information about console mode flags can be found at: https://learn.microsoft.com/en-us/windows/console/setconsolemode
	newMode := currentMode | windows.ENABLE_PROCESSED_OUTPUT |
		windows.ENABLE_WRAP_AT_EOL_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING

	// Set the new console mode for the standard output.
	windows.SetConsoleMode(stdout, newMode)
}
