package bittorrent

import (
	"fmt"
	"github.com/anacrolix/missinggo/bitmap"
	lt "github.com/anacrolix/torrent"
	"github.com/dustin/go-humanize"
	"io"
	"sync"
	"time"
)

const (
	sdPieceTicker      = 200             // milliseconds
	sdPieceBufferSize  = 5 * 1024 * 1024 // 5 MB
	sdReaderBufferSize = 256 * 1024      // 256 KB
)

type SequentialDownloader interface {
	Start()
	IsDownloading() bool
	Seek(offset int64) error
}

type SequentialReader struct {
	lt.Reader
	sd *SequentialDownloader
}

func (sr *SequentialReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := sr.Reader.Seek(offset, whence)
	if err == nil && *sr.sd != nil && (*sr.sd).IsDownloading() {
		if e := (*sr.sd).Seek(pos); e != nil {

		}
	}
	return pos, err
}

// Our own implementation of a sequential downloader
type SequentialPieceDownloader struct {
	mu                   *sync.RWMutex
	torrent              *BTTorrent
	pieceLength          int64
	firstPieceIndex      int
	lastPieceIndex       int
	numPieces            int
	numPiecesCompleted   int
	completedPieces      bitmap.Bitmap
	downloadingPieces    bitmap.Bitmap
	maxPiecesDownloading int
	pos                  int
	posChanged           bool
	totalSize            int64
	isDownloading        bool
}

func NewSequentialPieceDownloader(torrent *BTTorrent, firstPieceIndex, lastPieceIndex int) SequentialDownloader {
	pieceLength := torrent.Info().PieceLength
	numPieces := lastPieceIndex - firstPieceIndex + 1
	maxPiecesDownloading := int(sdPieceBufferSize / pieceLength)
	if maxPiecesDownloading > numPieces {
		maxPiecesDownloading = numPieces
	}

	return &SequentialPieceDownloader{
		mu:                   &sync.RWMutex{},
		torrent:              torrent,
		pieceLength:          pieceLength,
		firstPieceIndex:      firstPieceIndex,
		lastPieceIndex:       lastPieceIndex,
		numPieces:            numPieces,
		maxPiecesDownloading: maxPiecesDownloading,
		pos:                  firstPieceIndex,
		totalSize:            pieceLength * int64(numPieces),
	}
}

func (sd *SequentialPieceDownloader) Start() {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	if !sd.isDownloading {
		go sd.sequentialDownload()
		sd.isDownloading = true
	}
}

func (sd *SequentialPieceDownloader) Seek(offset int64) (err error) {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	pos := sd.firstPieceIndex + int(offset/sd.pieceLength)
	if pos <= sd.lastPieceIndex {
		sd.pos = pos
	} else {
		err = fmt.Errorf("trying to seek with offset %d, but the maximum allowed is %d", offset, sd.totalSize)
	}
	return err
}

func (sd *SequentialPieceDownloader) IsDownloading() bool {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.isDownloading
}

func (sd *SequentialPieceDownloader) setDownloading(isDownloading bool) {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.isDownloading = isDownloading
}

func (sd *SequentialPieceDownloader) sequentialDownload() {
	start := time.Now()
	ticker := time.NewTicker(sdPieceTicker * time.Millisecond)

	defer sd.setDownloading(false)

	for {
		select {
		case <-sd.torrent.Closed():
			log.Infof("Torrent closed. Stopping sequential downloader")
			return
		case <-ticker.C:
			downloadingPiecesCount := 0
			sd.downloadingPieces.Copy().IterTyped(func(piece int) bool {
				if sd.torrent.torrent.PieceState(piece).Complete {
					sd.completedPieces.Add(piece)
					sd.downloadingPieces.Remove(piece)
					sd.numPiecesCompleted++
				} else {
					downloadingPiecesCount++
				}
				return true
			})

			if sd.numPiecesCompleted == sd.numPieces {
				end := time.Since(start)
				log.Infof("Sequential download completed in %s after downloading %s at an average of %s/s",
					end, humanize.IBytes(uint64(sd.totalSize)), humanize.IBytes(uint64(float64(sd.totalSize)/end.Seconds())))
				return
			}

			pos := sd.pos
			// In case seek happened, re read it
			if sd.posChanged {
				sd.mu.Lock()
				pos = sd.pos
				sd.posChanged = false
				sd.mu.Unlock()
			}

			downloaderPos := pos
			shouldIncrement := true

			for downloadingPiecesCount < sd.maxPiecesDownloading && downloadingPiecesCount+sd.numPiecesCompleted < sd.numPieces {
				if !sd.completedPieces.Get(downloaderPos) {
					shouldIncrement = false
					if !sd.downloadingPieces.Get(downloaderPos) {
						sd.downloadingPieces.Add(downloaderPos)
						sd.torrent.torrent.Piece(downloaderPos).SetPriority(lt.PiecePriorityHigh)
						downloadingPiecesCount++
					}
				}
				if downloaderPos == sd.lastPieceIndex {
					downloaderPos = sd.firstPieceIndex
				} else {
					downloaderPos++
				}
				if shouldIncrement {
					pos = downloaderPos
				}
			}

			sd.mu.Lock()
			if !sd.posChanged {
				sd.pos = pos
			}
			sd.mu.Unlock()
		}
	}
}

// Sequential Downloader using Readers
type Downloader interface {
	Length() int64
	BytesCompleted() int64
	NewReader() lt.Reader
}

type SequentialReaderDownloader struct {
	sdReader      lt.Reader
	downloader    Downloader
	mu            *sync.RWMutex
	posChanged    bool
	isDownloading bool
}

func NewSequentialReaderDownloader(downloader Downloader) SequentialDownloader {
	reader := downloader.NewReader()
	reader.SetReadahead(downloader.Length() / 100)

	return &SequentialReaderDownloader{
		sdReader:   reader,
		downloader: downloader,
		mu:         &sync.RWMutex{},
	}
}

func (sd *SequentialReaderDownloader) Start() {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	if !sd.isDownloading {
		buf := make([]byte, sdReaderBufferSize)
		go sd.sequentialDownload(buf)
		sd.isDownloading = true
	}
}

func (sd *SequentialReaderDownloader) Seek(offset int64) error {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	_, err := sd.sdReader.Seek(offset, io.SeekStart)
	if err == nil {
		sd.posChanged = true
	}
	return err
}

/*func (sd *SequentialReaderDownloader) Stop() {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	if sd.isDownloading {
		sd.isDownloading = false
	}
}*/

func (sd *SequentialReaderDownloader) IsDownloading() bool {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.isDownloading
}

func (sd *SequentialReaderDownloader) setDownloading(isDownloading bool) {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.isDownloading = isDownloading
}

func (sd *SequentialReaderDownloader) sequentialDownload(buf []byte) {
	defer sd.setDownloading(false)
	start := time.Now()

	for sd.downloader.Length() > sd.downloader.BytesCompleted() {
		sd.posChanged = false
		_, err := sd.sdReader.Read(buf)
		if err != nil {
			if err == io.EOF {
				sd.mu.Lock()
				if !sd.posChanged {
					_, _ = sd.sdReader.Seek(0, io.SeekStart)
				}
				sd.mu.Unlock()
			} else {
				log.Errorf("Sequential download error: %s", err.Error())
				return
			}
		}
	}
	end := time.Since(start)
	log.Infof("Sequential download completed in %s after downloading %s at an average of %s/s",
		end, humanize.IBytes(uint64(sd.downloader.Length())), humanize.IBytes(uint64(float64(sd.downloader.Length())/end.Seconds())))
}
