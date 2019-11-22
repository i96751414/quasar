package bittorrent

import (
	lt "github.com/anacrolix/torrent"
	"github.com/op/go-logging"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var tfsLog = logging.MustGetLogger("torrentfs")

// SeekableContent describes an io.ReadSeeker that can be closed as well.
type SeekableContent interface {
	io.ReadSeeker
	io.Closer
}

// FileEntry helps reading a torrent file.
type FileEntry struct {
	*BTFile
	lt.Reader
}

func (f *FileEntry) Close() (err error) {
	return f.Reader.Close()
}

// Seek seeks to the correct file position, paying attention to the offset.
func (f *FileEntry) Seek(offset int64, whence int) (int64, error) {
	return f.Reader.Seek(offset+f.BTFile.Offset(), whence)
}

// NewFileReader sets up a torrent file for streaming reading.
func NewFileReader(f *BTFile, sequential bool) (SeekableContent, error) {
	reader := f.Torrent().NewReader()

	if sequential {
		// We read ahead 2% of the file continuously.
		reader.SetReadahead(f.Length() / 50)
	}
	reader.SetResponsive()
	_, err := reader.Seek(f.Offset(), io.SeekStart)

	return &FileEntry{
		BTFile: f,
		Reader: reader,
	}, err
}

func serveTorrent(file *BTFile, sequential bool, w http.ResponseWriter, r *http.Request) {
	entry, err := NewFileReader(file, sequential)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	defer func() {
		if err := entry.Close(); err != nil {
			tfsLog.Errorf("Error closing file reader: %s\n", err)
		}
	}()

	w.Header().Set("Content-Disposition", "attachment; filename=\""+file.Torrent().Name()+"\"")
	http.ServeContent(w, r, file.DisplayPath(), time.Now(), entry)
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
					serveTorrent(f, !isRarArchive(path), w, r)
					return
				}
			}
		}

		http.Error(w, "file not found", http.StatusNotFound)
	}))
}
