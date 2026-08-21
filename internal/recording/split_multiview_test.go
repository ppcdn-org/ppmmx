package recording

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// fakeViewResolver is a controllable TableViewResolver for testing
// table-with-multiple-views fan-out.
type fakeViewResolver struct {
	views map[string][]string
}

func (f *fakeViewResolver) ViewsForStream(streamName string) ([]string, error) {
	return f.views[streamName], nil
}

// multiviewTestController is a PathController whose SplitRecording result
// is distinguishable per instance, so a test can tell which path's
// controller was actually split.
type multiviewTestController struct {
	mu          sync.Mutex
	splitCount  int
	splitErr    error
	renamedTo   []string
	returnsPath string
}

func (c *multiviewTestController) StartOnDemandRecording(Options) (string, error) {
	return "recording.mp4", nil
}
func (c *multiviewTestController) StopOnDemandRecording() (int64, int64, error) { return 0, 0, nil }
func (c *multiviewTestController) IsOnline() bool                              { return true }
func (c *multiviewTestController) SplitRecording(renameTo string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.splitCount++
	if renameTo != "" {
		c.renamedTo = append(c.renamedTo, renameTo)
	}
	if c.splitErr != nil {
		return "", c.splitErr
	}
	if c.returnsPath != "" {
		return c.returnsPath, nil
	}
	return "recording.mp4", nil
}

func TestStartRoundRecordsEveryConfiguredView(t *testing.T) {
	const table = "table1"
	pathFwh := "live/table1-fwh"
	pathFwv := "live/table1-fwv"

	mgr := newTestManager(t)
	ctrlFwh := &multiviewTestController{}
	ctrlFwv := &multiviewTestController{}
	_, err := mgr.Start(pathFwh, Options{Format: "fmp4"}, ctrlFwh)
	require.NoError(t, err)
	_, err = mgr.Start(pathFwv, Options{Format: "fmp4"}, ctrlFwv)
	require.NoError(t, err)

	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	startReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1"}
	c := newSplitRecGinContext()
	require.NoError(t, h.execute(c, startReq))

	require.Equal(t, 1, ctrlFwh.splitCount, "fwh view must be started")
	require.Equal(t, 1, ctrlFwv.splitCount, "fwv view must also be started, not just the first match")

	// A concurrent start for the same table must be rejected, even though
	// the request only names the table (no view), confirming the lock is
	// table-scoped rather than per-path.
	require.Error(t, h.execute(newSplitRecGinContext(), startReq))

	stopReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1", GC: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))

	require.Equal(t, []string{"table1-fwh-round1-game1"}, ctrlFwh.renamedTo)
	require.Equal(t, []string{"table1-fwv-round1-game1"}, ctrlFwv.renamedTo)
}

func TestStartRoundSkipsMissingViewButRecordsTheRest(t *testing.T) {
	const table = "table1"
	pathFwh := "live/table1-fwh"
	// pathFwv is intentionally never started with mgr.Start: it has no
	// publisher/controller, simulating a configured view that isn't live.

	mgr := newTestManager(t)
	ctrlFwh := &multiviewTestController{}
	_, err := mgr.Start(pathFwh, Options{Format: "fmp4"}, ctrlFwh)
	require.NoError(t, err)

	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	startReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))
	require.Equal(t, 1, ctrlFwh.splitCount, "the view that is live must still be recorded")

	stopReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1", GC: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))
	require.Equal(t, []string{"table1-fwh-round1-game1"}, ctrlFwh.renamedTo)
}

func TestStartRoundFailsWhenNoConfiguredViewIsLive(t *testing.T) {
	const table = "table1"

	mgr := newTestManager(t)
	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	startReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1"}
	err := h.execute(newSplitRecGinContext(), startReq)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no path found")

	// The table lock must be released on total failure: retrying fails the
	// same way (no path found), not with "already being recorded" - proof
	// a failed start doesn't leave the table permanently locked.
	err = h.execute(newSplitRecGinContext(), startReq)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no path found")
}

func TestStopRoundContinuesOnPerPathFailure(t *testing.T) {
	const table = "table1"
	pathFwh := "live/table1-fwh"
	pathFwv := "live/table1-fwv"

	mgr := newTestManager(t)
	ctrlFwh := &multiviewTestController{}
	ctrlFwv := &multiviewTestController{splitErr: fmt.Errorf("split failed")}
	_, err := mgr.Start(pathFwh, Options{Format: "fmp4"}, ctrlFwh)
	require.NoError(t, err)
	_, err = mgr.Start(pathFwv, Options{Format: "fmp4"}, ctrlFwv)
	require.NoError(t, err)

	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	startReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))

	// fwv's stop-time split will fail; fwh's must still succeed and finalize.
	ctrlFwv.splitErr = fmt.Errorf("split failed")
	stopReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1", GC: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq),
		"one broken view must not prevent finalizing the others")
	require.Equal(t, []string{"table1-fwh-round1-game1"}, ctrlFwh.renamedTo)
}

func TestAppEnvDistinguishesOwnersWithSameGame(t *testing.T) {
	const table = "table1"
	path := "live/table1-fwh"

	mgr := newTestManager(t)
	ctrl := &multiviewTestController{}
	_, err := mgr.Start(path, Options{Format: "fmp4"}, ctrl)
	require.NoError(t, err)

	h := NewSplitRecHandler(mgr, nil, test.NilLogger)

	// Same game, different app_env: must be treated as different owners,
	// so the second start is rejected as "already being recorded by
	// another game" rather than "already has an active recording" (which
	// would imply it's the same owner retrying).
	startReq := splitRecRequest{Time: "9999999999", Table: table, Game: "p2w001", AppEnv: "test"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))

	otherEnvReq := splitRecRequest{Time: "9999999999", Table: table, Game: "p2w001", AppEnv: "prod"}
	err = h.execute(newSplitRecGinContext(), otherEnvReq)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already being recorded by another game")

	// The owner that did not start the round cannot stop it either.
	otherEnvStop := splitRecRequest{Time: "9999999999", Table: table, Game: "p2w001", AppEnv: "prod", GC: "round1"}
	err = h.execute(newSplitRecGinContext(), otherEnvStop)
	require.Error(t, err)
	require.Contains(t, err.Error(), "started by a different game")

	// The original owner (same app_env) can still stop it.
	stopReq := splitRecRequest{Time: "9999999999", Table: table, Game: "p2w001", AppEnv: "test", GC: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))
}

// TestStartRoundPersistsAppEnvToAuditRecord verifies that the split-rec
// request's app_env field is written into the recordings.db audit row -
// not just used transiently for owner identity/upload routing - so a
// deployment serving multiple game environments from one process can tell
// which environment produced a given recording after the fact.
//
// Looks up records by the recordID startRound generated (via h.activeGames,
// a white-box read - this file is part of package recording) rather than
// FindRunning(path): mgr.Start in test setup below also inserts its own
// "running" row for the same path (to register the fake controller so
// findController can see it), so more than one running row can exist per
// path and FindRunning's unordered SELECT could pick either one.
func TestStartRoundPersistsAppEnvToAuditRecord(t *testing.T) {
	pathWithEnv := "live/table1-fwh"
	pathWithoutEnv := "live/table2-fwh"

	mgr := newTestManager(t)
	ctrlWithEnv := &multiviewTestController{}
	_, err := mgr.Start(pathWithEnv, Options{Format: "fmp4"}, ctrlWithEnv)
	require.NoError(t, err)
	ctrlWithoutEnv := &multiviewTestController{}
	_, err = mgr.Start(pathWithoutEnv, Options{Format: "fmp4"}, ctrlWithoutEnv)
	require.NoError(t, err)

	h := NewSplitRecHandler(mgr, nil, test.NilLogger)

	startWithEnv := splitRecRequest{Time: "9999999999", Table: "table1", Game: "p2w001", AppEnv: "uat"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startWithEnv))
	recordIDWithEnv := h.activeGames["table1"].recordIDs[pathWithEnv]
	require.NotEmpty(t, recordIDWithEnv)

	startWithoutEnv := splitRecRequest{Time: "9999999999", Table: "table2", Game: "p2w002"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startWithoutEnv))
	recordIDWithoutEnv := h.activeGames["table2"].recordIDs[pathWithoutEnv]
	require.NotEmpty(t, recordIDWithoutEnv)

	recWithEnv, err := mgr.Store().Get(recordIDWithEnv)
	require.NoError(t, err)
	require.NotNil(t, recWithEnv)
	require.Equal(t, "uat", recWithEnv.AppEnv)

	// A caller that omits app_env must persist an empty string, not just
	// "not set" behaving coincidentally like empty.
	recWithoutEnv, err := mgr.Store().Get(recordIDWithoutEnv)
	require.NoError(t, err)
	require.NotNil(t, recWithoutEnv)
	require.Empty(t, recWithoutEnv.AppEnv)
}

func TestNoAppEnvFallsBackToGameOnlyOwnership(t *testing.T) {
	const table = "table1"
	path := "live/table1-fwh"

	mgr := newTestManager(t)
	ctrl := &multiviewTestController{}
	_, err := mgr.Start(path, Options{Format: "fmp4"}, ctrl)
	require.NoError(t, err)

	h := NewSplitRecHandler(mgr, nil, test.NilLogger)

	// No app_env in either call: behaves exactly like before (game alone
	// identifies the owner).
	startReq := splitRecRequest{Time: "9999999999", Table: table, Game: "p2w001"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))

	stopReq := splitRecRequest{Time: "9999999999", Table: table, Game: "p2w001", GC: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))
}

// TestStopRoundWithoutPriorStartIsDroppedNotAnError verifies that a stop
// call for a table with no matching prior start is silently dropped
// (returns success, no error) rather than surfacing as a 500 - the caller
// may fire a stop signal unconditionally regardless of whether a start
// actually preceded it. This is distinct from stopping a table that's
// held by a *different* owner, which is a real conflict and must still
// fail (see TestAppEnvDistinguishesOwnersWithSameGame).
func TestStopRoundWithoutPriorStartIsDroppedNotAnError(t *testing.T) {
	const table = "table1"

	mgr := newTestManager(t)
	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	// No start round was ever issued for this table.
	stopReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1", GC: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq),
		"a stop with no matching start must be dropped, not treated as an error")

	// The table must remain free to start afterward - the dropped stop
	// must not have left any stray lock/state behind.
	startReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1"}
	path := "live/table1-fwh"
	ctrl := &multiviewTestController{}
	_, err := mgr.Start(path, Options{Format: "fmp4"}, ctrl)
	require.NoError(t, err)
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))
}

func TestSingleViewTableStillWorksWithoutResolver(t *testing.T) {
	const table = "legacy-table"
	path := "live/legacy-table-fwh"

	mgr := newTestManager(t)
	ctrl := &multiviewTestController{}
	_, err := mgr.Start(path, Options{Format: "fmp4"}, ctrl)
	require.NoError(t, err)

	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	// No SetViewResolver call: falls back to the legacy default.

	startReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))
	require.Equal(t, 1, ctrl.splitCount)

	stopReq := splitRecRequest{Time: "9999999999", Table: table, Game: "game1", GC: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))
	require.Equal(t, []string{"legacy-table-fwh-round1-game1"}, ctrl.renamedTo)
}
