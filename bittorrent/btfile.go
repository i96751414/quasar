package bittorrent

import (
	lt "github.com/anacrolix/torrent"
)

type BTFile struct {
	file                 *lt.File
	t                    *BTTorrent
	bufferPieces         []int
	bufferSize           int64
	markedForDownload    bool
	isBuffering          bool
	bytesCompleted       int64
	bufferBytesMissing   int64
	sequentialDownloader *SequentialDownloader
}

func NewBTFile(f *lt.File, t *BTTorrent) *BTFile {
	return &BTFile{
		file: f,
		t:    t,
	}
}

func (f *BTFile) Torrent() *BTTorrent {
	return f.t
}

func (f *BTFile) NewReader() lt.Reader {
	return f.file.NewReader()
}

func (f *BTFile) State() []lt.FilePieceState {
	return f.file.State()
}

func (f *BTFile) Path() string {
	return f.file.Path()
}

func (f *BTFile) DisplayPath() string {
	return f.file.DisplayPath()
}

func (f *BTFile) Length() int64 {
	return f.file.Length()
}

func (f *BTFile) Offset() int64 {
	return f.file.Offset()
}

func (f *BTFile) BytesCompleted() int64 {
	return f.bytesCompleted
}

func (f *BTFile) NewSequentialReader() *SequentialReader {
	return &SequentialReader{
		Reader: f.file.NewReader(),
		sd:     &f.sequentialDownloader,
	}
}

func (f *BTFile) Download() {
	log.Debugf("Choosing file for download: %s", f.DisplayPath())
	f.markedForDownload = true
	f.file.SetPriority(lt.PiecePriorityNormal)
}

func (f *BTFile) SequentialDownload() {
	log.Debugf("Choosing file for sequential download: %s", f.DisplayPath())
	f.markedForDownload = true
	if f.sequentialDownloader == nil {
		f.sequentialDownloader = NewSequentialDownloader(f)
	}
	f.sequentialDownloader.Start()
}

func (f *BTFile) BufferAndDownload(startBufferSize, endBufferSize int64) {
	f.bufferSize = 0
	f.bufferPieces = nil
	bufferSize := startBufferSize + endBufferSize

	if f.Length() >= bufferSize {
		aFirstPieceIndex, aEndPieceIndex := f.getPiecesIndexes(0, startBufferSize)
		for idx := aFirstPieceIndex; idx <= aEndPieceIndex; idx++ {
			piece := f.t.torrent.Piece(idx)
			piece.SetPriority(lt.PiecePriorityNow)
			f.bufferSize += piece.Info().Length()
			f.bufferPieces = append(f.bufferPieces, idx)
		}

		bFirstPieceIndex, bEndPieceIndex := f.getPiecesIndexes(f.Length()-endBufferSize, endBufferSize)
		for idx := bFirstPieceIndex; idx <= bEndPieceIndex; idx++ {
			piece := f.t.torrent.Piece(idx)
			piece.SetPriority(lt.PiecePriorityNow)
			f.bufferSize += piece.Info().Length()
			f.bufferPieces = append(f.bufferPieces, idx)
		}
	} else {
		firstPieceIndex, endPieceIndex := f.getPiecesIndexes(0, f.Length())
		for idx := firstPieceIndex; idx <= endPieceIndex; idx++ {
			piece := f.t.torrent.Piece(idx)
			piece.SetPriority(lt.PiecePriorityNow)
			f.bufferSize += piece.Info().Length()
			f.bufferPieces = append(f.bufferPieces, idx)
		}
	}

	f.isBuffering = true
	f.markedForDownload = true
}

func (f *BTFile) getPiecesIndexes(off, length int64) (firstPieceIndex, endPieceIndex int) {
	if off < 0 {
		off = 0
	}
	end := off + length
	if end > f.Length() {
		end = f.Length()
	}
	pieceLength := f.t.Info().PieceLength
	firstPieceIndex = int((f.Offset() + off) / pieceLength)
	endPieceIndex = int((f.Offset() + end) / pieceLength)
	return
}

func (f *BTFile) updateStats() {
	/*f.bytesCompleted = 0
	for _, p := range f.State() {
		if p.Complete {
			f.bytesCompleted += p.Bytes
		}
	}*/

	f.bytesCompleted = f.file.BytesCompleted()
	if f.isBuffering {
		f.bufferBytesMissing = 0
		for _, piece := range f.bufferPieces {
			f.bufferBytesMissing += f.t.PieceBytesMissing(piece)
		}
		if f.bufferBytesMissing == 0 {
			f.isBuffering = false
			f.Download()
		}
	}
}

func (f *BTFile) GetBufferingProgress() float64 {
	if f.bufferSize == 0 {
		return 0
	}
	return float64(f.bufferSize-f.bufferBytesMissing) / float64(f.bufferSize) * 100.0
}

func (f *BTFile) GetProgress() float64 {
	return getFilesProgress(f)
}

func (f *BTFile) GetState() TorrentStatus {
	f.t.mu.Lock()
	defer f.t.mu.Unlock()
	return f.t.getState(f)
}
