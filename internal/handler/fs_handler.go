package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

type FsHandler struct{}

func NewFsHandler() *FsHandler {
	return &FsHandler{}
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

// Browse lists subdirectories at the given path.
// GET /api/v1/fs/browse?path=/some/path
func (h *FsHandler) Browse(c *gin.Context) {
	path := c.DefaultQuery("path", "/")
	path = filepath.Clean(path)

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
	if parent == path {
		parent = ""
	}

	respondOK(c, fsBrowseResponse{
		Path:    path,
		Parent:  parent,
		Entries: dirs,
	})
}
