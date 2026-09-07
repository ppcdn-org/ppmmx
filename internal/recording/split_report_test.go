package recording

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// fakeSplitRecFileReporter records every ReportSplitRecFile call it
// receives, optionally returning a canned error.
type fakeSplitRecFileReporter struct {
	mu      sync.Mutex
	reports []SplitRecFileInfo
	err     error
}

func (f *fakeSplitRecFileReporter) ReportSplitRecFile(_ context.Context, file SplitRecFileInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, file)
	return f.err
}

func (f *fakeSplitRecFileReporter) calls() []SplitRecFileInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SplitRecFileInfo(nil), f.reports...)
}

// TestReportSplitRecFileFillsObjectKeyAppEnvAndPlaybackURL verifies
// reportSplitRecFile (called by uploadWithRetry only after a successful
// upload) forwards the caller-supplied round identity untouched and fills
// in the three fields only known at upload time: objectKey, appEnv (the
// resolved environment the file actually landed under), and playbackUrl.
func TestReportSplitRecFileFillsObjectKeyAppEnvAndPlaybackURL(t *testing.T) {
	reporter := &fakeSplitRecFileReporter{}
	u := newUploader(UploadConfig{
		MinioEndpoint: "127.0.0.1:0", MinioAccessKey: "ak", MinioSecretKey: "sk",
		MinioDomain: "cdn.example.com",
	}, test.NilLogger)
	u.reporter = reporter

	u.reportSplitRecFile("table1-fwh-gc001-p2w001.mp4", "test", SplitRecFileInfo{
		TableID: "table1", GameID: "p2w001", GameRound: "gc001",
		StreamPath: "live/table1-fwh", FileName: "table1-fwh-gc001-p2w001.mp4",
		DurationSeconds: 45, SizeBytes: 1024000,
	})

	calls := reporter.calls()
	require.Len(t, calls, 1)
	got := calls[0]
	require.Equal(t, "table1", got.TableID)
	require.Equal(t, "p2w001", got.GameID)
	require.Equal(t, "gc001", got.GameRound)
	require.Equal(t, "live/table1-fwh", got.StreamPath)
	require.Equal(t, "table1-fwh-gc001-p2w001.mp4", got.FileName)
	require.Equal(t, int64(45), got.DurationSeconds)
	require.Equal(t, int64(1024000), got.SizeBytes)
	require.Equal(t, "test", got.AppEnv, "appEnv must be the resolved env, filled in by reportSplitRecFile")
	require.Equal(t, "table1-fwh-gc001-p2w001.mp4", got.ObjectKey)
	require.Equal(t, "https://cdn.example.com/test/table1-fwh-gc001-p2w001.mp4", got.PlaybackURL)
}

// TestReportSplitRecFileNoopWithoutReporter verifies a nil reporter (the
// default until SetSplitRecFileReporter is called - e.g. mmxControl
// disabled) is silently skipped rather than panicking.
func TestReportSplitRecFileNoopWithoutReporter(t *testing.T) {
	u := newUploader(UploadConfig{}, test.NilLogger)
	require.NotPanics(t, func() {
		u.reportSplitRecFile("key.mp4", "prod", SplitRecFileInfo{TableID: "t", GameID: "g", GameRound: "r", FileName: "key.mp4"})
	})
}

// TestReportSplitRecFileLogsButDoesNotPanicOnReporterError verifies a
// reporter failure (ppcenter unreachable, 500, etc.) is swallowed with a
// log line rather than propagated - the file has already been uploaded
// successfully by the time this runs, so there's nothing to roll back.
func TestReportSplitRecFileLogsButDoesNotPanicOnReporterError(t *testing.T) {
	reporter := &fakeSplitRecFileReporter{err: fmt.Errorf("ppcenter unreachable")}
	u := newUploader(UploadConfig{}, test.NilLogger)
	u.reporter = reporter

	require.NotPanics(t, func() {
		u.reportSplitRecFile("key.mp4", "prod", SplitRecFileInfo{TableID: "t", GameID: "g", GameRound: "r", FileName: "key.mp4"})
	})
	require.Len(t, reporter.calls(), 1)
}

// TestConfigureUploadCarriesReporterForward verifies ConfigureUpload (a
// config reload swapping in a fresh *uploader) preserves whatever reporter
// SetSplitRecFileReporter already wired in - see ConfigureUpload's doc
// comment for why this matters (reload order doesn't guarantee
// SetSplitRecFileReporter reruns after it).
func TestConfigureUploadCarriesReporterForward(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	reporter := &fakeSplitRecFileReporter{}
	h.SetSplitRecFileReporter(reporter)
	h.ConfigureUpload(UploadConfig{})

	require.Same(t, SplitRecFileReporter(reporter), h.uploader.reporter)
}

// TestSetSplitRecFileReporterUpdatesExistingUploader verifies calling
// SetSplitRecFileReporter after ConfigureUpload (the normal boot order:
// splitHandler.ConfigureUpload runs before the mmxControl-based reporter
// exists) still reaches the live uploader instance.
func TestSetSplitRecFileReporterUpdatesExistingUploader(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureUpload(UploadConfig{})
	reporter := &fakeSplitRecFileReporter{}
	h.SetSplitRecFileReporter(reporter)

	require.Same(t, SplitRecFileReporter(reporter), h.uploader.reporter)
}
