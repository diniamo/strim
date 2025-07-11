package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	log "github.com/diniamo/glog"
)

type fileServer struct {
	title string
	path string
}

func (s *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	file, err := os.Open(s.path)
	if err != nil {
		log.Errorf("Failed to open file: %s", err)
		return
	}
	defer file.Close()

	http.ServeContent(w, r, filepath.Base(s.path), time.Time{}, file)
}

func (s *Server) serveStream() {
	err := s.fs.Serve(s.cmux.stream)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("File server failed: %s", err)
	}
}
