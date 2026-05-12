package hls

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Server serves HLS segments and playlists from a directory.
// FFmpeg writes .m3u8 and .ts files here; the browser or Cloudflare pulls them.
type Server struct {
	mu  sync.RWMutex
	dir string
}

// NewServer creates an HLS server that serves files from dir.
func NewServer(dir string) (*Server, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Server{dir: dir}, nil
}

// Dir returns the directory where HLS segments are written.
func (s *Server) Dir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dir
}

// Clean removes all HLS files from the output directory.
func (s *Server) Clean() error {
	dir := s.Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		ext := filepath.Ext(name)
		if ext == ".m3u8" || ext == ".ts" {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	return nil
}

// ServeHTTP serves HLS files with appropriate CORS and cache headers.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS headers so external players/CDNs can fetch the playlist.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ext := filepath.Ext(r.URL.Path)
	switch ext {
	case ".m3u8":
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "public, max-age=60")
	}

	fs := http.StripPrefix("/hls/", http.FileServer(http.Dir(s.Dir())))
	fs.ServeHTTP(w, r)
}
