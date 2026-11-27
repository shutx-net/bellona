// Command bellona is the command-line entry point of Bellona. In M0 it has one
// subcommand, version, which prints the versions that a Run manifest records.
// The run, status, resume and further subcommands arrive with M1b and later;
// the core packages under internal/ do no terminal I/O (R-CLI-04), so the
// commands are thin callers of the engine.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/version"
)

const usage = "usage: bellona version"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line args (without the program name) and returns
// the exit status: 0 on success, 1 when the output cannot be written, and 2
// for any invocation other than exactly "version".
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "version" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	_, err := fmt.Fprintf(stdout, "bellona %s\nworkflow %s %d\nevent schema %d\n",
		version.Engine, fsm.WorkflowID, fsm.WorkflowVersion, domain.EventSchemaVersion)
	if err != nil {
		fmt.Fprintln(stderr, "bellona:", err)
		return 1
	}
	return 0
}
