// SPDX-License-Identifier: GPL-3.0-only
// Copyright (c) 2021 by Rafael Osipov <rafael.osipov@outlook.com>

package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cdefgah/whatsapp-jpeg-repair/internal/filesystem"
	"github.com/cdefgah/whatsapp-jpeg-repair/internal/options"
	"github.com/cdefgah/whatsapp-jpeg-repair/internal/testutil"
	"github.com/spf13/afero"
)

var testClock = &testutil.MockClock{FixedTime: time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)}

// snapshotTree returns a map of all paths under root with their sizes,
// used to prove that the preflight check does not write anything.
func snapshotTree(t *testing.T, fs afero.Fs, root string) map[string]int64 {
	t.Helper()

	snapshot := make(map[string]int64)
	err := afero.Walk(fs, root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		snapshot[path] = info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotTree() walk failed: %v", err)
	}

	return snapshot
}

func assertTreeUnchanged(t *testing.T, fs afero.Fs, root string, before map[string]int64) {
	t.Helper()

	after := snapshotTree(t, fs, root)
	if len(after) != len(before) {
		t.Fatalf("file tree changed: had %d entries before, %d after.\nBefore: %v\nAfter: %v",
			len(before), len(after), before, after)
	}

	for path, size := range before {
		if after[path] != size {
			t.Errorf("file %q changed size: %d -> %d", path, size, after[path])
		}
	}
}

func TestPlanManaged_BasicSortedAndZeroWrites(t *testing.T) {
	fs := afero.NewMemMapFs()

	_ = fs.MkdirAll("/src/sub", filesystem.DefaultFolderPermissions)
	_ = testutil.CreateJpegFile(fs, "/src/b.jpg")
	_ = testutil.CreateJpegFile(fs, "/src/a.jpeg")
	_ = afero.WriteFile(fs, "/src/c.jpg", []byte("this is not a jpeg"), filesystem.DefaultFilePermissions)
	_ = afero.WriteFile(fs, "/src/notes.txt", []byte("not an image, must be ignored"), filesystem.DefaultFilePermissions)
	_ = testutil.CreateJpegFile(fs, "/src/sub/d.jpg")

	before := snapshotTree(t, fs, "/")

	planner := NewPlanner(fs, testClock, 0) // 0 -> DefaultMaxEntries

	// Non-recursive first: sub/d.jpg must not appear.
	report, err := planner.PlanManaged(context.Background(), options.ManagedModeOptions{
		SourceFolderPath:      "/src",
		DestinationFolderPath: "/dst",
	})
	if err != nil {
		t.Fatalf("PlanManaged() failed: %v", err)
	}

	if report.Mode != "managed" {
		t.Errorf("Mode = %q, want %q", report.Mode, "managed")
	}

	wantSources := []string{"/src/a.jpeg", "/src/b.jpg", "/src/c.jpg"}
	if len(report.Entries) != len(wantSources) {
		t.Fatalf("got %d entries, want %d: %+v", len(report.Entries), len(wantSources), report.Entries)
	}

	for i, want := range wantSources {
		if report.Entries[i].Source != want {
			t.Errorf("entry %d: Source = %q, want %q (entries must be sorted)", i, report.Entries[i].Source, want)
		}
	}

	good := report.Entries[0]
	if !good.Decodable || good.Status != StatusOK {
		t.Errorf("good jpeg: Decodable = %v, Status = %q, want true/%q", good.Decodable, good.Status, StatusOK)
	}
	if good.Output != "/dst/a.jpeg" {
		t.Errorf("Output = %q, want %q", good.Output, "/dst/a.jpeg")
	}
	if good.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want > 0", good.SizeBytes)
	}

	bad := report.Entries[2]
	if bad.Decodable || bad.Status != StatusError {
		t.Errorf("bad jpeg: Decodable = %v, Status = %q, want false/%q", bad.Decodable, bad.Status, StatusError)
	}
	if report.Errors != 1 {
		t.Errorf("Errors = %d, want 1", report.Errors)
	}

	// Recursive: sub/d.jpg must appear.
	reportRec, err := planner.PlanManaged(context.Background(), options.ManagedModeOptions{
		SourceFolderPath:      "/src",
		DestinationFolderPath: "/dst",
		ProcessNestedFolders:  true,
	})
	if err != nil {
		t.Fatalf("PlanManaged() recursive failed: %v", err)
	}
	if len(reportRec.Entries) != 4 {
		t.Fatalf("recursive: got %d entries, want 4", len(reportRec.Entries))
	}
	if reportRec.Entries[3].Source != "/src/sub/d.jpg" || reportRec.Entries[3].Output != "/dst/sub/d.jpg" {
		t.Errorf("nested entry = %+v, want source /src/sub/d.jpg and output /dst/sub/d.jpg", reportRec.Entries[3])
	}

	// Zero writes: no destination folder, no backups, no modified files.
	if exists, _ := afero.Exists(fs, "/dst"); exists {
		t.Error("preflight created the destination folder, it must be read-only")
	}
	assertTreeUnchanged(t, fs, "/", before)
}

func TestPlanManaged_OutputExistsAndUnwritableDest(t *testing.T) {
	fs := afero.NewMemMapFs()

	_ = fs.MkdirAll("/src", filesystem.DefaultFolderPermissions)
	_ = testutil.CreateJpegFile(fs, "/src/a.jpg")
	_ = testutil.CreateJpegFile(fs, "/src/b.jpg")

	// Existing output for a.jpg -> overwrite warning plus planned backup.
	_ = fs.MkdirAll("/dst", filesystem.DefaultFolderPermissions)
	_ = testutil.CreateJpegFile(fs, "/dst/a.jpg")

	// Destination folder is not writable.
	if err := fs.Chmod("/dst", 0o555); err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}

	planner := NewPlanner(fs, testClock, 100)
	report, err := planner.PlanManaged(context.Background(), options.ManagedModeOptions{
		SourceFolderPath:      "/src",
		DestinationFolderPath: "/dst",
	})
	if err != nil {
		t.Fatalf("PlanManaged() failed: %v", err)
	}

	if len(report.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(report.Entries))
	}

	entryA := report.Entries[0]
	if entryA.Backup == "" || !strings.Contains(entryA.Backup, "_backup") {
		t.Errorf("existing output: Backup = %q, want a planned backup path", entryA.Backup)
	}

	joined := strings.Join(entryA.Issues, "\n")
	if !strings.Contains(joined, "will be overwritten") {
		t.Errorf("existing output: expected overwrite warning, got issues: %v", entryA.Issues)
	}
	if !strings.Contains(joined, "not writable") {
		t.Errorf("unwritable destination: expected permission issue, got issues: %v", entryA.Issues)
	}
	if entryA.Status != StatusError {
		t.Errorf("Status = %q, want %q (unwritable destination is blocking)", entryA.Status, StatusError)
	}

	entryB := report.Entries[1]
	joinedB := strings.Join(entryB.Issues, "\n")
	if !strings.Contains(joinedB, "not writable") {
		t.Errorf("unwritable destination: expected permission issue for b.jpg too, got: %v", entryB.Issues)
	}
}

func TestPlanManaged_SymlinkAndZeroWritesOnRealFS(t *testing.T) {
	fs := afero.NewOsFs()
	root := t.TempDir()

	srcDir := filepath.Join(root, "src")
	dstDir := filepath.Join(root, "dst")
	if err := os.MkdirAll(srcDir, filesystem.DefaultFolderPermissions); err != nil {
		t.Fatal(err)
	}

	if err := testutil.CreateJpegFile(fs, filepath.Join(srcDir, "real.jpg")); err != nil {
		t.Fatal(err)
	}

	linkPath := filepath.Join(srcDir, "link.jpg")
	if err := os.Symlink(filepath.Join(srcDir, "real.jpg"), linkPath); err != nil {
		t.Fatalf("cannot create symlink: %v", err)
	}

	before := snapshotTree(t, fs, root)

	planner := NewPlanner(fs, testClock, 100)
	report, err := planner.PlanManaged(context.Background(), options.ManagedModeOptions{
		SourceFolderPath:      srcDir,
		DestinationFolderPath: dstDir,
	})
	if err != nil {
		t.Fatalf("PlanManaged() failed: %v", err)
	}

	if len(report.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(report.Entries), report.Entries)
	}

	// Sorted: link.jpg before real.jpg.
	linkEntry := report.Entries[0]
	if linkEntry.Source != linkPath || linkEntry.Status != StatusSkipped {
		t.Errorf("symlink entry = %+v, want source %q with status %q", linkEntry, linkPath, StatusSkipped)
	}
	if report.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}

	realEntry := report.Entries[1]
	if realEntry.Status != StatusOK || !realEntry.Decodable {
		t.Errorf("real file entry = %+v, want ok and decodable", realEntry)
	}

	// Zero writes on the real file system.
	if _, err := os.Stat(dstDir); !os.IsNotExist(err) {
		t.Error("preflight created the destination folder, it must be read-only")
	}
	assertTreeUnchanged(t, fs, root, before)
}

func TestPlanDirect_DuplicatesMissingBadAndReadOnly(t *testing.T) {
	fs := afero.NewMemMapFs()

	_ = testutil.CreateJpegFile(fs, "/photo.jpg")
	_ = afero.WriteFile(fs, "/bad.jpg", []byte("not a jpeg"), filesystem.DefaultFilePermissions)
	_ = testutil.CreateJpegFile(fs, "/locked.jpg")
	if err := fs.Chmod("/locked.jpg", 0o444); err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}

	before := snapshotTree(t, fs, "/")

	planner := NewPlanner(fs, testClock, 100)
	report := planner.PlanDirect(context.Background(), options.DirectModeOptions{
		FilePaths: []string{"/photo.jpg", "/photo.jpg", "/missing.jpg", "/bad.jpg", "/locked.jpg"},
	})

	if report.Mode != "direct" {
		t.Errorf("Mode = %q, want %q", report.Mode, "direct")
	}

	if len(report.Entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(report.Entries))
	}

	bySource := make(map[string][]Entry)
	for _, e := range report.Entries {
		bySource[e.Source] = append(bySource[e.Source], e)
	}

	// Sorted output: /bad.jpg, /locked.jpg, /missing.jpg, /photo.jpg, /photo.jpg
	if report.Entries[0].Source != "/bad.jpg" || report.Entries[3].Source != "/photo.jpg" {
		t.Errorf("entries are not sorted by source path: %v", report.Entries)
	}

	dup := bySource["/photo.jpg"]
	if len(dup) != 2 {
		t.Fatalf("duplicate source: got %d entries, want 2", len(dup))
	}
	if dup[0].Status != StatusOK || dup[1].Status != StatusWarning {
		t.Errorf("duplicates: statuses = %q, %q; want %q, %q",
			dup[0].Status, dup[1].Status, StatusOK, StatusWarning)
	}
	if !strings.Contains(strings.Join(dup[1].Issues, "\n"), "duplicate") {
		t.Errorf("duplicate entry must mention the duplication, got: %v", dup[1].Issues)
	}
	if dup[0].Output != "/photo.jpg" {
		t.Errorf("direct mode Output = %q, want the source path itself", dup[0].Output)
	}
	if !strings.Contains(dup[0].Backup, "_backup") {
		t.Errorf("direct mode Backup = %q, want a planned backup path", dup[0].Backup)
	}

	missing := bySource["/missing.jpg"]
	if len(missing) != 1 || missing[0].Status != StatusError {
		t.Errorf("missing file: %+v, want one error entry", missing)
	}

	bad := bySource["/bad.jpg"]
	if len(bad) != 1 || bad[0].Decodable || bad[0].Status != StatusError {
		t.Errorf("bad jpeg: %+v, want one error entry, not decodable", bad)
	}

	locked := bySource["/locked.jpg"]
	if len(locked) != 1 || locked[0].Status != StatusError {
		t.Errorf("read-only file: %+v, want one error entry", locked)
	}
	if !strings.Contains(strings.Join(locked[0].Issues, "\n"), "not writable") {
		t.Errorf("read-only file must report a permission issue, got: %v", locked[0].Issues)
	}

	// Zero writes: no backup files, no modified files.
	assertTreeUnchanged(t, fs, "/", before)
}

func TestPlanDirect_SymlinkOnRealFS(t *testing.T) {
	fs := afero.NewOsFs()
	root := t.TempDir()

	realPath := filepath.Join(root, "real.jpg")
	if err := testutil.CreateJpegFile(fs, realPath); err != nil {
		t.Fatal(err)
	}

	linkPath := filepath.Join(root, "link.jpg")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("cannot create symlink: %v", err)
	}

	planner := NewPlanner(fs, testClock, 100)
	report := planner.PlanDirect(context.Background(), options.DirectModeOptions{
		FilePaths: []string{linkPath},
	})

	if len(report.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(report.Entries))
	}

	entry := report.Entries[0]
	if entry.Status != StatusWarning {
		t.Errorf("Status = %q, want %q for a symlink", entry.Status, StatusWarning)
	}
	if !entry.Decodable {
		t.Error("symlink to a valid jpeg must be decodable (repair follows links)")
	}
	if !strings.Contains(strings.Join(entry.Issues, "\n"), "symbolic link") {
		t.Errorf("symlink entry must mention the link, got: %v", entry.Issues)
	}
}

func TestPlanManaged_TruncationBound(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = fs.MkdirAll("/src", filesystem.DefaultFolderPermissions)

	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		_ = testutil.CreateJpegFile(fs, filepath.Join("/src", name))
	}

	planner := NewPlanner(fs, testClock, 2)
	report, err := planner.PlanManaged(context.Background(), options.ManagedModeOptions{
		SourceFolderPath:      "/src",
		DestinationFolderPath: "/dst",
	})
	if err != nil {
		t.Fatalf("PlanManaged() failed: %v", err)
	}

	if !report.Truncated {
		t.Error("Truncated = false, want true when the entry bound is hit")
	}
	if len(report.Entries) != 2 {
		t.Errorf("got %d entries, want the bound of 2", len(report.Entries))
	}
}

func TestPlan_Cancellation(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = fs.MkdirAll("/src", filesystem.DefaultFolderPermissions)
	_ = testutil.CreateJpegFile(fs, "/src/a.jpg")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	planner := NewPlanner(fs, testClock, 100)

	managedReport, err := planner.PlanManaged(ctx, options.ManagedModeOptions{
		SourceFolderPath:      "/src",
		DestinationFolderPath: "/dst",
	})
	if err != nil {
		t.Fatalf("PlanManaged() failed: %v", err)
	}
	if !managedReport.Interrupted {
		t.Error("managed: Interrupted = false, want true on a cancelled context")
	}
	if len(managedReport.Entries) != 0 {
		t.Errorf("managed: got %d entries, want 0 after immediate cancellation", len(managedReport.Entries))
	}

	directReport := planner.PlanDirect(ctx, options.DirectModeOptions{FilePaths: []string{"/src/a.jpg"}})
	if !directReport.Interrupted {
		t.Error("direct: Interrupted = false, want true on a cancelled context")
	}
	if len(directReport.Entries) != 0 {
		t.Errorf("direct: got %d entries, want 0 after immediate cancellation", len(directReport.Entries))
	}
}

func TestReport_WriteJSON(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = fs.MkdirAll("/src", filesystem.DefaultFolderPermissions)
	_ = testutil.CreateJpegFile(fs, "/src/a.jpg")
	_ = afero.WriteFile(fs, "/src/b.jpg", []byte("bad"), filesystem.DefaultFilePermissions)

	planner := NewPlanner(fs, testClock, 100)
	report, err := planner.PlanManaged(context.Background(), options.ManagedModeOptions{
		SourceFolderPath:      "/src",
		DestinationFolderPath: "/dst",
	})
	if err != nil {
		t.Fatalf("PlanManaged() failed: %v", err)
	}

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON() failed: %v", err)
	}

	var decoded struct {
		Mode    string `json:"mode"`
		Total   int    `json:"total"`
		Errors  int    `json:"errors"`
		Entries []struct {
			Source    string   `json:"source"`
			Output    string   `json:"output"`
			SizeBytes int64    `json:"sizeBytes"`
			Decodable bool     `json:"decodable"`
			Status    string   `json:"status"`
			Issues    []string `json:"issues"`
		} `json:"entries"`
	}

	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, buf.String())
	}

	if decoded.Mode != "managed" || decoded.Total != 2 || decoded.Errors != 1 {
		t.Errorf("unexpected summary: mode=%q total=%d errors=%d", decoded.Mode, decoded.Total, decoded.Errors)
	}
	if len(decoded.Entries) != 2 || decoded.Entries[0].Source != "/src/a.jpg" {
		t.Errorf("unexpected entries: %+v", decoded.Entries)
	}
	if decoded.Entries[0].Output != "/dst/a.jpg" {
		t.Errorf("entry output = %q, want /dst/a.jpg", decoded.Entries[0].Output)
	}
}

func TestReport_WriteText(t *testing.T) {
	fs := afero.NewMemMapFs()
	_ = testutil.CreateJpegFile(fs, "/photo.jpg")

	planner := NewPlanner(fs, testClock, 100)
	report := planner.PlanDirect(context.Background(), options.DirectModeOptions{FilePaths: []string{"/photo.jpg"}})

	var buf bytes.Buffer
	report.WriteText(&buf)
	out := buf.String()

	for _, want := range []string{"Preflight report (direct mode)", "no files were changed", "/photo.jpg", "decodable", "Total: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("text report does not contain %q.\nReport:\n%s", want, out)
		}
	}
}
