package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

type FsHandler struct {
	baseDir string // all browsing is restricted to paths within this directory
}

func NewFsHandler(baseDir string) *FsHandler {
	cleaned := ""
	if baseDir != "" {
		cleaned = filepath.Clean(baseDir)
	}
	return &FsHandler{baseDir: cleaned}
}

type fsDirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type fsBrowseResponse struct {
	Path    string       `json:"path"`
	Parent  string       `json:"parent"`
	Entries []fsDirEntry `json:"entries"`
}

// Browse lists subdirectories at the given path, restricted to baseDir.
// GET /api/v1/fs/browse?path=/some/path
func (h *FsHandler) Browse(c *gin.Context) {
	if h.baseDir == "" {
		respondErr(c, http.StatusForbidden, "filesystem browsing not configured")
		return
	}

	raw := c.DefaultQuery("path", h.baseDir)
	path := filepath.Clean(raw)

	// Enforce that the resolved path stays within the configured base directory.
	if !strings.HasPrefix(path+string(filepath.Separator), h.baseDir+string(filepath.Separator)) {
		respondErr(c, http.StatusForbidden, "path outside allowed directory")
		return
	}

	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		respondErr(c, http.StatusBadRequest, "invalid path")
		return
	}

	rawEntries, err := os.ReadDir(path)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to read directory")
		return
	}

	var dirs []fsDirEntry
	for _, e := range rawEntries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		dirs = append(dirs, fsDirEntry{
			Name: name,
			Path: filepath.Join(path, name),
		})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })

	parent := filepath.Dir(path)
	// Don't expose a parent above the base directory.
	if parent == path || !strings.HasPrefix(parent+string(filepath.Separator), h.baseDir+string(filepath.Separator)) {
		parent = ""
	}

	respondOK(c, fsBrowseResponse{
		Path:    path,
		Parent:  parent,
		Entries: dirs,
	})
}
