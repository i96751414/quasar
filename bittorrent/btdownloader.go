package bittorrent

import (
	lt "github.com/anacrolix/torrent"
	"github.com/dustin/go-humanize"
	"io"
	"sync"
	"time"
)

const sdBufferSize = 256 * 1024 // 256 KB

type Downloader interface {
	Length() int64
	BytesCompleted() int64
	NewReader() lt.Reader
}

type SequentialDownloader struct {
	sdReader      lt.Reader
	downloader    Downloader
	mu            *sync.RWMutex
	closing       chan struct{}
	pos           int64
	posChanged    bool
	isDownloading bool
}

func NewSequentialDownloader(downloader Downloader) *SequentialDownloader {
	reader := downloader.NewReader()
	reader.SetReadahead(downloader.Length() / 100)

	return &SequentialDownloader{
		sdReader:   reader,
		downloader: downloader,
		mu:         &sync.RWMutex{},
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
	var total uint64
	start := time.Now()

	for sd.downloader.Length() > sd.downloader.BytesCompleted() {
		if sd.posChanged {
			sd.mu.Lock()
			_, _ = sd.sdReader.Seek(sd.pos, io.SeekStart)
			sd.posChanged = false
			sd.mu.Unlock()
		}

		n, err := sd.sdReader.Read(buf)
		if err != nil {
			if err == io.EOF {
				if !sd.posChanged {
					log.Debugf("Sequential download seeking start")
					_, _ = sd.sdReader.Seek(0, io.SeekStart)
				}
			} else {
				log.Errorf("Sequential download error: %s", err.Error())
				return
			}
		} else {
			total += uint64(n)
		}
	}
	end := time.Since(start)
	log.Infof("Sequential download completed in %s after downloading %s at an average of %s/s",
		end, humanize.IBytes(total), humanize.IBytes(uint64(float64(total)/end.Seconds())))
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
