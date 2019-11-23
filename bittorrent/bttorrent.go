package bittorrent

import (
	"bytes"
	lt "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type TorrentStatus int

const (
	StatusQueued TorrentStatus = iota
	StatusChecking
	StatusFinding
	StatusPaused
	StatusBuffering
	StatusDownloading
	StatusFinished
	StatusSeeding
)

var StatusStrings = []string{
	"Queued",
	"Checking",
	"Finding",
	"Paused",
	"Buffering",
	"Downloading",
	"Finished",
	"Seeding",
}

type BTTorrent struct {
	torrent         *lt.Torrent
	service         *BTService
	infoHash        string
	closing         chan struct{}
	mu              *sync.RWMutex
	files           []*BTFile
	lastUpdateTime  time.Time
	downloadRate    float64
	uploadRate      float64
	seedingTime     float64
	finishedTime    float64
	activeTime      float64
	isPaused        bool
	downloadStarted bool
	bytesCompleted  int64
}

type BTTorrentStats struct {
	lt.TorrentStats
	TotalSeeders             int
	ConnectedIncompletePeers int
	TotalIncompletePeers     int
	DownloadRate             float64
	UploadRate               float64
	SeedingTime              float64
	FinishedTime             float64
	ActiveTime               float64
}

func NewBTTorrent(service *BTService, handle *lt.Torrent) *BTTorrent {
	t := &BTTorrent{
		torrent:  handle,
		service:  service,
		infoHash: handle.InfoHash().HexString(),
		closing:  make(chan struct{}),
		mu:       &sync.RWMutex{},
	}

	return t
}

func (t *BTTorrent) GotInfo() <-chan struct{} {
	return t.torrent.GotInfo()
}

func (t *BTTorrent) Info() *metainfo.Info {
	return t.torrent.Info()
}

func (t *BTTorrent) Metainfo() metainfo.MetaInfo {
	return t.torrent.Metainfo()
}

func (t *BTTorrent) Stats() lt.TorrentStats {
	return t.torrent.Stats()
}

func (t *BTTorrent) NumPieces() int {
	return t.torrent.NumPieces()
}

func (t *BTTorrent) Length() int64 {
	return t.torrent.Length()
}

func (t *BTTorrent) Name() string {
	return t.torrent.Name()
}

func (t *BTTorrent) InfoHashString() string {
	return t.infoHash
}

func (t *BTTorrent) PieceBytesMissing(piece int) int64 {
	return t.torrent.PieceBytesMissing(piece)
}

func (t *BTTorrent) PieceStateRuns() []lt.PieceStateRun {
	return t.torrent.PieceStateRuns()
}

func (t *BTTorrent) NewReader() lt.Reader {
	return t.torrent.NewReader()
}

func (t *BTTorrent) BytesMissing() int64 {
	return t.torrent.Length() - t.bytesCompleted
}

func (t *BTTorrent) BytesCompleted() int64 {
	return t.bytesCompleted
}

func (t *BTTorrent) Files() []*BTFile {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.files == nil {
		files := t.torrent.Files()
		t.files = make([]*BTFile, len(files))
		for i, f := range files {
			t.files[i] = NewBTFile(f, t)
		}
	}
	return t.files
}

func (t *BTTorrent) SaveMetaInfo(path string) error {
	var buf bytes.Buffer
	if err := t.Metainfo().Write(&buf); err != nil {
		return err
	}
	return ioutil.WriteFile(path, buf.Bytes(), 0644)
}

func (t *BTTorrent) watch() {
	var downloadedSize int64
	var uploadedSize int64

	lenRateCounter := 5
	downRates := make([]float64, lenRateCounter)
	upRates := make([]float64, lenRateCounter)
	rateCounterIdx := 0

	progressTicker := time.NewTicker(1000 * time.Millisecond)
	defer progressTicker.Stop()

	for {
		select {
		case <-progressTicker.C:
			go func() {
				t.mu.Lock()
				defer t.mu.Unlock()

				t.bytesCompleted = t.torrent.BytesCompleted()
				for _, f := range t.files {
					if f.markedForDownload {
						f.updateStats()
					}
				}

				timeNow := time.Now()
				timeDif := timeNow.Sub(t.lastUpdateTime).Seconds()
				t.lastUpdateTime = timeNow

				// Update times
				status := t.getState(t.files...)
				if status != StatusPaused {
					t.activeTime += timeDif
					switch status {
					case StatusSeeding:
						t.seedingTime += timeDif
						fallthrough
					case StatusFinished:
						t.finishedTime += timeDif
					}
				}

				// Calculate rates
				stats := t.Stats()
				currentDownloadedSize := stats.BytesReadData.Int64()
				currentUploadedSize := stats.BytesWrittenData.Int64()

				downRates[rateCounterIdx] = float64(currentDownloadedSize-downloadedSize) / timeDif
				upRates[rateCounterIdx] = float64(currentUploadedSize-uploadedSize) / timeDif

				downloadedSize = currentDownloadedSize
				uploadedSize = currentUploadedSize

				t.downloadRate = average(downRates)
				t.uploadRate = average(upRates)

				if rateCounterIdx == lenRateCounter-1 {
					rateCounterIdx = 0
				} else {
					rateCounterIdx++
				}
			}()

		case <-t.closing:
			return
		}
	}
}

func (t *BTTorrent) GetOverallProgress() float64 {
	return float64(t.BytesCompleted()) / float64(t.Length()) * 100.0
}

func (t *BTTorrent) GetProgress() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return getFilesProgress(t.files...)
}

func getFilesProgress(file ...*BTFile) float64 {
	var total int64
	var completed int64
	for _, f := range file {
		if f.markedForDownload {
			total += f.Length()
			completed += f.bytesCompleted
		}
	}

	if total == 0 {
		return 0
	}

	progress := float64(completed) / float64(total) * 100.0
	if progress > 100 {
		progress = 100
	}

	return progress
}

func (t *BTTorrent) GetCheckingProgress() float64 {
	total := 0
	checking := 0

	for _, state := range t.PieceStateRuns() {
		if state.Length > 0 {
			total += state.Length
			if state.Checking {
				checking += state.Length
			}
		}
	}
	if total > 0 {
		return float64(total-checking) / float64(total) * 100.0
	}
	return 0
}

func (t *BTTorrent) GetState() TorrentStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.getState(t.files...)
}

func (t *BTTorrent) getState(file ...*BTFile) TorrentStatus {
	if t.isPaused {
		return StatusPaused
	} else if t.Info() == nil {
		return StatusQueued
	}

	if !t.downloadStarted {
		downloadStarted := false
		for _, state := range t.PieceStateRuns() {
			if state.Length == 0 {
				continue
			}
			if state.Checking {
				return StatusChecking
			} else if state.Partial {
				downloadStarted = true
			}
		}
		if downloadStarted {
			t.downloadStarted = true
		}
	}

	progress := getFilesProgress(file...)
	if t.Stats().ActivePeers == 0 && progress == 0 {
		return StatusFinding
	}
	buffering := false
	for _, f := range file {
		if f.isBuffering {
			if f.GetBufferingProgress() < 100 {
				buffering = true
			} else {
				f.isBuffering = false
				f.Download()
			}
		}
	}
	if buffering {
		return StatusBuffering
	}
	if progress < 100 {
		if t.downloadStarted {
			return StatusDownloading
		}
	} else {
		if t.service.clientConfig.Seed {
			return StatusSeeding
		}
		return StatusFinished
	}

	return StatusQueued
}

func (t *BTTorrent) Pause() {
	t.torrent.SetMaxEstablishedConns(0)
	t.isPaused = true
}

func (t *BTTorrent) Resume() {
	t.torrent.SetMaxEstablishedConns(t.service.clientConfig.EstablishedConnsPerTorrent)
	t.isPaused = false
}

func (t *BTTorrent) Drop(removeFiles bool) {
	log.Infof("Dropping torrent: %s", t.Name())

	close(t.closing)
	t.torrent.Drop()

	if removeFiles {
		go func() {
			// Try to delete in N attempts
			// this is because of opened handles on files which silently goes by
			// so we try until rm fails
			for i := 1; i <= 5; i++ {
				left := false
				for _, f := range t.Files() {
					path := filepath.Join(t.service.clientConfig.DataDir, f.Path())
					if _, err := os.Stat(path); err == nil {
						log.Infof("Deleting torrent file at %s", path)
						if errRm := os.Remove(path); errRm != nil {
							continue
						}
						left = true
					}
				}

				if left {
					time.Sleep(time.Duration(i) * time.Second)
				} else {
					return
				}
			}
		}()
	}
}

func (t *BTTorrent) AllStats() *BTTorrentStats {
	t.mu.Lock()
	defer t.mu.Unlock()

	stats := t.Stats()
	return &BTTorrentStats{
		TorrentStats:             stats,
		TotalSeeders:             stats.TotalPeers - stats.ActivePeers + stats.ConnectedSeeders - stats.PendingPeers,
		ConnectedIncompletePeers: stats.ActivePeers - stats.ConnectedSeeders,
		TotalIncompletePeers:     stats.TotalPeers - stats.ConnectedSeeders - stats.PendingPeers,
		DownloadRate:             t.downloadRate,
		UploadRate:               t.uploadRate,
		SeedingTime:              t.seedingTime,
		FinishedTime:             t.finishedTime,
		ActiveTime:               t.activeTime,
	}
}

func average(xs []float64) float64 {
	var total float64
	for _, v := range xs {
		total += v
	}
	return total / float64(len(xs))
}
