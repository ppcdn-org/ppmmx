// Package recordcleaner contains the recording cleaner.
package recordcleaner

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"code.cloudfoundry.org/bytefmt"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

var timeNow = time.Now

// diskFreeBytes is a variable (not a direct call to freeBytes) so tests
// can stub it without needing an actual near-full filesystem.
var diskFreeBytes = freeBytes

// minFreeSpaceProtectedAge is how recent a segment must be to always
// survive enforceMinFreeSpace's forced reclaim, regardless of how far
// under RecordMinFreeSpace the disk is. Segment filenames are stamped
// with when they started, not when they finished (a segment can still be
// actively being written to well past its own Start, e.g. up to
// recordSegmentDuration later - see recorder.formatFMP4Segment), so this
// must be at least the largest recordSegmentDuration in use, not just
// "how far back is safe to assume nothing is in progress"; 10 minutes is
// the current requirement.
const minFreeSpaceProtectedAge = 10 * time.Minute

// Cleaner removes expired recording segments from disk.
type Cleaner struct {
	PathConfs map[string]*conf.Path
	// RecordMinFreeSpace is a floor on free disk space, checked after the
	// age-based (recordDeleteAfter) pass on every filesystem that holds at
	// least one path's recordings. If still below this, the globally
	// oldest segments (across all paths sharing that filesystem) are
	// force-deleted until back above it. Zero disables the check.
	RecordMinFreeSpace conf.StringSize
	Parent             logger.Writer

	ctx       context.Context
	ctxCancel func()

	chReloadConf chan map[string]*conf.Path
	done         chan struct{}
}

// Initialize initializes a Cleaner.
func (c *Cleaner) Initialize() {
	c.ctx, c.ctxCancel = context.WithCancel(context.Background())
	c.chReloadConf = make(chan map[string]*conf.Path)
	c.done = make(chan struct{})

	go c.run()
}

// Close closes the Cleaner.
func (c *Cleaner) Close() {
	c.ctxCancel()
	<-c.done
}

// Log implements logger.Writer.
func (c *Cleaner) Log(level logger.Level, format string, args ...any) {
	c.Parent.Log(level, "[record cleaner]"+format, args...)
}

// ReloadPathConfs is called by core.Core.
func (c *Cleaner) ReloadPathConfs(pathConfs map[string]*conf.Path) {
	select {
	case c.chReloadConf <- pathConfs:
	case <-c.ctx.Done():
	}
}

func (c *Cleaner) run() {
	defer close(c.done)

	c.doRun() //nolint:errcheck

	for {
		select {
		case <-time.After(c.cleanInterval()):
			c.doRun()

		case cnf := <-c.chReloadConf:
			c.PathConfs = cnf

		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Cleaner) cleanInterval() time.Duration {
	interval := 30 * 60 * time.Second

	for _, e := range c.PathConfs {
		if e.RecordDeleteAfter != 0 &&
			interval > (time.Duration(e.RecordDeleteAfter)/2) {
			interval = time.Duration(e.RecordDeleteAfter) / 2
		}
	}

	return interval
}

func (c *Cleaner) doRun() {
	now := timeNow()

	pathNames := recordstore.FindAllPathsWithSegments(c.PathConfs)

	for _, pathName := range pathNames {
		c.processPath(now, pathName) //nolint:errcheck
	}

	c.enforceMinFreeSpace(now, pathNames)
}

func (c *Cleaner) processPath(now time.Time, pathName string) error {
	pathConf, _, err := conf.FindPathConf(c.PathConfs, pathName)
	if err != nil {
		return err
	}

	if pathConf.RecordDeleteAfter == 0 {
		return nil
	}

	err = c.deleteExpiredSegments(now, pathName, pathConf)
	if err != nil {
		return err
	}

	c.deleteEmptyDirs(pathConf)

	return nil
}

func (c *Cleaner) deleteExpiredSegments(now time.Time, pathName string, pathConf *conf.Path) error {
	end := now.Add(-time.Duration(pathConf.RecordDeleteAfter))
	segments, err := recordstore.FindSegments(pathConf, pathName, nil, &end)
	if err != nil {
		return err
	}

	for _, seg := range segments {
		c.Log(logger.Debug, "removing %s", seg.Fpath)
		os.Remove(seg.Fpath)
	}

	return nil
}

// pathRoot returns the filesystem root (CommonPath) of a path's recordings,
// or "" if the path has no config (e.g. was removed from PathConfs between
// FindAllPathsWithSegments and here).
func (c *Cleaner) pathRoot(pathName string) string {
	pathConf, _, err := conf.FindPathConf(c.PathConfs, pathName)
	if err != nil {
		return ""
	}
	recordPath := strings.ReplaceAll(pathConf.RecordPath, "%path", pathName)
	return recordstore.CommonPath(recordPath)
}

// enforceMinFreeSpace force-deletes the globally oldest recording segments
// (oldest first, across every path that shares the affected filesystem)
// until free space is back at or above RecordMinFreeSpace. This runs after
// the age-based (recordDeleteAfter) pass and only kicks in when that
// wasn't enough - e.g. recordDeleteAfter's window is longer than what the
// disk can hold at the current recording bitrate.
func (c *Cleaner) enforceMinFreeSpace(now time.Time, pathNames []string) {
	if c.RecordMinFreeSpace == 0 {
		return
	}

	rootToPaths := make(map[string][]string)
	for _, pathName := range pathNames {
		root := c.pathRoot(pathName)
		if root == "" {
			continue
		}
		rootToPaths[root] = append(rootToPaths[root], pathName)
	}

	for root, pathsOnRoot := range rootToPaths {
		c.enforceMinFreeSpaceOnRoot(now, root, pathsOnRoot)
	}
}

func (c *Cleaner) enforceMinFreeSpaceOnRoot(now time.Time, root string, pathsOnRoot []string) {
	free, err := diskFreeBytes(root)
	if err != nil {
		c.Log(logger.Warn, "unable to check free disk space on %s: %v", root, err)
		return
	}
	if free >= uint64(c.RecordMinFreeSpace) {
		return
	}

	// A segment's filename records when it started, not when it finished;
	// it can still be open and actively being written to well after that
	// (see minFreeSpaceProtectedAge). Never delete a path's single most
	// recent segment - the one most likely to still be open - nor any
	// segment younger than the protection window, regardless of how far
	// under the free-space floor the disk still is.
	protectedCutoff := now.Add(-minFreeSpaceProtectedAge)

	segments := make([]*recordstore.Segment, 0)
	segPath := make(map[*recordstore.Segment]string)
	for _, pathName := range pathsOnRoot {
		pathConf, _, err := conf.FindPathConf(c.PathConfs, pathName)
		if err != nil {
			continue
		}
		found, err := recordstore.FindSegments(pathConf, pathName, nil, nil)
		if err != nil || len(found) == 0 {
			continue
		}

		latest := found[0].Start
		for _, seg := range found[1:] {
			if seg.Start.After(latest) {
				latest = seg.Start
			}
		}

		for _, seg := range found {
			if !seg.Start.Before(protectedCutoff) || !seg.Start.Before(latest) {
				continue // too recent, or is this path's latest (possibly still-open) segment
			}
			segments = append(segments, seg)
			segPath[seg] = pathName
		}
	}
	if len(segments) == 0 {
		return
	}

	sort.Slice(segments, func(i, j int) bool {
		return segments[i].Start.Before(segments[j].Start)
	})

	c.Log(logger.Warn,
		"free space on %s (%s) is below the configured minimum (%s); force-deleting oldest recordings "+
			"(older than %s, excluding each path's most recent segment) until it recovers",
		root, bytefmt.ByteSize(free), bytefmt.ByteSize(uint64(c.RecordMinFreeSpace)), minFreeSpaceProtectedAge)

	for _, seg := range segments {
		if free >= uint64(c.RecordMinFreeSpace) {
			break
		}

		info, statErr := os.Stat(seg.Fpath)

		c.Log(logger.Debug, "removing %s (path=%s) to reclaim disk space", seg.Fpath, segPath[seg])
		if err := os.Remove(seg.Fpath); err != nil {
			continue
		}

		if statErr == nil {
			free += uint64(info.Size())
		} else {
			// Fall back to re-checking the filesystem if we couldn't
			// stat the file's size before removing it.
			if newFree, err := diskFreeBytes(root); err == nil {
				free = newFree
			}
		}
	}

	for _, pathName := range pathsOnRoot {
		if pathConf, _, err := conf.FindPathConf(c.PathConfs, pathName); err == nil {
			c.deleteEmptyDirs(pathConf)
		}
	}
}

func (c *Cleaner) deleteEmptyDirs(pathConf *conf.Path) {
	recordPath := strings.ReplaceAll(pathConf.RecordPath, "%path", pathConf.Name)
	commonPath := recordstore.CommonPath(recordPath)

	filepath.WalkDir(commonPath, func(fpath string, info fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return err
		}

		if info.IsDir() {
			os.Remove(fpath)
		}

		return nil
	})
}
