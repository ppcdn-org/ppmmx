package webrtc

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/recording"
)

// ── Recording API handlers ──────────────────────────────────────

type recordingPathFinder interface {
	FindPath(name string) (recording.PathController, bool)
}

func (s *httpServer) onRecordingStart(ctx *gin.Context) {
	var req struct {
		Path            string `json:"path"`
		Format          string `json:"format"`
		SegmentDuration string `json:"segmentDuration"`
		MaxPartSize     int64  `json:"maxPartSize"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Path == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}

	mgr := s.parent.RecMgr
	if mgr == nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "recording manager not initialized"})
		return
	}

	// Find the path controller
	pathFinder, ok := s.pathManager.(recordingPathFinder)
	if !ok {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "recording path lookup is not available"})
		return
	}
	ctrl, ok := pathFinder.FindPath(req.Path)
	if !ok {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "stream not found"})
		return
	}

	opts := recording.Options{
		Format: "fmp4",
	}
	if req.Format != "" {
		opts.Format = req.Format
	}
	if req.SegmentDuration != "" {
		if d, err := time.ParseDuration(req.SegmentDuration); err == nil {
			opts.SegmentDuration = d
		}
	}
	opts.MaxPartSize = req.MaxPartSize

	r, err := mgr.Start(req.Path, opts, ctrl)
	if err != nil {
		ctx.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	ctx.JSON(http.StatusCreated, r)
}

func (s *httpServer) onRecordingStop(ctx *gin.Context) {
	var req struct {
		Path string `json:"path"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Path == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}

	mgr := s.parent.RecMgr
	if mgr == nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "recording manager not initialized"})
		return
	}

	r, err := mgr.Stop(req.Path)
	if err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, r)
}

func (s *httpServer) onRecordingList(ctx *gin.Context) {
	mgr := s.parent.RecMgr
	if mgr == nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "recording manager not initialized"})
		return
	}

	path := ctx.Query("path")
	status := ctx.Query("status")
	limit := 20
	offset := 0

	records, total, err := mgr.Store().List(path, status, limit, offset)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if records == nil {
		records = []recording.Record{}
	}

	ctx.JSON(http.StatusOK, gin.H{
		"total": total,
		"items": records,
	})
}

func (s *httpServer) onRecordingDelete(ctx *gin.Context) {
	// Extract ID from /api/v1/recordings/:id
	id := strings.TrimPrefix(ctx.Request.URL.Path, "/api/v1/recordings/")
	if id == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "recording id required"})
		return
	}

	mgr := s.parent.RecMgr
	if mgr == nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "recording manager not initialized"})
		return
	}

	// Check if running
	r, err := mgr.Store().Get(id)
	if err != nil || r == nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "recording not found"})
		return
	}
	if r.Status == "running" {
		ctx.JSON(http.StatusConflict, gin.H{"error": "cannot delete running recording"})
		return
	}

	if err := mgr.Store().Delete(id); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"deleted": true, "id": id, "filePath": r.FilePath})
}
