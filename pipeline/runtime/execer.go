// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

package runtime

import (
	"context"
	"sync"

	"github.com/drone/drone-go/drone"
	"github.com/drone/runner-go/environ"
	"github.com/drone/runner-go/livelog/extractor"
	"github.com/drone/runner-go/logger"
	"github.com/drone/runner-go/pipeline"

	"github.com/hashicorp/go-multierror"
	"github.com/natessilva/dag"
	"golang.org/x/sync/semaphore"
)

// Execer executes the pipeline.
type Execer struct {
	mu       sync.Mutex
	engine   Engine
	reporter pipeline.Reporter
	streamer pipeline.Streamer
	uploader pipeline.Uploader
	sem      *semaphore.Weighted
}

// NewExecer returns a new execer.
func NewExecer(
	reporter pipeline.Reporter,
	streamer pipeline.Streamer,
	uploader pipeline.Uploader,
	engine Engine,
	threads int64,
) *Execer {
	exec := &Execer{
		reporter: reporter,
		streamer: streamer,
		engine:   engine,
		uploader: uploader,
	}
	if threads > 0 {
		// optional semaphore that limits the number of steps
		// that can execute concurrently.
		exec.sem = semaphore.NewWeighted(threads)
	}
	return exec
}

// Exec executes the intermediate representation of the pipeline
// and returns an error if execution fails.
func (e *Execer) Exec(ctx context.Context, spec Spec, state *pipeline.State) error {
	log := logger.FromContext(ctx)

	defer func() {
		log.Debugln("destroying the pipeline environment")
		err := e.engine.Destroy(noContext, spec)
		if err != nil {
			log.WithError(err).
				Debugln("cannot destroy the pipeline environment")
		} else {
			log.Debugln("successfully destroyed the pipeline environment")
		}
	}()

	if err := e.engine.Setup(noContext, spec); err != nil {
		state.FailAll(err)
		return e.reporter.ReportStage(noContext, state)
	}

	// create a new context with cancel in order to
	// support fail failure when a step fails.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// create a directed graph, where each vertex in the graph
	// is a pipeline step.
	var d dag.Runner
	for i := 0; i < spec.StepLen(); i++ {
		step := spec.StepAt(i)
		d.AddVertex(step.GetName(), func() error {
			err := e.exec(ctx, state, spec, step)
			// if the step is configured to fast fail the
			// pipeline, and if the step returned a non-zero
			// exit code, cancel the entire pipeline.
			if step.GetErrPolicy() == ErrFailFast {
				step := state.Find(step.GetName())
				// reading data from the step is not thread
				// safe so we need to acquire a lock.
				state.Lock()
				exit := step.ExitCode
				state.Unlock()
				if exit > 0 {
					cancel()
				}
			}
			return err
		})
	}

	// create the vertex edges from the values configured in the
	// depends_on attribute.
	for i := 0; i < spec.StepLen(); i++ {
		step := spec.StepAt(i)
		for _, dep := range step.GetDependencies() {
			d.AddEdge(dep, step.GetName())
		}
	}

	var result error
	if err := d.Run(); err != nil {
		switch err.Error() {
		case "missing vertext":
			log.Error(err)
		case "dependency cycle detected":
			log.Error(err)
		}
		result = multierror.Append(result, err)

		// if the pipeline is not in a failing state,
		// returning an unexpected error must place the
		// pipeline in a failing state.
		if !state.Failed() {
			state.FailAll(err)
		}
	}

	// once pipeline execution completes, notify the state
	// manager that all steps are finished.
	state.FinishAll()
	if err := e.reporter.ReportStage(noContext, state); err != nil {
		result = multierror.Append(result, err)
	}
	return result
}

func (e *Execer) exec(ctx context.Context, state *pipeline.State, spec Spec, step Step) error {
	var result error

	select {
	case <-ctx.Done():
		state.Cancel()
		return nil
	default:
	}

	log := logger.FromContext(ctx)
	log = log.WithField("step.name", step.GetName())
	ctx = logger.WithContext(ctx, log)

	if e.sem != nil {
		log.Trace("acquiring semaphore")

		// the semaphore limits the number of steps that can run
		// concurrently. acquire the semaphore and release when
		// the pipeline completes.
		err := e.sem.Acquire(ctx, 1)

		// if acquiring the semaphore failed because the context
		// deadline exceeded (e.g. the pipeline timed out) the
		// state should be canceled.
		switch ctx.Err() {
		case context.Canceled, context.DeadlineExceeded:
			log.Trace("acquiring semaphore canceled")
			state.Cancel()
			return nil
		}

		// if acquiring the semaphore failed for unexpected reasons
		// the pipeline should error.
		if err != nil {
			log.WithError(err).Errorln("failed to acquire semaphore.")
			return err
		}

		defer func() {
			// recover from a panic to ensure the semaphore is
			// released to prevent deadlock. we do not expect a
			// panic, however, we are being overly cautious.
			if r := recover(); r != nil {
				// TODO(bradrydzewski) log the panic.
			}
			// release the semaphore
			e.sem.Release(1)
			log.Trace("semaphore released")
		}()
	}

	switch {
	case state.Cancelled():
		// skip if the pipeline was cancelled, either by the
		// end user or due to timeout.
		return nil
	case step.GetRunPolicy() == RunNever:
		return nil
	case step.GetRunPolicy() == RunAlways:
		break
	case step.GetRunPolicy() == RunOnFailure && state.Failed() == false:
		state.Skip(step.GetName())
		return e.reporter.ReportStep(noContext, state, step.GetName())
	case step.GetRunPolicy() == RunOnSuccess && state.Failed():
		state.Skip(step.GetName())
		return e.reporter.ReportStep(noContext, state, step.GetName())
	case state.Finished(step.GetName()):
		// skip if the step if already in a finished state,
		// for example, if the step is marked as skipped.
		return nil
	}

	state.Start(step.GetName())
	err := e.reporter.ReportStep(noContext, state, step.GetName())
	if err != nil {
		return err
	}

	copy := step.Clone()

	// the pipeline environment variables need to be updated to
	// reflect the current state of the build and stage.
	state.Lock()
	copy.SetEnviron(
		environ.Combine(
			copy.GetEnviron(),
			environ.Build(state.Build),
			environ.Stage(state.Stage),
			environ.Step(findStep(state, step.GetName())),
		),
	)
	state.Unlock()

	// writer used to stream build logs.
	wc := e.streamer.Stream(noContext, state, step.GetName())
	wc = newReplacer(wc, secretSlice(step))

	// wrap writer in extrator
	ext := extractor.New(wc)

	// if the step is configured as a daemon, it is detached
	// from the main process and executed separately.
	if step.IsDetached() {
		go func() {
			e.engine.Run(ctx, spec, copy, ext)
			wc.Close()
		}()
		return nil
	}

	exited, err := e.engine.Run(ctx, spec, copy, ext)

	// close the stream. If the session is a remote session, the
	// full log buffer is uploaded to the remote server.
	if err := wc.Close(); err != nil {
		result = multierror.Append(result, err)
	}

	// upload card if exists
	card, ok := ext.File()
	if ok {
		err = e.uploader.UploadCard(ctx, card, state, step.GetName())
		if err != nil {
			log.Warnln("cannot upload card")
		}
	}

	// if the context was cancelled and returns a Canceled or
	// DeadlineExceeded error this indicates the pipeline was
	// cancelled.
	switch ctx.Err() {
	case context.Canceled, context.DeadlineExceeded:
		state.Cancel()
		return nil
	}

	if exited != nil {
		if exited.OOMKilled {
			log.Debugln("received oom kill.")
			state.Finish(step.GetName(), 137)
		} else {
			log.Debugf("received exit code %d", exited.ExitCode)
			state.Finish(step.GetName(), exited.ExitCode)
		}
		err := e.reporter.ReportStep(noContext, state, step.GetName())
		if err != nil {
			log.Warnln("cannot report step status.")
			result = multierror.Append(result, err)
		}
		// if the exit code is 78 the system will skip all
		// subsequent pending steps in the pipeline.
		if exited.ExitCode == 78 {
			log.Debugln("received exit code 78. early exit.")
			state.SkipAll()
		}
		return result
	}

	switch err {
	case context.Canceled, context.DeadlineExceeded:
		state.Cancel()
		return nil
	}

	// if the step failed with an internal error (as opposed to a
	// runtime error) the step is failed.
	state.Fail(step.GetName(), err)
	err = e.reporter.ReportStep(noContext, state, step.GetName())
	if err != nil {
		log.Warnln("cannot report step failure.")
		result = multierror.Append(result, err)
	}
	return result
}

// helper function returns the named step from the state.
func findStep(state *pipeline.State, name string) *drone.Step {
	for _, step := range state.Stage.Steps {
		if step.Name == name {
			return step
		}
	}
	panic("step not found: " + name)
}

// helper function returns an array of secrets from the
// pipeline step.
func secretSlice(step Step) []Secret {
	var secrets []Secret
	for i := 0; i < step.GetSecretLen(); i++ {
		secrets = append(secrets, step.GetSecretAt(i))
	}
	return secrets
}
