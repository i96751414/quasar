package bittorrent

import (
	"bytes"
	lt "github.com/anacrolix/torrent"
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
	*lt.Torrent
	service         *BTService
	infoHash        string
	closing         chan struct{}
	mu              *sync.RWMutex
	chosenFiles     []*lt.File
	bufferFiles     []*lt.File
	lastUpdateTime  time.Time
	bufferPieces    []int
	bufferSize      int64
	downloadRate    float64
	uploadRate      float64
	seedingTime     float64
	finishedTime    float64
	activeTime      float64
	isPaused        bool
	isBuffering     bool
	downloadStarted bool
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
		Torrent:  handle,
		service:  service,
		infoHash: handle.InfoHash().HexString(),
		closing:  make(chan struct{}),
		mu:       &sync.RWMutex{},
	}

	return t
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
	downRates := []float64{0, 0, 0, 0, 0}
	upRates := []float64{0, 0, 0, 0, 0}
	rateCounter := 0
	lastRateCounterIdx := len(downRates) - 1

	progressTicker := time.NewTicker(1 * time.Second)
	defer progressTicker.Stop()

	for {
		select {
		case <-progressTicker.C:
			go func() {
				t.mu.Lock()
				defer t.mu.Unlock()

				timeNow := time.Now()
				timeDif := timeNow.Sub(t.lastUpdateTime).Seconds()
				t.lastUpdateTime = timeNow

				// Update times
				status := t.getState()
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

				downRates[rateCounter] = float64(currentDownloadedSize-downloadedSize) / timeDif
				upRates[rateCounter] = float64(currentUploadedSize-uploadedSize) / timeDif

				downloadedSize = currentDownloadedSize
				uploadedSize = currentUploadedSize

				t.downloadRate = average(downRates)
				t.uploadRate = average(upRates)

				if rateCounter == lastRateCounterIdx {
					rateCounter = 0
				} else {
					rateCounter++
				}
			}()

		case <-t.closing:
			return
		}
	}
}
func (t *BTTorrent) GetBufferingProgress() float64 {
	var missingLength int64
	for _, piece := range t.bufferPieces {
		missingLength += t.PieceBytesMissing(piece)
	}

	return float64(t.bufferSize-missingLength) / float64(t.bufferSize) * 100.0
}

func (t *BTTorrent) GetCheckingProgress() float64 {
	total := 0
	checking := 0
	var progress float64

	for _, state := range t.PieceStateRuns() {
		if state.Length > 0 {
			total += state.Length
			if state.Checking {
				checking += state.Length
			}
		}
	}
	if total > 0 {
		progress = float64(total-checking) / float64(total) * 100.0
	}
	return progress
}

func (t *BTTorrent) DownloadFile(file *lt.File) {
	if file == nil {
		log.Debug("Received a null file")
		return
	}
	for _, f := range t.chosenFiles {
		if f == file {
			return
		}
	}

	t.chosenFiles = append(t.chosenFiles, file)
	log.Debugf("Choosing file for download: %s", file.DisplayPath())
	file.SetPriority(lt.PiecePriorityNormal)
}

func (t *BTTorrent) Buffer(file *lt.File, startBufferSize, endBufferSize int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if file == nil {
		log.Debug("Received a null file")
		return
	}

	for _, f := range t.bufferFiles {
		if f == file {
			return
		}
	}

	t.bufferFiles = append(t.bufferFiles, file)
	bufferSize := startBufferSize + endBufferSize

	if file.Length() >= bufferSize {
		aFirstPieceIndex, aEndPieceIndex := t.getBufferPieces(file, 0, startBufferSize)
		for idx := aFirstPieceIndex; idx <= aEndPieceIndex; idx++ {
			piece := t.Piece(int(idx))
			piece.SetPriority(lt.PiecePriorityNow)
			t.bufferSize += piece.Info().Length()
			t.bufferPieces = append(t.bufferPieces, int(idx))
		}

		bFirstPieceIndex, bEndPieceIndex := t.getBufferPieces(file, file.Length()-endBufferSize, endBufferSize)
		for idx := bFirstPieceIndex; idx <= bEndPieceIndex; idx++ {
			piece := t.Piece(int(idx))
			piece.SetPriority(lt.PiecePriorityNow)
			t.bufferSize += piece.Info().Length()
			t.bufferPieces = append(t.bufferPieces, int(idx))
		}
	} else {
		firstPieceIndex, endPieceIndex := t.getBufferPieces(file, 0, file.Length())
		for idx := firstPieceIndex; idx <= endPieceIndex; idx++ {
			piece := t.Piece(int(idx))
			piece.SetPriority(lt.PiecePriorityNow)
			t.bufferSize += piece.Info().Length()
			t.bufferPieces = append(t.bufferPieces, int(idx))
		}
	}

	t.isBuffering = true
}

func (t *BTTorrent) getBufferPieces(file *lt.File, off, length int64) (firstPieceIndex, endPieceIndex int64) {
	if off < 0 {
		off = 0
	}
	end := off + length
	if end > file.Length() {
		end = file.Length()
	}
	firstPieceIndex = (file.Offset() + off) * int64(t.NumPieces()) / t.Length()
	endPieceIndex = (file.Offset() + end) * int64(t.NumPieces()) / t.Length()
	return
}

func (t *BTTorrent) GetState() TorrentStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.getState()
}

func (t *BTTorrent) getState() TorrentStatus {
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

	progress := t.GetProgress()
	if t.Stats().ActivePeers == 0 && progress == 0 {
		return StatusFinding
	}
	if t.isBuffering {
		if t.GetBufferingProgress() < 100 {
			return StatusBuffering
		} else {
			t.isBuffering = false
		}
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

func (t *BTTorrent) GetProgress() float64 {
	var total int64
	for _, f := range t.chosenFiles {
		total += f.Length()
	}

	if total == 0 {
		return 0
	}

	progress := float64(t.BytesCompleted()) / float64(total) * 100.0
	if progress > 100 {
		progress = 100
	}

	return progress
}

func (t *BTTorrent) Pause() {
	t.SetMaxEstablishedConns(0)
	t.isPaused = true
}

func (t *BTTorrent) Resume() {
	t.SetMaxEstablishedConns(t.service.clientConfig.EstablishedConnsPerTorrent)
	t.isPaused = false
}

func (t *BTTorrent) Drop(removeFiles bool) {
	log.Infof("Dropping torrent: %s", t.Name())
	var files []string
	for _, f := range t.Files() {
		files = append(files, f.Path())
	}

	close(t.closing)
	t.Torrent.Drop()

	if removeFiles {
		go func() {
			// Try to delete in N attempts
			// this is because of opened handles on files which silently goes by
			// so we try until rm fails
			for i := 1; i <= 5; i++ {
				left := false
				for _, f := range files {
					path := filepath.Join(t.service.clientConfig.DataDir, f)
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
