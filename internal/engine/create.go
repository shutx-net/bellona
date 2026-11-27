package engine

import (
	"context"
	"fmt"

	"github.com/shutx-net/bellona/internal/artifact"
	"github.com/shutx-net/bellona/internal/config"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/manifest"
	"github.com/shutx-net/bellona/internal/projection"
	"github.com/shutx-net/bellona/internal/storage"
	"github.com/shutx-net/bellona/internal/strictjson"
	"github.com/shutx-net/bellona/roles"
)

// CreateParams is the input of Create.
type CreateParams struct {
	// Request is the user's request, stored as task.json.
	Request string
	// Config is the effective configuration, the result of config.Resolve. It
	// is frozen as policy.json.
	Config config.Config
	// Roles are the role definitions to freeze, one per role in the order of
	// domain.Roles. Nil means the embedded definitions, roles.Definitions().
	Roles []domain.RoleDefinition
}

// Create makes a new Run: it validates the input, creates the Run directory
// under the state root, takes the Run lock, and commits RunCreated and
// ManifestFrozen. The second event freezes the effective configuration (as
// policy.json), the role definitions (as copies under definitions/roles/) and
// the contract versions (in manifest.json) by content (R-MAN-01, invariant
// 12): from then on the Run uses those copies, whatever the configuration and
// the embedded definitions become. The Run is in Created when Create returns.
//
// Invalid input is refused before anything is written. When a commit fails
// after the Run directory exists, Create releases the lock and returns the
// error; the partial Run directory stays in place, since nothing is deleted
// automatically, and a later Open reports ErrIncompleteRun for it.
//
// ctx is checked once, before anything is written: a context that is already
// done makes Create fail and creates nothing. From the first write on, the
// cancellation of ctx is ignored, because Create is short and a Run that has
// RunCreated but not ManifestFrozen is an incomplete Run. So a cancellation
// that arrives later cannot make Create return an error for a Run whose two
// initial events were committed.
func (e *Engine) Create(ctx context.Context, p CreateParams) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	task := artifact.Task{SchemaVersion: artifact.TaskSchemaVersion, Request: p.Request}
	if err := task.Validate(); err != nil {
		return nil, fmt.Errorf("engine: the request is invalid: %w", err)
	}
	if err := p.Config.ValidateEffective(); err != nil {
		return nil, err
	}
	defs := p.Roles
	if defs == nil {
		defs = roles.Definitions()
	}
	if err := domain.ValidateRoleDefinitions(defs); err != nil {
		return nil, err
	}
	limits, err := p.Config.Limits()
	if err != nil {
		return nil, err
	}
	id, err := domain.NewRunID(e.rand)
	if err != nil {
		return nil, fmt.Errorf("engine: draw a Run ID: %w", err)
	}
	var versions manifest.Versions // the zero value means manifest.Current()
	if e.recordVersions != nil {
		versions = *e.recordVersions
	}
	frozen, err := manifest.Build(manifest.BuildInput{RunID: id, Effective: p.Config, Roles: defs, Versions: versions})
	if err != nil {
		return nil, err
	}
	taskData, err := strictjson.EncodeIndent(task)
	if err != nil {
		return nil, fmt.Errorf("engine: encode the task: %w", err)
	}
	taskRef, err := domain.NewArtifactRef(domain.TaskPath, taskData)
	if err != nil {
		return nil, err
	}
	manifestFile := frozen.Files[len(frozen.Files)-1] // manifest.json comes last
	manifestRef, err := domain.NewArtifactRef(manifestFile.Path, manifestFile.Data)
	if err != nil {
		return nil, err
	}
	files := make([]newFile, len(frozen.Files))
	for i, f := range frozen.Files {
		files[i] = newFile{Path: f.Path, Data: f.Data}
	}

	// The first write follows. Past it, a cancellation of the caller must not stop
	// the two initial commits halfway: run.call returns ctx.Err() as soon as ctx
	// is done, even for a request that the control loop goes on to commit.
	ctx = context.WithoutCancel(ctx)
	store, err := storage.CreateRun(storage.RunDir(e.stateRoot, id), storage.Options{Faults: e.faults})
	if err != nil {
		return nil, err
	}
	run := e.startRun(id, store, projection.State{})
	if _, err := run.propose(ctx, domain.RunCreated{Task: taskRef}, newFile{Path: domain.TaskPath, Data: taskData}); err != nil {
		return nil, e.abandonCreate(run, err)
	}
	if _, err := run.propose(ctx, domain.ManifestFrozen{Manifest: manifestRef, Policy: frozen.Manifest.Policy, Limits: limits}, files...); err != nil {
		return nil, e.abandonCreate(run, err)
	}
	return run, nil
}

// abandonCreate releases the lock of a Run whose creation failed and returns
// the failure.
func (e *Engine) abandonCreate(run *Run, cause error) error {
	if err := run.Close(context.Background()); err != nil {
		e.logger.Error("releasing a Run whose creation failed", "run", string(run.ID()), "error", err)
	}
	return cause
}
