// SPDX-License-Identifier: GPL-3.0-only
// Copyright (c) 2021 by Rafael Osipov <rafael.osipov@outlook.com>

// Package preflight implements a read-only preflight check.
// It lists what a repair run would do (source files, planned output and
// backup paths, file sizes, JPEG decodability, path conflicts and
// permission problems) without creating folders or backups and without
// modifying any file.
package preflight

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cdefgah/whatsapp-jpeg-repair/internal/filesystem"
	"github.com/cdefgah/whatsapp-jpeg-repair/internal/options"
	"github.com/cdefgah/whatsapp-jpeg-repair/internal/repair"
	"github.com/spf13/afero"
)

// DefaultMaxEntries bounds the number of report entries, so that
// a preflight run over a huge directory tree stays within predictable
// memory limits. When the bound is hit, the report is marked as truncated.
const DefaultMaxEntries = 100_000

// Status describes the severity of a single preflight entry.
type Status string

// Possible entry statuses.
const (
	StatusOK      Status = "ok"
	StatusWarning Status = "warning"
	StatusError   Status = "error"
	StatusSkipped Status = "skipped"
)

// Entry describes the preflight result for a single file system object.
type Entry struct {
	Source    string   `json:"source"`
	Output    string   `json:"output,omitempty"`
	Backup    string   `json:"backup,omitempty"`
	SizeBytes int64    `json:"sizeBytes"`
	Decodable bool     `json:"decodable"`
	Status    Status   `json:"status"`
	Issues    []string `json:"issues,omitempty"`
}

// addIssue attaches an issue to the entry and escalates its status.
func (e *Entry) addIssue(status Status, message string) {
	e.Issues = append(e.Issues, message)

	switch status {
	case StatusError:
		e.Status = StatusError
	case StatusWarning:
		if e.Status == StatusOK {
			e.Status = StatusWarning
		}
	}
}

// Report is the full preflight checklist, suitable for both
// human-readable and machine-readable (JSON) output.
type Report struct {
	Mode        string  `json:"mode"` // "direct" or "managed"
	Entries     []Entry `json:"entries"`
	Total       int     `json:"total"`
	OK          int     `json:"ok"`
	Warnings    int     `json:"warnings"`
	Errors      int     `json:"errors"`
	Skipped     int     `json:"skipped"`
	Truncated   bool    `json:"truncated"`
	Interrupted bool    `json:"interrupted"`
}

// finalize computes the summary counters of the report.
func (r *Report) finalize() {
	r.Total = len(r.Entries)
	for _, e := range r.Entries {
		switch e.Status {
		case StatusOK:
			r.OK++
		case StatusWarning:
			r.Warnings++
		case StatusError:
			r.Errors++
		case StatusSkipped:
			r.Skipped++
		}
	}
}

// WriteJSON writes the report as machine-readable JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText writes the report as a human-readable checklist.
func (r *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "Preflight report (%s mode) - read-only check, no files were changed.\n\n", r.Mode)

	for _, e := range r.Entries {
		decodable := "not decodable"
		if e.Decodable {
			decodable = "decodable"
		}

		if e.Output != "" {
			fmt.Fprintf(w, "[%s] %s -> %s (%d bytes, %s)\n",
				strings.ToUpper(string(e.Status)), e.Source, e.Output, e.SizeBytes, decodable)
		} else {
			fmt.Fprintf(w, "[%s] %s (%d bytes, %s)\n",
				strings.ToUpper(string(e.Status)), e.Source, e.SizeBytes, decodable)
		}

		if e.Backup != "" {
			fmt.Fprintf(w, "    backup: %s\n", e.Backup)
		}

		for _, issue := range e.Issues {
			fmt.Fprintf(w, "    - %s\n", issue)
		}
	}

	fmt.Fprintf(w, "\nTotal: %d file(s). OK: %d. Warnings: %d. Errors: %d. Skipped: %d.\n",
		r.Total, r.OK, r.Warnings, r.Errors, r.Skipped)

	if r.Truncated {
		fmt.Fprintln(w, "The list was truncated: too many entries.")
	}

	if r.Interrupted {
		fmt.Fprintln(w, "The preflight check was interrupted before completion.")
	}
}

// Planner builds preflight reports. It never writes to the file system:
// it only stats, lists directories and reads file headers.
type Planner struct {
	fs         afero.Fs
	clock      repair.Clock
	maxEntries int
}

// NewPlanner creates a new Planner. If maxEntries is not positive,
// DefaultMaxEntries is used.
func NewPlanner(fs afero.Fs, clock repair.Clock, maxEntries int) *Planner {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}

	return &Planner{
		fs:         fs,
		clock:      clock,
		maxEntries: maxEntries,
	}
}

// lstat returns file information without following the final symlink,
// falling back to Stat on file systems that do not support Lstat.
func lstat(fsys afero.Fs, path string) (os.FileInfo, error) {
	if lst, ok := fsys.(afero.Lstater); ok {
		info, _, err := lst.LstatIfPossible(path)
		return info, err
	}

	return fsys.Stat(path)
}

// writeForbidden heuristically reports whether writing is denied by permission bits.
// A zero permission mask means "permissions unknown" (for example, files created
// by in-memory file systems), so the check is skipped rather than reporting
// a false positive.
func writeForbidden(mode os.FileMode) bool {
	perm := mode.Perm()
	return perm != 0 && perm&0o222 == 0
}

// readForbidden heuristically reports whether reading is denied by permission bits.
// See writeForbidden for the zero-mask special case.
func readForbidden(mode os.FileMode) bool {
	perm := mode.Perm()
	return perm != 0 && perm&0o444 == 0
}

// checkDecodable opens the file and decodes only the JPEG header,
// which keeps the check cheap even for very large images.
func (p *Planner) checkDecodable(path string) error {
	file, err := p.fs.Open(path)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	if _, err := jpeg.DecodeConfig(bufio.NewReader(file)); err != nil {
		return fmt.Errorf("decode JPEG header: %w", err)
	}

	return nil
}

// nearestExistingAncestor walks up from path until it finds an existing object.
func (p *Planner) nearestExistingAncestor(path string) (os.FileInfo, string, error) {
	current := path
	for {
		info, err := p.fs.Stat(current)
		if err == nil {
			return info, current, nil
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return nil, "", fmt.Errorf("no existing ancestor found for %q", path)
		}
		current = parent
	}
}

// PlanDirect builds a preflight report for direct mode,
// where files are repaired in place.
func (p *Planner) PlanDirect(ctx context.Context, opts options.DirectModeOptions) *Report {
	report := &Report{Mode: "direct", Entries: []Entry{}}
	seen := make(map[string]bool)

	for _, path := range opts.FilePaths {
		if ctx.Err() != nil {
			report.Interrupted = true
			break
		}

		if len(report.Entries) >= p.maxEntries {
			report.Truncated = true
			break
		}

		entry := Entry{Source: path, Output: path, Status: StatusOK}

		if seen[path] {
			entry.addIssue(StatusWarning, "duplicate source path: repair would process this file more than once")
		}
		seen[path] = true

		p.inspectDirectEntry(&entry)
		report.Entries = append(report.Entries, entry)
	}

	sortEntries(report.Entries)
	report.finalize()
	return report
}

// inspectDirectEntry fills size, decodability, planned backup path
// and permission issues for a single direct mode entry.
func (p *Planner) inspectDirectEntry(entry *Entry) {
	path := entry.Source

	info, err := lstat(p.fs, path)
	if err != nil {
		entry.addIssue(StatusError, fmt.Sprintf("cannot access source file: %v", err))
		return
	}

	if info.Mode()&os.ModeSymlink != 0 {
		entry.addIssue(StatusWarning, "source is a symbolic link")
		info, err = p.fs.Stat(path)
		if err != nil {
			entry.addIssue(StatusError, fmt.Sprintf("cannot resolve symbolic link: %v", err))
			return
		}
	}

	if !info.Mode().IsRegular() {
		entry.addIssue(StatusError, "source is not a regular file")
		return
	}

	entry.SizeBytes = info.Size()
	entry.Backup = repair.PlannedBackupPath(path, p.clock.Now())

	if err := p.checkDecodable(path); err != nil {
		entry.addIssue(StatusError, fmt.Sprintf("source is not decodable as JPEG: %v", err))
	} else {
		entry.Decodable = true
	}

	if writeForbidden(info.Mode()) {
		entry.addIssue(StatusError, "source file is not writable, repair cannot overwrite it")
	}

	dirInfo, err := p.fs.Stat(filepath.Dir(path))
	if err != nil {
		entry.addIssue(StatusWarning, fmt.Sprintf("cannot check source folder permissions: %v", err))
	} else if writeForbidden(dirInfo.Mode()) {
		entry.addIssue(StatusError, "source folder is not writable, a backup cannot be created there")
	}
}

// PlanManaged builds a preflight report for managed mode.
// Unlike the repair run it does not create the destination folder.
func (p *Planner) PlanManaged(ctx context.Context, opts options.ManagedModeOptions) (*Report, error) {
	srcBase := filepath.Clean(opts.SourceFolderPath)
	dstBase := filepath.Clean(opts.DestinationFolderPath)

	info, err := p.fs.Stat(srcBase)
	if err != nil {
		return nil, fmt.Errorf("stat source folder: %w", err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("source path is not a directory: %q", srcBase)
	}

	report := &Report{Mode: "managed", Entries: []Entry{}}
	outputs := make(map[string]string) // planned output path -> first source path that claimed it

	// Iterative depth-first walk, mirroring the repair iterator rules,
	// but read-only and reporting symlinks instead of silently skipping them.
	type frame struct {
		dir     string
		entries []os.FileInfo
		index   int
	}

	rootEntries, err := afero.ReadDir(p.fs, srcBase)
	if err != nil {
		return nil, fmt.Errorf("read source folder: %w", err)
	}

	stack := []frame{{dir: srcBase, entries: rootEntries}}

	for len(stack) > 0 {
		if ctx.Err() != nil {
			report.Interrupted = true
			break
		}

		if len(report.Entries) >= p.maxEntries {
			report.Truncated = true
			break
		}

		top := &stack[len(stack)-1]
		if top.index >= len(top.entries) {
			stack = stack[:len(stack)-1]
			continue
		}

		item := top.entries[top.index]
		top.index++
		fullPath := filepath.Join(top.dir, item.Name())

		if item.Mode()&os.ModeSymlink != 0 {
			if filesystem.IsJpegFileExtension(fullPath) {
				report.Entries = append(report.Entries, Entry{
					Source: fullPath,
					Status: StatusSkipped,
					Issues: []string{"symbolic link: skipped by repair"},
				})
			}
			continue
		}

		if item.IsDir() {
			if opts.ProcessNestedFolders {
				subEntries, err := afero.ReadDir(p.fs, fullPath)
				if err != nil {
					report.Entries = append(report.Entries, Entry{
						Source: fullPath,
						Status: StatusSkipped,
						Issues: []string{fmt.Sprintf("folder cannot be read: %v", err)},
					})
					continue
				}
				stack = append(stack, frame{dir: fullPath, entries: subEntries})
			}
			continue
		}

		if !item.Mode().IsRegular() || !filesystem.IsJpegFileExtension(fullPath) {
			continue
		}

		entry := Entry{Source: fullPath, Status: StatusOK, SizeBytes: item.Size()}

		relPath, err := filepath.Rel(srcBase, filepath.Dir(fullPath))
		if err != nil || strings.HasPrefix(relPath, "..") {
			entry.addIssue(StatusError, "source file is outside of the source folder")
		} else {
			entry.Output = filepath.Join(dstBase, relPath, filepath.Base(fullPath))
		}

		if err := p.checkDecodable(fullPath); err != nil {
			entry.addIssue(StatusError, fmt.Sprintf("source is not decodable as JPEG: %v", err))
		} else {
			entry.Decodable = true
		}

		if readForbidden(item.Mode()) {
			entry.addIssue(StatusError, "source file is not readable")
		}

		if entry.Output != "" {
			p.inspectManagedOutput(&entry, outputs)
		}

		report.Entries = append(report.Entries, entry)
	}

	sortEntries(report.Entries)
	report.finalize()
	return report, nil
}


// inspectManagedOutput checks the planned output path for conflicts,
// overwrites and permission problems.
func (p *Planner) inspectManagedOutput(entry *Entry, outputs map[string]string) {
	out := entry.Output

	if firstSource, ok := outputs[out]; ok {
		entry.addIssue(StatusError, fmt.Sprintf("output path conflicts with another source file: %s", firstSource))
	} else {
		outputs[out] = entry.Source
	}

	if out == entry.Source {
		entry.addIssue(StatusWarning, "output path is the same as the source path, the file will be overwritten in place")
	}

	switch outInfo, err := p.fs.Stat(out); {
	case err == nil:
		if outInfo.IsDir() {
			entry.addIssue(StatusError, "output path is an existing directory")
		} else {
			entry.Backup = repair.PlannedBackupPath(out, p.clock.Now())
			entry.addIssue(StatusWarning, "output file exists and will be overwritten (a backup will be created)")
			if writeForbidden(outInfo.Mode()) {
				entry.addIssue(StatusError, "existing output file is not writable")
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		entry.addIssue(StatusWarning, fmt.Sprintf("cannot check output path: %v", err))
	}

	ancestorInfo, ancestorPath, err := p.nearestExistingAncestor(filepath.Dir(out))
	if err != nil {
		entry.addIssue(StatusWarning, fmt.Sprintf("cannot check destination folder: %v", err))
		return
	}

	if !ancestorInfo.IsDir() {
		entry.addIssue(StatusError, fmt.Sprintf("destination path is blocked by non-directory: %s", ancestorPath))
		return
	}

	if writeForbidden(ancestorInfo.Mode()) {
		entry.addIssue(StatusError, fmt.Sprintf("destination folder is not writable: %s", ancestorPath))
	}
}

// sortEntries sorts entries by source path, keeping a stable order
// for entries with the same source (for example, duplicates).
func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Source < entries[j].Source
	})
}
