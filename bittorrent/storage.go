package bittorrent

import (
	"errors"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"io"
	"sync"
)

const (
	storageBufferSize = 20 * 1024 * 1024
)

type CachedStorage struct {
	storage.ClientImpl
	bufferSize int64
	len        int64
	torrents   map[metainfo.Hash]*CachedTorrentImpl
	counter    uint64
}

type CachedTorrentImpl struct {
	storage.TorrentImpl
	storage       *CachedStorage
	infoHash      metainfo.Hash
	mu            *sync.RWMutex
	pieces        []*CachedPiece
	bufferedPiece *CachedPiece
	readBuffer    []byte
}

type CachedPiece struct {
	storage.PieceImpl
	torrent   *CachedTorrentImpl
	buf       []byte
	length    int64
	lastCount uint64
}

func NewCachedStorage(st storage.ClientImpl, bufferSize int64) storage.ClientImpl {
	return &CachedStorage{
		ClientImpl: st,
		bufferSize: bufferSize,
		torrents:   make(map[metainfo.Hash]*CachedTorrentImpl),
		counter:    1,
	}
}

func (s *CachedStorage) flushOldest() (ok bool) {
	var piece *CachedPiece
	minCount := s.counter
	for _, t := range s.torrents {
		for _, p := range t.pieces {
			if p != nil && p.buf != nil && p.lastCount < minCount {
				minCount = p.lastCount
				piece = p
			}
		}
	}
	if ok = piece != nil; ok {
		//noinspection GoNilness
		piece.flush()
	}
	return
}

func (s *CachedStorage) availableSize() int64 {
	return s.bufferSize - s.len
}

func (s *CachedStorage) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	torrent, err := s.ClientImpl.OpenTorrent(info, infoHash)
	if err == nil {
		t := &CachedTorrentImpl{
			TorrentImpl: torrent,
			storage:     s,
			infoHash:    infoHash,
			mu:          &sync.RWMutex{},
			pieces:      make([]*CachedPiece, info.NumPieces()),
		}
		s.torrents[infoHash] = t
		return t, nil
	}
	return nil, err
}

func (s *CachedStorage) Close() error {
	for _, t := range s.torrents {
		t.mu.Lock()
		t.flush()
		t.mu.Unlock()
	}
	return s.ClientImpl.Close()
}

func (t *CachedTorrentImpl) Piece(p metainfo.Piece) storage.PieceImpl {
	index := p.Index()
	piece := t.pieces[index]

	if piece == nil {
		piece = &CachedPiece{
			PieceImpl: t.TorrentImpl.Piece(p),
			torrent:   t,
			length:    p.Length(),
		}
		t.pieces[index] = piece
	}

	return piece
}

func (t *CachedTorrentImpl) flush() {
	for _, p := range t.pieces {
		p.flush()
	}
}

func (t *CachedTorrentImpl) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.flush()
	delete(t.storage.torrents, t.infoHash)
	return t.TorrentImpl.Close()
}

func (sp *CachedPiece) getBufParameters(b []byte, off int64) (n int64, err error) {
	if off < 0 {
		err = errors.New("invalid offset: too low")
		return
	}
	n = int64(len(b))
	if n+off > sp.length {
		err = errors.New("too high offset/length")
	}
	return
}

func (sp *CachedPiece) ReadAt(b []byte, off int64) (n int, err error) {
	sp.torrent.mu.Lock()
	defer sp.torrent.mu.Unlock()

	if sp.buf == nil {
		if sp.torrent.bufferedPiece != sp {
			if int64(len(sp.torrent.readBuffer)) != sp.length {
				sp.torrent.readBuffer = make([]byte, sp.length)
			}
			if _, e := sp.PieceImpl.ReadAt(sp.torrent.readBuffer, 0); e == nil {
				sp.torrent.bufferedPiece = sp
			} else {
				// log.Warningf("Failed reading piece for buffering: %s", e.Error())
				return sp.PieceImpl.ReadAt(b, off)
			}
		}
		n = copy(b, sp.torrent.readBuffer[off:off+int64(len(b))])
	} else {
		n1, err := sp.getBufParameters(b, off)
		if err != nil {
			return 0, err
		}
		n = copy(b, sp.buf[off:off+n1])
	}
	return
}

func (sp *CachedPiece) WriteAt(b []byte, off int64) (n int, err error) {
	sp.torrent.mu.Lock()
	defer sp.torrent.mu.Unlock()

	if sp.buf == nil {
		for sp.length > sp.torrent.storage.availableSize() {
			if !sp.torrent.storage.flushOldest() {
				panic("no more pieces to flush")
			}
		}

		// If this is the piece being read buffered, clean it, as it is now buffered here
		if sp.torrent.bufferedPiece == sp {
			sp.torrent.bufferedPiece = nil
		}

		sp.buf = make([]byte, sp.length)
		sp.torrent.storage.len += sp.length
		if _, err := sp.PieceImpl.ReadAt(sp.buf, 0); err != nil && err != io.ErrUnexpectedEOF {
			log.Errorf("Failed reading saved piece data: %s", err.Error())
		}
	}

	if sp.lastCount != sp.torrent.storage.counter {
		sp.torrent.storage.counter++
		sp.lastCount = sp.torrent.storage.counter
	}

	n1, err := sp.getBufParameters(b, off)
	if err != nil {
		return 0, err
	}

	n = copy(sp.buf[off:off+n1], b)
	return
}

func (sp *CachedPiece) flush() {
	if sp.buf != nil {
		if _, err := sp.PieceImpl.WriteAt(sp.buf, 0); err != nil {
			log.Errorf("Failed flushing piece: %s", err.Error())
		}
		sp.buf = nil
		sp.torrent.storage.len -= sp.length
	}
}

func (sp *CachedPiece) Completion() storage.Completion {
	sp.torrent.mu.Lock()
	defer sp.torrent.mu.Unlock()

	sp.flush()
	return sp.PieceImpl.Completion()
}

func (sp *CachedPiece) MarkComplete() error {
	sp.torrent.mu.Lock()
	defer sp.torrent.mu.Unlock()

	sp.flush()
	return sp.PieceImpl.MarkComplete()
}

func NewCachedFile(baseDir string) storage.ClientImpl {
	return NewCachedStorage(storage.NewFile(baseDir), storageBufferSize)
}
