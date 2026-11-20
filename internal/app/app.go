// SPDX-License-Identifier: GPL-3.0-only
// Copyright (c) 2021 by Rafael Osipov <rafael.osipov@outlook.com>

package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/cdefgah/whatsapp-jpeg-repair/internal/filesystem"
	"github.com/cdefgah/whatsapp-jpeg-repair/internal/options"
	"github.com/cdefgah/whatsapp-jpeg-repair/internal/preflight"
	"github.com/cdefgah/whatsapp-jpeg-repair/internal/repair"
	"github.com/spf13/afero"
)

// Runner encapsulates core params for the application.
type Runner struct {
	fs     afero.Fs
	stdout io.Writer
	stderr io.Writer
	clock  repair.Clock
}

// NewAppRunner create new instance of AppRunner structure.
func NewAppRunner(fs afero.Fs, stdout io.Writer, stderr io.Writer, clock repair.Clock) *Runner {
	return &Runner{
		fs:     fs,
		stdout: stdout,
		stderr: stderr,
		clock:  clock,
	}
}

// cliProcessParams encapsulates processing command line params for the application.
type cliProcessParams struct {
	Stdin              io.Reader
	ExeFolderPath      string
	ArgsWithoutAppName []string
}

// NewGlobalProcessParams creates new instance of GlobalProcessParams structure.
func NewGlobalProcessParams(stdin io.Reader, exeFolderPath string, argsWithoutAppName []string) *cliProcessParams {
	return &cliProcessParams{
		Stdin:              stdin,
		ExeFolderPath:      exeFolderPath,
		ArgsWithoutAppName: argsWithoutAppName,
	}
}

// ProcessCommandLineArguments is entry point to the repair process, handles command line arguments and acts accordingly.
func (r *Runner) ProcessCommandLineArguments(ctx context.Context, params cliProcessParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	managedOptions := options.NewDefaultManagedModeOptions(params.ExeFolderPath)

	flagSet, displayHelp := options.NewManagedFlagSet(r.stderr, managedOptions)
	if err := flagSet.Parse(params.ArgsWithoutAppName); err != nil || *displayHelp {
		flagSet.Usage()
		return nil
	}

	useManagedMode, err := options.IsManagedMode(params.ArgsWithoutAppName, flagSet)
	if err != nil {
		return err
	}

	if useManagedMode {
		// check if non-managed mode arguments present for managed mode
		positionalArgsPresent := len(flagSet.Args()) > 0

		if positionalArgsPresent && !managedOptions.Preflight {
			flagSet.Usage()
			return nil
		}

		if managedOptions.Preflight {
			if positionalArgsPresent {
				// preflight for explicitly passed files, following direct mode rules
				directOptions := options.NewDirectOptions(flagSet.Args())
				return r.runPreflightDirectMode(ctx, directOptions, managedOptions.PreflightJSON)
			}

			managedOptions.SourceFolderPath = filepath.Clean(managedOptions.SourceFolderPath)
			managedOptions.DestinationFolderPath = filepath.Clean(managedOptions.DestinationFolderPath)

			return r.runPreflightManagedMode(ctx, params.Stdin, *managedOptions)
		}

		managedOptions.SourceFolderPath = filepath.Clean(managedOptions.SourceFolderPath)
		managedOptions.DestinationFolderPath = filepath.Clean(managedOptions.DestinationFolderPath)

		return r.runAppInManagedMode(ctx, params.Stdin, *managedOptions)
	}

	directOptions := options.NewDirectOptions(flagSet.Args())
	return r.runAppInDirectMode(ctx, directOptions, r.clock)
}

// runAppInDirectMode runs application in direct mode, repairs files whose paths were specified in the command-line parameters.
func (r *Runner) runAppInDirectMode(ctx context.Context, opts options.DirectModeOptions, clock repair.Clock) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	fmt.Fprintln(r.stderr, "Now the application runs in direct mode, processing file paths that are passed in the command line.")

	imageRepairer := repair.NewImageRepairerForDirectMode(r.fs, opts, r.stderr, clock)
	filePathIterator := filesystem.NewFilePathsIteratorForDirectMode(opts.FilePaths)

	repair.ProcessAllFiles(ctx, filePathIterator, imageRepairer)
	fmt.Fprintln(r.stderr, imageRepairer.TextReport())

	if imageRepairer.HasErrors() {
		return fmt.Errorf("the processing of image files in direct mode has failed")
	}

	return nil
}

// runPreflightDirectMode runs the read-only preflight check for files passed in the command line.
func (r *Runner) runPreflightDirectMode(ctx context.Context, opts options.DirectModeOptions, asJSON bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	fmt.Fprintln(r.stderr, "Running read-only preflight check in direct mode, no files will be changed.")

	planner := preflight.NewPlanner(r.fs, r.clock, preflight.DefaultMaxEntries)
	report := planner.PlanDirect(ctx, opts)

	return r.emitPreflightReport(report, asJSON)
}

// runPreflightManagedMode runs the read-only preflight check according to the managed mode parameters.
func (r *Runner) runPreflightManagedMode(ctx context.Context, stdin io.Reader, opts options.ManagedModeOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	fmt.Fprintln(r.stderr, "Running read-only preflight check in managed mode with the following parameters:")
	fmt.Fprintln(r.stderr, opts.String())

	planner := preflight.NewPlanner(r.fs, r.clock, preflight.DefaultMaxEntries)
	report, err := planner.PlanManaged(ctx, opts)
	if err != nil {
		return err
	}

	reportErr := r.emitPreflightReport(report, opts.PreflightJSON)

	repair.RunAndWaitForExit(ctx, stdin, r.stderr, opts.DontWaitToClose)

	return reportErr
}

// emitPreflightReport prints the report as JSON to stdout or as text to stderr,
// and returns an error if the check found blocking problems.
func (r *Runner) emitPreflightReport(report *preflight.Report, asJSON bool) error {
	if asJSON {
		if err := report.WriteJSON(r.stdout); err != nil {
			return fmt.Errorf("write JSON preflight report: %w", err)
		}
	} else {
		report.WriteText(r.stderr)
	}

	if report.Errors > 0 {
		return fmt.Errorf("preflight check found problems with %d file(s)", report.Errors)
	}

	return nil
}

// runAppInManagedMode runs application in managed mode, according to the parameters passed in the command line.
func (r *Runner) runAppInManagedMode(ctx context.Context, stdin io.Reader, opts options.ManagedModeOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	fmt.Fprintln(r.stderr, "Now the application runs in managed mode with the following parameters:")
	fmt.Fprintln(r.stderr, opts.String())

	filePathIterator, err :=
		filesystem.NewFilePathsIteratorForManagedMode(r.fs,
			opts.SourceFolderPath,
			opts.ProcessNestedFolders)

	if err != nil {
		return err
	}

	imageRepairer := repair.NewImageRepairerForManagedMode(r.fs, opts, r.stderr, r.clock)

	repair.ProcessAllFiles(ctx, filePathIterator, imageRepairer)
	fmt.Fprintln(r.stderr, imageRepairer.TextReport())

	repair.RunAndWaitForExit(ctx, stdin, r.stderr, opts.DontWaitToClose)

	if imageRepairer.HasErrors() {
		return fmt.Errorf("the processing of image files in managed mode has failed")
	}

	return nil
}
