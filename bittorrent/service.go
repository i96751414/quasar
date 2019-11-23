package bittorrent

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	lt "github.com/anacrolix/torrent"
	"github.com/boltdb/bolt"
	"github.com/i96751414/quasar/broadcast"
	"github.com/i96751414/quasar/config"
	"github.com/i96751414/quasar/tmdb"
	"github.com/i96751414/quasar/util"
	"github.com/i96751414/quasar/xbmc"
	"github.com/op/go-logging"
)

const (
	Bucket = "BitTorrent"
)

const (
	Delete = iota
	Update
	RemoveFromLibrary
)

const (
	Remove = iota
	Active
)

var DefaultTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://tracker.coppersurfer.tk:6969/announce",
	"udp://tracker.leechers-paradise.org:6969/announce",
	"udp://tracker.openbittorrent.com:80/announce",
	"udp://public.popcorn-tracker.org:6969/announce",
	"udp://explodie.org:6969",
}

const (
	DownloadRateBurst = 1 << 20
	UploadRateBurst   = 256 << 10
)

const (
	DefaultConnectionsLimit = 200
)

type ProxyType int

const (
	ProxyTypeSocks4 ProxyType = iota
	ProxyTypeSocks5
	ProxyTypeSocks5Password
	ProxyTypeSocksHTTP
	ProxyTypeSocksHTTPPassword
	ProxyTypeI2PSAM
)

type ProxySettings struct {
	Type     ProxyType
	Port     int
	Hostname string
	Username string
	Password string
}

type BTConfiguration struct {
	SpoofUserAgent      int
	BufferSize          int
	MaxUploadRate       int
	MaxDownloadRate     int
	LimitAfterBuffering bool
	ConnectionsLimit    int
	SessionSave         int
	ShareRatioLimit     int
	SeedTimeRatioLimit  int
	SeedTimeLimit       int
	DisableDHT          bool
	DisableUPNP         bool
	EncryptionPolicy    int
	LowerListenPort     int
	UpperListenPort     int
	ListenInterfaces    string
	OutgoingInterfaces  string
	TunedStorage        bool
	DownloadPath        string
	TorrentsPath        string
	DisableBgProgress   bool
	CompletedMove       bool
	CompletedMoviesPath string
	CompletedShowsPath  string
	Proxy               *ProxySettings
}

type BTService struct {
	db                *bolt.DB
	client            *lt.Client
	config            *BTConfiguration
	log               *logging.Logger
	libtorrentLog     *logging.Logger
	alertsBroadcaster *broadcast.Broadcaster
	dialogProgressBG  *xbmc.DialogProgressBG
	clientConfig      *lt.ClientConfig
	SpaceChecked      map[string]bool
	MarkedToMove      int
	UserAgent         string
	closing           chan interface{}
	Torrents          []*BTTorrent
}

type DBItem struct {
	ID      int    `json:"id"`
	State   int    `json:"state"`
	Type    string `json:"type"`
	File    int    `json:"file"`
	ShowID  int    `json:"showid"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

type PlayingItem struct {
	DBItem      *DBItem
	WatchedTime float64
	Duration    float64
}

type ResumeFile struct {
	InfoHash string     `bencode:"info-hash"`
	Trackers [][]string `bencode:"trackers"`
}

type activeTorrent struct {
	torrentName  string
	downloadRate float64
	uploadRate   float64
	progress     int
}

func NewBTService(conf BTConfiguration, db *bolt.DB) *BTService {
	s := &BTService{
		db:                db,
		log:               logging.MustGetLogger("btservice"),
		libtorrentLog:     logging.MustGetLogger("libtorrent"),
		alertsBroadcaster: broadcast.NewBroadcaster(),
		SpaceChecked:      make(map[string]bool, 0),
		config:            &conf,
		closing:           make(chan interface{}),
	}

	if _, err := os.Stat(s.config.TorrentsPath); os.IsNotExist(err) {
		if err := os.Mkdir(s.config.TorrentsPath, 0755); err != nil {
			s.log.Error("Unable to create Torrents folder")
		}
	}

	err := s.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(Bucket))
		if err != nil {
			s.log.Error(err)
			xbmc.Notify("Quasar", err.Error(), config.AddonIcon())
			return err
		}
		return nil
	})
	if err != nil {
		s.log.Error(err)
	}

	s.configure()

	tmdb.CheckApiKey()

	go s.loadTorrentFiles()
	go s.downloadProgress()

	return s
}

func (s *BTService) Close() {
	s.log.Info("Stopping BT Services...")
	s.stopServices()
	close(s.closing)
	s.client.Close()
}

func (s *BTService) Reconfigure(config BTConfiguration) {
	s.stopServices()
	s.client.Close()
	s.config = &config
	s.configure()
	s.loadTorrentFiles()
}

func (s *BTService) configure() {
	s.Torrents = []*BTTorrent{}
	s.log.Info("Applying session settings...")

	var bep20 string
	if s.config.SpoofUserAgent > 0 {
		switch s.config.SpoofUserAgent {
		case 2:
			s.UserAgent = "libtorrent (Rasterbar) 1.1.0"
			bep20 = "-LT1100-"
		case 3:
			s.UserAgent = "BitTorrent/7.5.0"
			bep20 = "-BT7500-"
		case 4:
			s.UserAgent = "BitTorrent/7.4.3"
			bep20 = "-BT7430-"
		case 5:
			s.UserAgent = "µTorrent/3.4.9"
			bep20 = "-UT3490-"
		case 6:
			s.UserAgent = "µTorrent/3.2.0"
			bep20 = "-UT3200-"
		case 7:
			s.UserAgent = "µTorrent/2.2.1"
			bep20 = "-UT2210-"
		case 8:
			s.UserAgent = "Transmission/2.92"
			bep20 = "-TR2920-"
		case 9:
			s.UserAgent = "Deluge/1.3.6.0"
			bep20 = "-DG1360-"
		case 10:
			s.UserAgent = "Deluge/1.3.12.0"
			bep20 = "-DG1312-"
		case 11:
			s.UserAgent = "Vuze/5.7.3.0"
			bep20 = "-VZ5730-"
		default:
			s.UserAgent = lt.DefaultHTTPUserAgent
		}
	} else {
		s.UserAgent = util.UserAgent()
		bep20 = util.DefaultPeerID()
	}
	s.log.Infof("UserAgent: %s", s.UserAgent)

	s.clientConfig = lt.NewDefaultClientConfig()

	listenIP, listenIPv6, listenPort, err := util.GetListenAddr(
		s.config.ListenInterfaces, s.config.LowerListenPort, s.config.UpperListenPort)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}
	if listenIPv6 == nil {
		s.log.Infof("Using IPv4 '%s' on port '%d' (no IPv6)", listenIP, listenPort)
	} else {
		s.log.Infof("Using IPv4 '%s' and IPv6 '%s' on port '%d'", listenIP, listenPort)
	}

	s.clientConfig.DisableIPv6 = listenIPv6 == nil
	s.clientConfig.ListenHost = func(network string) string {
		// Check for tcp6 and udp6
		if strings.Contains(network, "6") {
			return *listenIPv6
		}
		return listenIP
	}
	s.clientConfig.ListenPort = listenPort

	s.clientConfig.DataDir = s.config.DownloadPath
	s.clientConfig.DefaultStorage = storage.NewFile(s.config.DownloadPath)
	if bep20 != "" {
		s.clientConfig.HTTPUserAgent = s.UserAgent
		s.clientConfig.Bep20 = bep20
	}

	s.clientConfig.Seed = s.config.SeedTimeLimit > 0
	s.clientConfig.NoUpload = s.config.SeedTimeLimit == 0

	if s.config.LimitAfterBuffering == false {
		if s.config.MaxDownloadRate > 0 {
			s.clientConfig.DownloadRateLimiter = rate.NewLimiter(rate.Limit(s.config.MaxDownloadRate), DownloadRateBurst)
		}
		if s.config.MaxUploadRate > 0 {
			s.clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(s.config.MaxUploadRate), UploadRateBurst)
		}
	}

	s.clientConfig.NoDHT = s.config.DisableDHT
	s.clientConfig.NoDefaultPortForwarding = s.config.DisableUPNP

	s.clientConfig.HeaderObfuscationPolicy.Preferred = s.config.EncryptionPolicy > 0
	s.clientConfig.HeaderObfuscationPolicy.RequirePreferred = s.config.EncryptionPolicy == 2

	if s.config.Proxy != nil {
		s.log.Info("Applying proxy settings...")
		switch s.config.Proxy.Type {
		case ProxyTypeSocks4:
			s.clientConfig.ProxyURL = fmt.Sprintf("socks4://%s:%d", s.config.Proxy.Hostname, s.config.Proxy.Port)
		case ProxyTypeSocks5:
			s.clientConfig.ProxyURL = fmt.Sprintf("socks5://%s:%d", s.config.Proxy.Hostname, s.config.Proxy.Port)
		case ProxyTypeSocks5Password:
			s.clientConfig.ProxyURL = fmt.Sprintf("socks5://%s:%s@%s:%d", s.config.Proxy.Username, s.config.Proxy.Password, s.config.Proxy.Hostname, s.config.Proxy.Port)
		case ProxyTypeSocksHTTP:
			s.clientConfig.ProxyURL = fmt.Sprintf("http://%s:%d", s.config.Proxy.Hostname, s.config.Proxy.Port)
		case ProxyTypeSocksHTTPPassword:
			s.clientConfig.ProxyURL = fmt.Sprintf("http://%s:%s@%s:%d", s.config.Proxy.Username, s.config.Proxy.Password, s.config.Proxy.Hostname, s.config.Proxy.Port)
		case ProxyTypeI2PSAM:
			s.clientConfig.ProxyURL = fmt.Sprintf("i2psam://%s:%d", s.config.Proxy.Hostname, s.config.Proxy.Port)
		}
	}

	if s.config.ConnectionsLimit > 0 {
		s.clientConfig.EstablishedConnsPerTorrent = s.config.ConnectionsLimit
	} else {
		s.clientConfig.EstablishedConnsPerTorrent = DefaultConnectionsLimit
	}

	setPlatformSpecificSettings(s.clientConfig)

	s.clientConfig.HalfOpenConnsPerTorrent = s.clientConfig.EstablishedConnsPerTorrent / 2
	s.clientConfig.TorrentPeersLowWater = s.clientConfig.EstablishedConnsPerTorrent
	s.clientConfig.TorrentPeersHighWater = s.clientConfig.EstablishedConnsPerTorrent * 10

	log.Debugf("BitClient config: %#v", s.clientConfig)
	if s.client, err = lt.NewClient(s.clientConfig); err != nil {
		// If client can't be created - we should panic
		log.Errorf("Error creating bit client: %#v", err)
		xbmc.Notify("Quasar", err.Error(), config.AddonIcon())
		os.Exit(1)
	} else {
		log.Debugf("Created bit client: %#v", s.client)
		log.Debugf("client listening on: %d", s.client.LocalPort())
	}
}

func (s *BTService) PrintConnTrackerStatus(w io.Writer) {
	s.clientConfig.ConnTracker.PrintStatus(w)
}

func (s *BTService) stopServices() {
	if s.dialogProgressBG != nil {
		s.dialogProgressBG.Close()
	}
	s.dialogProgressBG = nil
	xbmc.ResetRPC()
}

func (s *BTService) loadTorrentFiles() {
	pattern := filepath.Join(s.config.TorrentsPath, "*.torrent")
	files, _ := filepath.Glob(pattern)

	for _, torrentFile := range files {
		log.Infof("Loading torrent file %s", torrentFile)

		var err error
		var torrentHandle *lt.Torrent
		if torrentHandle, err = s.client.AddTorrentFromFile(torrentFile); err != nil || torrentHandle == nil {
			log.Errorf("Error adding torrent file for %s", torrentFile)
			if _, err := os.Stat(torrentFile); err == nil {
				if err := os.Remove(torrentFile); err != nil {
					log.Error(err)
				}
			}

			continue
		}

		torrent := NewBTTorrent(s, torrentHandle)
		s.Torrents = append(s.Torrents, torrent)

		go torrent.watch()
	}
}

func (s *BTService) AddTorrent(uri string) (*BTTorrent, error) {
	log.Infof("Adding torrent from %s", uri)

	if s.config.DownloadPath == "." {
		log.Warningf("Cannot add torrent since download path is not set")
		xbmc.Notify("Quasar", "LOCALIZE[30113]", config.AddonIcon())
		return nil, fmt.Errorf("download path is empty")
	}

	var err error
	var torrentHandle *lt.Torrent
	torrent := NewTorrent(uri)
	if torrent.IsMagnet() {
		if torrentHandle, err = s.client.AddMagnet(uri); err != nil {
			return nil, err
		} else if torrentHandle == nil {
			return nil, errors.New("could not add torrent")
		}
	} else {
		if strings.HasPrefix(uri, "http") {
			if err = torrent.Resolve(); err != nil {
				log.Warningf("Could not resolve torrent %s: %#v", uri, err)
				return nil, err
			}
		}

		log.Debugf("Adding torrent: %#v", torrent.URI)
		if torrentHandle, err = s.client.AddTorrentFromFile(torrent.URI); err != nil {
			log.Warningf("Could not add torrent %s: %#v", torrent.URI, err)
			return nil, err
		} else if torrentHandle == nil {
			return nil, errors.New("could not add torrent")
		}
	}

	log.Debugf("Making new torrent item with url = '%s'", uri)
	t := NewBTTorrent(s, torrentHandle)

	for _, tor := range s.Torrents {
		if tor.infoHash == t.infoHash {
			return nil, errors.New("torrent already added")
		}
	}
	s.Torrents = append(s.Torrents, t)

	go t.watch()

	return t, nil
}

func (s *BTService) RemoveTorrent(t *BTTorrent, removeFiles bool) bool {
	log.Debugf("Removing torrent: %s", t.Name())
	if t == nil {
		return false
	}

	for i, tor := range s.Torrents {
		if t == tor {
			s.Torrents = append(s.Torrents[:i], s.Torrents[i+1:]...)
			t.Drop(removeFiles)
			return true
		}
	}

	return false
}

func (s *BTService) downloadProgress() {
	rotateTicker := time.NewTicker(5 * time.Second)
	defer rotateTicker.Stop()

	pathChecked := make(map[string]bool)
	warnedMissing := make(map[string]bool)

	showNext := 0
	for {
		select {
		case <-rotateTicker.C:
			var totalDownloadRate float64
			var totalUploadRate float64
			var totalProgress int

			activeTorrents := make([]*activeTorrent, 0)

			for i, torrentHandle := range s.Torrents {
				if torrentHandle == nil {
					continue
				}

				info := torrentHandle.Info()
				if info == nil {
					continue
				}

				torrentName := info.Name
				progress := int(torrentHandle.GetProgress())
				stats := torrentHandle.AllStats()
				seeded := false

				downloadRate := stats.DownloadRate / 1024
				uploadRate := stats.UploadRate / 1024
				totalDownloadRate += downloadRate
				totalUploadRate += uploadRate

				if progress < 100 && !torrentHandle.isPaused {
					activeTorrents = append(activeTorrents, &activeTorrent{
						torrentName:  torrentName,
						downloadRate: downloadRate,
						uploadRate:   uploadRate,
						progress:     progress,
					})
					totalProgress += progress
					continue
				}

				seedingTime := stats.SeedingTime
				finishedTime := stats.FinishedTime
				if progress == 100 && seedingTime == 0 {
					seedingTime = finishedTime
				}

				if s.config.SeedTimeLimit > 0 {
					if seedingTime >= float64(s.config.SeedTimeLimit) {
						if !torrentHandle.isPaused {
							s.log.Warningf("Seeding time limit reached, pausing %s", torrentName)
							torrentHandle.Pause()
						}
						seeded = true
					}
				}
				if s.config.SeedTimeRatioLimit > 0 {
					timeRatio := 0
					downloadTime := stats.ActiveTime - seedingTime
					if downloadTime > 1 {
						timeRatio = int(seedingTime * 100 / downloadTime)
					}
					if timeRatio >= s.config.SeedTimeRatioLimit {
						if !torrentHandle.isPaused {
							s.log.Warningf("Seeding time ratio reached, pausing %s", torrentName)
							torrentHandle.Pause()
						}
						seeded = true
					}
				}
				if s.config.ShareRatioLimit > 0 {
					ratio := int64(0)
					stats := torrentHandle.AllStats()
					allTimeDownload := stats.BytesReadData.Int64()
					if allTimeDownload > 0 {
						ratio = stats.BytesWrittenData.Int64() * 100 / allTimeDownload
					}
					if ratio >= int64(s.config.ShareRatioLimit) {
						if !torrentHandle.isPaused {
							s.log.Warningf("Share ratio reached, pausing %s", torrentName)
							torrentHandle.Pause()
						}
						seeded = true
					}
				}
				if s.MarkedToMove >= 0 && i == s.MarkedToMove {
					s.MarkedToMove = -1
					seeded = true
				}

				//
				// Handle moving completed downloads
				//
				if !s.config.CompletedMove || !seeded || Playing {
					continue
				}
				if xbmc.PlayerIsPlaying() {
					continue
				}

				infoHash := torrentHandle.infoHash
				if _, exists := warnedMissing[infoHash]; exists {
					continue
				}

				s.db.View(func(tx *bolt.Tx) error {
					b := tx.Bucket([]byte(Bucket))
					v := b.Get([]byte(infoHash))
					var item *DBItem
					errMsg := fmt.Sprintf("Missing item type to move files to completed folder for %s", torrentName)
					if err := json.Unmarshal(v, &item); err != nil {
						s.log.Warning(errMsg)
						warnedMissing[infoHash] = true
						return err
					}
					if item.Type == "" {
						s.log.Error(errMsg)
						return errors.New(errMsg)
					} else {
						s.log.Warning(torrentName, "finished seeding, moving files...")

						// Check paths are valid and writable, and only once
						if _, exists := pathChecked[item.Type]; !exists {
							if item.Type == "movie" {
								if err := config.IsWritablePath(s.config.CompletedMoviesPath); err != nil {
									warnedMissing[infoHash] = true
									pathChecked[item.Type] = true
									s.log.Error(err)
									return err
								}
								pathChecked[item.Type] = true
							} else {
								if err := config.IsWritablePath(s.config.CompletedShowsPath); err != nil {
									warnedMissing[infoHash] = true
									pathChecked[item.Type] = true
									s.log.Error(err)
									return err
								}
								pathChecked[item.Type] = true
							}
						}

						s.log.Info("Removing the torrent without deleting files...")
						s.RemoveTorrent(torrentHandle, false)

						// Delete leftover .parts file if any
						partsFile := filepath.Join(config.Get().DownloadPath, fmt.Sprintf(".%s.parts", infoHash))
						os.Remove(partsFile)

						// Delete torrent file
						torrentFile := filepath.Join(s.config.TorrentsPath, fmt.Sprintf("%s.torrent", infoHash))
						if _, err := os.Stat(torrentFile); err == nil {
							s.log.Info("Deleting torrent file at", torrentFile)
							if err := os.Remove(torrentFile); err != nil {
								s.log.Error(err)
								return err
							}
						}

						// Delete fast resume data
						fastResumeFile := filepath.Join(s.config.TorrentsPath, fmt.Sprintf("%s.fastresume", infoHash))
						if _, err := os.Stat(fastResumeFile); err == nil {
							s.log.Info("Deleting fast resume data at", fastResumeFile)
							if err := os.Remove(fastResumeFile); err != nil {
								s.log.Error(err)
								return err
							}
						}

						filePath := torrentHandle.Files()[item.File].Path()
						fileName := filepath.Base(filePath)

						extracted := ""
						re := regexp.MustCompile("(?i).*\\.rar")
						if re.MatchString(fileName) {
							extractedPath := filepath.Join(s.config.DownloadPath, filepath.Dir(filePath), "extracted")
							files, err := ioutil.ReadDir(extractedPath)
							if err != nil {
								return err
							}
							if len(files) == 1 {
								extracted = files[0].Name()
							} else {
								for _, file := range files {
									fileName := file.Name()
									re := regexp.MustCompile("(?i).*\\.(mkv|mp4|mov|avi)")
									if re.MatchString(fileName) {
										extracted = fileName
										break
									}
								}
							}
							if extracted != "" {
								filePath = filepath.Join(filepath.Dir(filePath), "extracted", extracted)
							} else {
								return errors.New("no extracted file to move")
							}
						}

						var dstPath string
						if item.Type == "movie" {
							dstPath = filepath.Dir(s.config.CompletedMoviesPath)
						} else {
							dstPath = filepath.Dir(s.config.CompletedShowsPath)
							if item.ShowID > 0 {
								show := tmdb.GetShow(item.ShowID, "en")
								if show != nil {
									showPath := util.ToFileName(fmt.Sprintf("%s (%s)", show.Name, strings.Split(show.FirstAirDate, "-")[0]))
									seasonPath := filepath.Join(showPath, fmt.Sprintf("Season %d", item.Season))
									if item.Season == 0 {
										seasonPath = filepath.Join(showPath, "Specials")
									}
									dstPath = filepath.Join(dstPath, seasonPath)
									os.MkdirAll(dstPath, 0755)
								}
							}
						}

						go func() {
							s.log.Infof("Moving %s to %s", fileName, dstPath)
							srcPath := filepath.Join(s.config.DownloadPath, filePath)
							if dst, err := util.Move(srcPath, dstPath); err != nil {
								s.log.Error(err)
							} else {
								// Remove leftover folders
								if dirPath := filepath.Dir(filePath); dirPath != "." {
									os.RemoveAll(filepath.Dir(srcPath))
									if extracted != "" {
										parentPath := filepath.Clean(filepath.Join(filepath.Dir(srcPath), ".."))
										if parentPath != "." && parentPath != s.config.DownloadPath {
											os.RemoveAll(parentPath)
										}
									}
								}
								s.log.Warning(fileName, "moved to", dst)

								s.log.Infof("Marking %s for removal from library and database...", torrentName)
								s.UpdateDB(RemoveFromLibrary, infoHash, 0, "")
							}
						}()
					}
					return nil
				})
			}

			totalActive := len(activeTorrents)
			if totalActive > 0 {
				var showProgress int
				var showTorrent string
				if showNext == totalActive {
					showNext = 0
					showProgress = totalProgress / totalActive
					showTorrent = fmt.Sprintf("Total - D/L: %.2f kB/s - U/L: %.2f kB/s", totalDownloadRate, totalUploadRate)
				} else {
					showProgress = activeTorrents[showNext].progress
					torrentName := activeTorrents[showNext].torrentName
					if len(torrentName) > 30 {
						torrentName = torrentName[:30] + "..."
					}
					showTorrent = fmt.Sprintf("%s - %.2f kB/s - %.2f kB/s", torrentName, activeTorrents[showNext].downloadRate, activeTorrents[showNext].uploadRate)
					showNext++
				}
				if !s.config.DisableBgProgress {
					if s.dialogProgressBG == nil {
						s.dialogProgressBG = xbmc.NewDialogProgressBG("Quasar", "")
					}
					s.dialogProgressBG.Update(showProgress, "Quasar", showTorrent)
				}
			} else if !s.config.DisableBgProgress && s.dialogProgressBG != nil {
				s.dialogProgressBG.Close()
				s.dialogProgressBG = nil
			}
		}
	}
}

//
// Database updates
//
func (s *BTService) UpdateDB(Operation int, InfoHash string, ID int, Type string, infos ...int) (err error) {
	switch Operation {
	case Delete:
		err = s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(Bucket))
			if err := b.Delete([]byte(InfoHash)); err != nil {
				return err
			}
			return nil
		})
	case Update:
		err = s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(Bucket))
			item := DBItem{
				State:   Active,
				ID:      ID,
				Type:    Type,
				File:    infos[0],
				ShowID:  infos[1],
				Season:  infos[2],
				Episode: infos[3],
			}
			if buf, err := json.Marshal(item); err != nil {
				return err
			} else if err := b.Put([]byte(InfoHash), buf); err != nil {
				return err
			}
			return nil
		})
	case RemoveFromLibrary:
		err = s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(Bucket))
			v := b.Get([]byte(InfoHash))
			var item *DBItem
			if err := json.Unmarshal(v, &item); err != nil {
				s.log.Error(err)
				return err
			}
			item.State = Remove
			if buf, err := json.Marshal(item); err != nil {
				return err
			} else if err := b.Put([]byte(InfoHash), buf); err != nil {
				return err
			}
			return nil
		})
	}
	return err
}

func (s *BTService) GetDBItem(infoHash string) (dbItem *DBItem) {
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(Bucket))
		v := b.Get([]byte(infoHash))
		if err := json.Unmarshal(v, &dbItem); err != nil {
			return err
		}
		return nil
	})
	return
}
