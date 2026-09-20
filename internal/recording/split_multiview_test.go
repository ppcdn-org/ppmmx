package recording

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// fakeViewResolver is a controllable TableViewResolver for testing
// table-with-multiple-views fan-out. Only returns view names - the actual
// path (appId+"/"+table+"-"+view) is derived by tableToPaths, not by the
// resolver (see TableViewResolver's doc comment).
type fakeViewResolver struct {
	views map[string][]string
}

func (f *fakeViewResolver) ViewsForTable(table string) ([]string, error) {
	return f.views[table], nil
}

// fakePathFinder is a minimal PathFinder test double: split-rec looks paths
// up here (via SplitRecHandler.pathFinder) the same way it looks them up in
// the real path manager in production. Registering a controller here rather
// than via mgr.Start keeps each test's fake controller decoupled from
// Manager's own on-demand-recording bookkeeping, so a round-start's
// StartOnDemandRecording call isn't preceded by an unrelated one already
// made during test setup.
type fakePathFinder map[string]PathController

func (f fakePathFinder) FindPath(name string) (PathController, bool) {
	ctrl, ok := f[name]
	return ctrl, ok
}

// multiviewTestController is a PathController whose SplitRecording result
// is distinguishable per instance, so a test can tell which path's
// controller was actually split.
type multiviewTestController struct {
	mu          sync.Mutex
	startCount  int
	splitCount  int
	splitErr    error
	renamedTo   []string
	returnsPath string
}

func (c *multiviewTestController) StartOnDemandRecording(Options) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startCount++
	return "recording.mp4", nil
}
func (c *multiviewTestController) StopOnDemandRecording() (int64, int64, error) { return 0, 0, nil }
func (c *multiviewTestController) IsOnline() bool                               { return true }
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
	const appID = "app1"
	const table = "table1"
	pathFwh := "app1/table1-fwh"
	pathFwv := "app1/table1-fwv"

	mgr := newTestManager(t)
	ctrlFwh := &multiviewTestController{}
	ctrlFwv := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{pathFwh: ctrlFwh, pathFwv: ctrlFwv}, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1"}
	c := newSplitRecGinContext()
	require.NoError(t, h.execute(c, startReq))

	require.Equal(t, 1, ctrlFwh.startCount, "fwh view must be started")
	require.Equal(t, 1, ctrlFwv.startCount, "fwv view must also be started, not just the first match")

	// A concurrent start for the same (appId, table) must be rejected, even
	// though the request only names the table (no view), confirming the
	// lock is table-scoped rather than per-path.
	require.Error(t, h.execute(newSplitRecGinContext(), startReq))

	stopReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1", GameRound: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))

	require.Equal(t, []string{"table1-fwh-round1-game1"}, ctrlFwh.renamedTo)
	require.Equal(t, []string{"table1-fwv-round1-game1"}, ctrlFwv.renamedTo)
}

func TestStartRoundSkipsMissingViewButRecordsTheRest(t *testing.T) {
	const appID = "app1"
	const table = "table1"
	pathFwh := "app1/table1-fwh"
	// pathFwv is intentionally never registered in the path finder: it has
	// no publisher/controller, simulating a configured view that isn't live.

	mgr := newTestManager(t)
	ctrlFwh := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{pathFwh: ctrlFwh}, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))
	require.Equal(t, 1, ctrlFwh.startCount, "the view that is live must still be recorded")

	stopReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1", GameRound: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))
	require.Equal(t, []string{"table1-fwh-round1-game1"}, ctrlFwh.renamedTo)
}

func TestStartRoundFailsWhenNoConfiguredViewIsLive(t *testing.T) {
	const appID = "app1"
	const table = "table1"

	mgr := newTestManager(t)
	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1"}
	err := h.execute(newSplitRecGinContext(), startReq)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not online")

	// The table lock must be released on total failure: retrying fails the
	// same way (not online), not with "already being recorded" - proof a
	// failed start doesn't leave the table permanently locked.
	err = h.execute(newSplitRecGinContext(), startReq)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not online")
}

func TestStopRoundContinuesOnPerPathFailure(t *testing.T) {
	const appID = "app1"
	const table = "table1"
	pathFwh := "app1/table1-fwh"
	pathFwv := "app1/table1-fwv"

	mgr := newTestManager(t)
	ctrlFwh := &multiviewTestController{}
	ctrlFwv := &multiviewTestController{splitErr: fmt.Errorf("split failed")}

	h := NewSplitRecHandler(mgr, fakePathFinder{pathFwh: ctrlFwh, pathFwv: ctrlFwv}, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))

	// fwv's stop-time split will fail; fwh's must still succeed and finalize.
	ctrlFwv.splitErr = fmt.Errorf("split failed")
	stopReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1", GameRound: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq),
		"one broken view must not prevent finalizing the others")
	require.Equal(t, []string{"table1-fwh-round1-game1"}, ctrlFwh.renamedTo)
}

func TestAppEnvDistinguishesOwnersWithSameGame(t *testing.T) {
	const appID = "app1"
	const table = "table1"
	path := "app1/table1-fwh"

	mgr := newTestManager(t)
	ctrl := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{path: ctrl}, test.NilLogger)

	// Same game, different app_env: must be treated as different owners,
	// so the second start is rejected as "already being recorded by
	// another game" rather than "already has an active recording" (which
	// would imply it's the same owner retrying).
	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "p2w001", AppEnv: "test"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))

	otherEnvReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "p2w001", AppEnv: "prod"}
	err := h.execute(newSplitRecGinContext(), otherEnvReq)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already being recorded by another game")

	// The owner that did not start the round cannot stop it either.
	otherEnvStop := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "p2w001", AppEnv: "prod", GameRound: "round1"}
	err = h.execute(newSplitRecGinContext(), otherEnvStop)
	require.Error(t, err)
	require.Contains(t, err.Error(), "started by a different game")

	// The original owner (same app_env) can still stop it.
	stopReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "p2w001", AppEnv: "test", GameRound: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))
}

// TestStartRoundPersistsAppEnvToAuditRecord verifies that the split-rec
// request's app_env field is written into the recordings.db audit row -
// not just used transiently for owner identity/upload routing - so a
// deployment serving multiple game environments from one process can tell
// which environment produced a given recording after the fact. Looks up
// records by the recordID startRound generated (via h.activeGames, a
// white-box read - this file is part of package recording) since that ID
// isn't otherwise returned to the caller.
func TestStartRoundPersistsAppEnvToAuditRecord(t *testing.T) {
	const appID = "app1"
	pathWithEnv := "app1/table1-fwh"
	pathWithoutEnv := "app1/table2-fwh"

	mgr := newTestManager(t)
	ctrlWithEnv := &multiviewTestController{}
	ctrlWithoutEnv := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{pathWithEnv: ctrlWithEnv, pathWithoutEnv: ctrlWithoutEnv}, test.NilLogger)

	startWithEnv := splitRecRequest{Time: "9999999999", AppID: appID, TableID: "table1", GameID: "p2w001", AppEnv: "uat"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startWithEnv))
	recordIDWithEnv := h.activeGames[tableLockKey(appID, "table1")].recordIDs[pathWithEnv]
	require.NotEmpty(t, recordIDWithEnv)

	startWithoutEnv := splitRecRequest{Time: "9999999999", AppID: appID, TableID: "table2", GameID: "p2w002"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startWithoutEnv))
	recordIDWithoutEnv := h.activeGames[tableLockKey(appID, "table2")].recordIDs[pathWithoutEnv]
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
	const appID = "app1"
	const table = "table1"
	path := "app1/table1-fwh"

	mgr := newTestManager(t)
	ctrl := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{path: ctrl}, test.NilLogger)

	// No app_env in either call: behaves exactly like before (game alone
	// identifies the owner).
	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "p2w001"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))

	stopReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "p2w001", GameRound: "round1"}
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
	const appID = "app1"
	const table = "table1"
	path := "app1/table1-fwh"

	mgr := newTestManager(t)
	ctrl := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{path: ctrl}, test.NilLogger)
	h.SetViewResolver(&fakeViewResolver{views: map[string][]string{table: {"fwh", "fwv"}}})

	// No start round was ever issued for this table.
	stopReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1", GameRound: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq),
		"a stop with no matching start must be dropped, not treated as an error")

	// The table must remain free to start afterward - the dropped stop
	// must not have left any stray lock/state behind.
	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))
}

// TestDifferentAppsWithSameTableIDDoNotContendForTheLock verifies that the
// activeGames lock is scoped to (appId, tableId), not tableId alone: two
// different apps using the identical tableId string must be able to hold
// independent rounds at the same time, since their resolved paths
// (appId+"/"+tableId+"-"+view) are unrelated physical streams.
func TestDifferentAppsWithSameTableIDDoNotContendForTheLock(t *testing.T) {
	const table = "table"

	mgr := newTestManager(t)
	ctrlA := &multiviewTestController{}
	ctrlB := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{"appA/table-fwh": ctrlA, "appB/table-fwh": ctrlB}, test.NilLogger)

	startA := splitRecRequest{Time: "9999999999", AppID: "appA", TableID: table, GameID: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startA))

	startB := splitRecRequest{Time: "9999999999", AppID: "appB", TableID: table, GameID: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startB),
		"a different app with the same tableId string must not be blocked by appA's lock")

	require.Equal(t, 1, ctrlA.startCount)
	require.Equal(t, 1, ctrlB.startCount)
}

func TestSingleViewTableStillWorksWithoutResolver(t *testing.T) {
	const appID = "app1"
	const table = "legacy-table"
	path := "app1/legacy-table-fwh"

	mgr := newTestManager(t)
	ctrl := &multiviewTestController{}

	h := NewSplitRecHandler(mgr, fakePathFinder{path: ctrl}, test.NilLogger)
	// No SetViewResolver call: falls back to the single-view "fwh" default.

	startReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), startReq))
	require.Equal(t, 1, ctrl.startCount)

	stopReq := splitRecRequest{Time: "9999999999", AppID: appID, TableID: table, GameID: "game1", GameRound: "round1"}
	require.NoError(t, h.execute(newSplitRecGinContext(), stopReq))
	require.Equal(t, []string{"legacy-table-fwh-round1-game1"}, ctrl.renamedTo)
}
