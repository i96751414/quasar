package bittorrent

import (
	lt "github.com/anacrolix/torrent"
	"io"
	"sync"
)

const sdBufferSize = 64 * 1024 // 64 KB

type DownloadStats interface {
	Length() int64
	BytesCompleted() int64
	NewReader() lt.Reader
}

type SequentialDownloader struct {
	sdReader      lt.Reader
	stats         DownloadStats
	mu            *sync.RWMutex
	closing       chan struct{}
	pos           int64
	posChanged    bool
	isDownloading bool
}

func NewSequentialDownloader(stats DownloadStats) *SequentialDownloader {
	reader := stats.NewReader()
	reader.SetReadahead(stats.Length() / 100)

	return &SequentialDownloader{
		sdReader: reader,
		stats:    stats,
		mu:       &sync.RWMutex{},
	}
}

func (sd *SequentialDownloader) Start() {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	if !sd.isDownloading {
		sd.closing = make(chan struct{})
		buf := make([]byte, sdBufferSize)
		go sd.sequentialDownload(buf)
		sd.isDownloading = true
	}
}

func (sd *SequentialDownloader) Seek(offset int64) {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.pos = offset
	sd.posChanged = true
}

func (sd *SequentialDownloader) Stop() {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	if sd.isDownloading {
		close(sd.closing)
		sd.isDownloading = false
	}
}

func (sd *SequentialDownloader) IsDownloading() bool {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.isDownloading
}

func (sd *SequentialDownloader) setDownloading(isDownloading bool) {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.isDownloading = isDownloading
}

func (sd *SequentialDownloader) sequentialDownload(buf []byte) {
	defer sd.setDownloading(false)

	for sd.stats.Length() > sd.stats.BytesCompleted() {
		select {
		case <-sd.closing:
			return
		default:
			if sd.posChanged {
				sd.mu.Lock()
				_, _ = sd.sdReader.Seek(sd.pos, io.SeekStart)
				sd.posChanged = false
				sd.mu.Unlock()
			}

			_, err := sd.sdReader.Read(buf)
			if err != nil {
				if err == io.EOF {
					if !sd.posChanged {
						_, _ = sd.sdReader.Seek(0, io.SeekStart)
					}
				} else {
					log.Errorf("Sequential download error: %s", err.Error())
					return
				}
			}
		}
	}
	log.Infof("Sequential download completed")
}

type SequentialReader struct {
	lt.Reader
	sd **SequentialDownloader
}

func (sr *SequentialReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := sr.Reader.Seek(offset, whence)
	if err == nil && *sr.sd != nil && (*sr.sd).IsDownloading() {
		(*sr.sd).Seek(pos)
	}
	return pos, err
}
