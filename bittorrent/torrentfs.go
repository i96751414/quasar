package bittorrent

import (
	"github.com/op/go-logging"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var tfsLog = logging.MustGetLogger("torrentfs")

func serveTorrent(file *BTFile, w http.ResponseWriter, r *http.Request) {
	reader := file.NewReader()
	// We read ahead 1% of the file continuously.
	reader.SetReadahead(file.Length() / 100)
	reader.SetResponsive()

	defer func() {
		if err := reader.Close(); err != nil {
			tfsLog.Errorf("Error closing file reader: %s\n", err)
		}
	}()

	w.Header().Set("Content-Disposition", "attachment; filename=\""+file.Torrent().Name()+"\"")
	http.ServeContent(w, r, file.DisplayPath(), time.Now(), reader)
}

func ServeTorrent(s *BTService, downloadPath string) http.Handler {
	return http.StripPrefix("/files", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		filePath := filepath.Join(downloadPath, path)
		file, err := os.Open(filePath)
		if err != nil {
			tfsLog.Errorf("Unable to open '%s'", filePath)
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		// make sure we don't open a file that's locked, as it can happen
		// on BSD systems (darwin included)
		if err := unlockFile(file); err != nil {
			tfsLog.Errorf("Unable to unlock file because: %s", err)
		}

		// Get the torrent
		for _, torrent := range s.Torrents {
			for _, f := range torrent.Files() {
				if path[1:] == f.Path() {
					tfsLog.Noticef("%s belongs to torrent %s", path, torrent.Name())
					serveTorrent(f, w, r)
					return
				}
			}
		}

		http.Error(w, "file not found", http.StatusNotFound)
	}))
}
