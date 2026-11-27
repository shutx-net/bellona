// Package agent is a draft of the AgentRuntime contract of SPEC §11: the port
// through which the engine starts, resumes and cancels one agent execution on
// a host, without knowing which host it is.
//
// The contract is PROVISIONAL. The M0 host capability probe (SPEC §11.3,
// R-RUN-18) records what the first host does, and that evidence may change
// any type, field or method in this package; M1a freezes the contract. The
// draft is deliberately thin. It has only the fields that every adapter must
// provide (session, outcome, output, exit code) and the four capabilities that
// the engine branches on. The optional parts of SPEC §11 (launch receipt,
// refused calls, models and usage, per-Turn limits, customization scope,
// environment names, scope rules, the subagent setting and extra arguments)
// are added in M1a, after the probe.
//
// Points the draft leaves open, to be settled in M1a: how the text of the
// role definition reaches the runtime (Request has only Instructions), the
// meaning of the context passed to Cancel, and the staged cancellation of
// SPEC §11.2.
//
// Host-specific names never appear here. Command-line options, output
// formats and event formats of a host belong to its adapter, which maps them
// to the types of this package. The package imports only internal/domain and
// starts no process; adapters live in packages of their own.
package agent

import (
	"context"
	"time"

	"github.com/shutx-net/bellona/internal/domain"
)

// Runtime starts and resumes agent executions on one host. Whether a process
// is launched per Turn or kept across Turns is the adapter's business (SPEC
// §11.1); the contract sees only executions. An implementation must be safe
// for concurrent use.
//
// The context passed to Start or Resume bounds the execution: once it is
// cancelled the runtime stops the execution and Wait reports the outcome
// interrupted. Staged cancellation (graceful interrupt, termination, kill)
// is done through Execution.Cancel and belongs to M1a.
type Runtime interface {
	// Capabilities reports what the host supports. The core must work when
	// a capability is absent (SPEC §11).
	Capabilities(ctx context.Context) (Capabilities, error)

	// Start begins a new Session. It returns once the execution has been
	// launched, not when it ends; the caller must call Wait on the returned
	// Execution to learn the result and to release the execution. A launch
	// failure is returned as the error, and no Execution exists then.
	Start(ctx context.Context, req Request) (Execution, error)

	// Resume begins a new Turn in an existing Session. Resume only by an
	// explicit session ID, never through the host's choice of the most
	// recent session (SPEC §11.1). The instructions of a resumed Turn travel
	// in req.Instructions, because a host may keep the instructions it
	// recorded when the Session started.
	Resume(ctx context.Context, sessionID domain.SessionID, req Request) (Execution, error)
}

// Execution is one running agent process.
type Execution interface {
	// Events delivers normalized host observations while the execution
	// runs. The channel is closed before Wait returns. Delivery never
	// blocks the runtime, so a consumer may ignore the channel, and a slow
	// consumer may miss observations.
	Events() <-chan Event

	// Wait blocks until the process has been reaped and returns the result.
	// It may be called once. A non-nil error means the runtime could not
	// carry the execution to a host-reported end; the Result then holds only
	// what is known. An agent that ran and failed is not an error: it is a
	// Result whose Outcome is domain.OutcomeFailed.
	Wait() (Result, error)

	// Cancel asks the execution to stop and is idempotent. Wait then returns
	// with the outcome domain.OutcomeInterrupted. Staged cancellation is
	// M1a.
	Cancel(ctx context.Context) error
}

// Capabilities states which optional features the host supports. The engine
// fails configuration validation at Run start when a role setting needs a
// feature that is absent (SPEC §11).
type Capabilities struct {
	// SessionIDBeforeLaunch: the engine can choose the session ID before
	// the launch by setting Request.SessionID.
	SessionIDBeforeLaunch bool
	// Resume: Runtime.Resume works.
	Resume bool
	// StructuredOutput: the host validates the final message against
	// Request.OutputSchema itself.
	StructuredOutput bool
	// GracefulInterrupt: Cancel can first ask the host to end the Turn and
	// record its state.
	GracefulInterrupt bool
}

// Request describes one Turn.
type Request struct {
	// Role is the logical role of the agent.
	Role domain.Role
	// Model is the host's model name, or empty for the host's default.
	Model string
	// Effort is the requested effort level, or empty for the host's default.
	Effort string
	// Instructions is the text the agent receives for this Turn.
	Instructions string
	// Workspace is the working directory of the process.
	Workspace string
	// Inputs are the finalized artifacts that the Turn reads.
	Inputs []domain.ArtifactRef
	// OutputSchema is the JSON Schema, as text, that the final message must
	// satisfy. It is empty when the role has no structured output.
	OutputSchema string
	// SessionID is chosen by the engine before launch, and only when
	// Capabilities.SessionIDBeforeLaunch is true; otherwise it is empty and
	// the host reports the session ID in the Result.
	SessionID domain.SessionID
}

// Result is what the host reported when the process ended.
type Result struct {
	// SessionID is the Session the Turn ran in.
	SessionID domain.SessionID
	// Outcome comes from the host's own result. It is never derived from
	// the exit code alone, because a host can exit successfully after
	// refusing the calls that the task needed (SPEC §11).
	Outcome domain.ExecutionOutcome
	// Output is the agent's final structured output, if any.
	Output []byte
	// ExitCode is the exit status of the process, kept for the record.
	ExitCode int
}

// Event is a normalized host observation. It is distinct from a domain event
// and is never written to the journal in M0.
type Event struct {
	// Kind names the observation in the adapter's host-neutral vocabulary.
	Kind string
	// Time is when the adapter observed it.
	Time time.Time
	// Data is the observation's payload; its format belongs to Kind.
	Data []byte
}
