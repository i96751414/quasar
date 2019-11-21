package bittorrent

import (
	"io"

	lt "github.com/anacrolix/torrent"
)

// SeekableContent describes an io.ReadSeeker that can be closed as well.
type SeekableContent interface {
	io.ReadSeeker
	io.Closer
}

// FileEntry helps reading a torrent file.
type FileEntry struct {
	*lt.File
	lt.Reader
}

func (f *FileEntry) Close() (err error) {
	return f.Reader.Close()
}

// Seek seeks to the correct file position, paying attention to the offset.
func (f *FileEntry) Seek(offset int64, whence int) (int64, error) {
	return f.Reader.Seek(offset+f.File.Offset(), whence)
}

// NewFileReader sets up a torrent file for streaming reading.
func NewFileReader(f *lt.File, sequential bool) (SeekableContent, error) {
	torrent := f.Torrent()
	reader := torrent.NewReader()

	if sequential {
		// We read ahead 1% of the file continuously.
		reader.SetReadahead(f.Length() / 100)
	}
	reader.SetResponsive()
	_, err := reader.Seek(f.Offset(), io.SeekStart)

	return &FileEntry{
		File:   f,
		Reader: reader,
	}, err
}
