package bittorrent

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/time/rate"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/i96751414/quasar/broadcast"
	"github.com/i96751414/quasar/config"
	"github.com/i96751414/quasar/diskusage"
	"github.com/i96751414/quasar/trakt"
	"github.com/i96751414/quasar/xbmc"
	"github.com/op/go-logging"
)

const (
	startBufferPercent = 0.005
	endBufferSize      = 10 * 1024 * 1024 // 10m
	playbackMaxWait    = 20 * time.Second
	minCandidateSize   = 100 * 1024 * 1024
)

var (
	Paused        bool
	Seeked        bool
	Playing       bool
	WasPlaying    bool
	FromLibrary   bool
	WatchedTime   float64
	VideoDuration float64
)

type BTPlayer struct {
	bts                  *BTService
	log                  *logging.Logger
	dialogProgress       *xbmc.DialogProgress
	overlayStatus        *xbmc.OverlayStatus
	uri                  string
	torrentFile          string
	contentType          string
	fileIndex            int
	resumeIndex          int
	tmdbId               int
	showId               int
	season               int
	episode              int
	scrobble             bool
	deleteAfter          bool
	askToDelete          bool
	askToKeepDownloading bool
	overlayStatusEnabled bool
	torrentHandle        *BTTorrent
	chosenFile           *BTFile
	subtitlesFile        *BTFile
	fileSize             int64
	fileName             string
	torrentName          string
	extracted            string
	isRarArchive         bool
	isDownloading        bool
	notEnoughSpace       bool
	diskStatus           *diskusage.DiskStatus
	bufferEvents         *broadcast.Broadcaster
	closing              chan interface{}
}

type BTPlayerParams struct {
	URI         string
	FileIndex   int
	ResumeIndex int
	FromLibrary bool
	ContentType string
	TMDBId      int
	ShowID      int
	Season      int
	Episode     int
}

type candidateFile struct {
	Index    int
	Filename string
}

type byFilename []*candidateFile

func (a byFilename) Len() int           { return len(a) }
func (a byFilename) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byFilename) Less(i, j int) bool { return a[i].Filename < a[j].Filename }

var rarPattern = regexp.MustCompile("(?i).*\\.rar")

func NewBTPlayer(bts *BTService, params BTPlayerParams) *BTPlayer {
	Playing = true
	if params.FromLibrary {
		FromLibrary = true
	}
	btp := &BTPlayer{
		log:                  logging.MustGetLogger("btplayer"),
		bts:                  bts,
		uri:                  params.URI,
		fileIndex:            params.FileIndex,
		resumeIndex:          params.ResumeIndex,
		fileSize:             0,
		fileName:             "",
		overlayStatusEnabled: config.Get().EnableOverlayStatus == true,
		askToKeepDownloading: config.Get().BackgroundHandling == false,
		deleteAfter:          config.Get().KeepFilesAfterStop == false,
		askToDelete:          config.Get().KeepFilesAsk == true,
		scrobble:             config.Get().Scrobble == true && params.TMDBId > 0 && config.Get().TraktToken != "",
		contentType:          params.ContentType,
		tmdbId:               params.TMDBId,
		showId:               params.ShowID,
		season:               params.Season,
		episode:              params.Episode,
		torrentFile:          "",
		isDownloading:        false,
		notEnoughSpace:       false,
		closing:              make(chan interface{}),
		bufferEvents:         broadcast.NewBroadcaster(),
	}
	return btp
}

func (btp *BTPlayer) PlayURL() string {
	chosenFile := btp.chosenFile.Path()
	if btp.isRarArchive {
		extractedPath := filepath.Join(filepath.Dir(chosenFile), "extracted", btp.extracted)
		return strings.Join(strings.Split(extractedPath, string(os.PathSeparator)), "/")
	} else {
		return strings.Join(strings.Split(chosenFile, string(os.PathSeparator)), "/")
	}
}

func (btp *BTPlayer) Buffer() error {
	var err error
	if btp.resumeIndex >= 0 {
		if btp.resumeIndex >= len(btp.bts.Torrents) {
			return fmt.Errorf("unable to resume torrent with index %d", btp.resumeIndex)
		}
		btp.torrentHandle = btp.bts.Torrents[btp.resumeIndex]
		btp.torrentHandle.Resume()
	} else {
		if status, err := diskusage.DiskUsage(btp.bts.config.DownloadPath); err != nil {
			btp.bts.log.Warningf("Unable to retrieve the free space for %s, continuing anyway...", btp.bts.config.DownloadPath)
		} else {
			btp.diskStatus = status
		}
		if btp.torrentHandle, err = btp.bts.AddTorrent(btp.uri); err != nil {
			return err
		}
	}

	btp.torrentFile = filepath.Join(btp.bts.config.TorrentsPath, fmt.Sprintf("%s.torrent", btp.torrentHandle.infoHash))

	buffered, done := btp.bufferEvents.Listen()
	defer close(done)

	go btp.waitMetadata()

	btp.dialogProgress = xbmc.NewDialogProgress("Quasar", "", "", "")
	defer btp.dialogProgress.Close()

	btp.overlayStatus = xbmc.NewOverlayStatus()

	go btp.waitCheckAvailableSpace()
	go btp.playerLoop()

	if err := <-buffered; err != nil {
		return err.(error)
	}
	return nil
}

func (btp *BTPlayer) waitCheckAvailableSpace() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if btp.chosenFile != nil {
				if !btp.CheckAvailableSpace() {
					btp.bufferEvents.Broadcast(errors.New("not enough space on download destination"))
				}
				return
			}
		}
	}
}

func (btp *BTPlayer) CheckAvailableSpace() bool {
	if btp.diskStatus != nil {
		if btp.torrentHandle == nil || btp.torrentHandle.Info() == nil {
			btp.log.Warning("Missing torrent info to check available space.")
			return false
		}

		sizeLeft := btp.torrentHandle.BytesMissing()
		totalDone := btp.torrentHandle.BytesCompleted()
		totalSize := totalDone + sizeLeft
		if btp.fileSize > 0 && !btp.isRarArchive {
			totalSize = btp.fileSize
		}
		availableSpace := btp.diskStatus.Free

		btp.log.Infof("Checking for sufficient space on %s", btp.bts.config.DownloadPath)
		btp.log.Infof("Total size of download: %s", humanize.Bytes(uint64(totalSize)))
		btp.log.Infof("All time download: %s", humanize.Bytes(uint64(btp.torrentHandle.BytesCompleted())))
		btp.log.Infof("Size total done: %s", humanize.Bytes(uint64(totalDone)))
		if btp.isRarArchive {
			sizeLeft *= 2
			btp.log.Infof("Size left to download (x2 to extract): %s", humanize.Bytes(uint64(sizeLeft)))
		} else {
			btp.log.Infof("Size left to download: %s", humanize.Bytes(uint64(sizeLeft)))
		}
		btp.log.Infof("Available space: %s", humanize.Bytes(uint64(availableSpace)))

		if availableSpace < sizeLeft {
			btp.log.Errorf("No sufficient free space on %s. Has %d, needs %d.", btp.bts.config.DownloadPath, btp.diskStatus.Free, sizeLeft)
			xbmc.Notify("Quasar", "LOCALIZE[30207]", config.AddonIcon())
			btp.bufferEvents.Broadcast(errors.New("not enough space on download destination"))
			btp.notEnoughSpace = true
			return false
		}
	}
	return true
}

func (btp *BTPlayer) waitMetadata() {
	select {
	case <-btp.torrentHandle.GotInfo():
		btp.onMetadataReceived()
	}
}

func (btp *BTPlayer) getBufferSize(file *BTFile) int64 {
	minBufferSize := int64(float64(file.Length()) * startBufferPercent)
	bufferSize := int64(btp.bts.config.BufferSize)
	if minBufferSize < bufferSize {
		return bufferSize
	}
	return minBufferSize
}

func (btp *BTPlayer) onMetadataReceived() {
	btp.log.Info("Metadata received.")
	btp.torrentName = btp.torrentHandle.Name()

	if btp.resumeIndex < 0 {
		// Save .torrent
		btp.log.Infof("Saving %s", btp.torrentFile)
		if err := btp.torrentHandle.SaveMetaInfo(btp.torrentFile); err != nil {
			btp.log.Info("Failed to save torrent: %s", err.Error())
		}
	}

	files := btp.torrentHandle.Files()
	chosenFileIndex, err := btp.chooseFile(files)
	if err != nil {
		btp.bufferEvents.Broadcast(err)
		return
	}
	btp.chosenFile = files[chosenFileIndex]
	btp.fileSize = btp.chosenFile.Length()
	btp.fileName = filepath.Base(btp.chosenFile.Path())
	btp.log.Infof("Chosen file: %s", btp.fileName)

	btp.subtitlesFile = btp.findSubtitlesFile(files)

	btp.log.Infof("Saving torrent to database")
	btp.bts.UpdateDB(Update, btp.torrentHandle.infoHash, btp.tmdbId, btp.contentType, chosenFileIndex, btp.showId, btp.season, btp.episode)

	if btp.isRarArchive {
		btp.chosenFile.Download()
		return
	}

	btp.log.Info("Setting file priorities")
	if btp.subtitlesFile != nil {
		btp.subtitlesFile.Download()
	}
	btp.chosenFile.BufferAndDownload(btp.getBufferSize(btp.chosenFile), endBufferSize)
}

func (btp *BTPlayer) statusStrings(progress float64, state TorrentStatus) (string, string, string) {
	line1 := fmt.Sprintf("%s (%.2f%%)", StatusStrings[state], progress)
	info := btp.torrentHandle.Info()
	if info != nil {
		var totalSize int64
		if btp.fileSize > 0 && !btp.isRarArchive {
			totalSize = btp.fileSize
		} else {
			totalSize = info.Length
		}
		line1 += " - " + humanize.Bytes(uint64(totalSize))
	}
	stats := btp.torrentHandle.AllStats()
	line2 := fmt.Sprintf("D:%.0fkB/s U:%.0fkB/s S:%d/%d P:%d/%d",
		stats.DownloadRate/1024,
		stats.UploadRate/1024,
		stats.ConnectedSeeders,
		stats.TotalSeeders,
		stats.ConnectedIncompletePeers,
		stats.TotalIncompletePeers,
	)
	line3 := ""
	if btp.fileName != "" && !btp.isRarArchive {
		line3 = btp.fileName
	} else {
		line3 = btp.torrentName
	}
	return line1, line2, line3
}

func isRarArchive(fileName string) bool {
	return rarPattern.MatchString(fileName)
}

func (btp *BTPlayer) chooseFile(files []*BTFile) (int, error) {
	var biggestFile int
	maxSize := int64(0)
	var candidateFiles []int

	for i, file := range files {
		size := file.Length()
		if size > maxSize {
			maxSize = size
			biggestFile = i
		}
		if size > minCandidateSize {
			candidateFiles = append(candidateFiles, i)
		}

		fileName := filepath.Base(file.Path())

		if isRarArchive(fileName) && size > 10*1024*1024 {
			btp.isRarArchive = true
			if !xbmc.DialogConfirm("Quasar", "LOCALIZE[30303]") {
				btp.notEnoughSpace = true
				return i, errors.New("RAR archive detected and download was cancelled")
			}
			return i, nil
		}
	}

	if len(candidateFiles) > 1 {
		btp.log.Info(fmt.Sprintf("There are %d candidate files", len(candidateFiles)))
		if btp.fileIndex >= 0 && btp.fileIndex < len(candidateFiles) {
			return candidateFiles[btp.fileIndex], nil
		}

		choices := make(byFilename, 0, len(candidateFiles))
		for _, index := range candidateFiles {
			fileName := filepath.Base(files[index].Path())
			candidate := &candidateFile{
				Index:    index,
				Filename: fileName,
			}
			choices = append(choices, candidate)
		}

		if btp.episode > 0 {
			var lastMatched int
			var foundMatches int
			// Case-insensitive, starting with a line-start or non-ascii, can have leading zeros, followed by non-ascii
			// TODO: Add logic for matching S01E0102 (double episode filename)
			re := regexp.MustCompile(fmt.Sprintf("(?i)(^|\\W)S0*?%dE0*?%d\\W", btp.season, btp.episode))
			for index, choice := range choices {
				if re.MatchString(choice.Filename) {
					lastMatched = index
					foundMatches++
				}
			}

			if foundMatches == 1 {
				return choices[lastMatched].Index, nil
			}
		}

		sort.Sort(choices)

		items := make([]string, 0, len(choices))
		for _, choice := range choices {
			items = append(items, choice.Filename)
		}

		choice := xbmc.ListDialog("LOCALIZE[30223]", items...)
		if choice >= 0 {
			return choices[choice].Index, nil
		} else {
			return 0, fmt.Errorf("User cancelled")
		}
	}

	return biggestFile, nil
}

func (btp *BTPlayer) findSubtitlesFile(files []*BTFile) *BTFile {
	extension := filepath.Ext(btp.fileName)
	chosenName := btp.fileName[0 : len(btp.fileName)-len(extension)]
	srtFileName := chosenName + ".srt"

	var lastMatched *BTFile
	countMatched := 0
	for _, file := range files {
		fileName := file.Path()
		if strings.HasSuffix(fileName, srtFileName) {
			return file
		} else if strings.HasSuffix(fileName, ".srt") {
			lastMatched = file
			countMatched++
		}
	}

	if countMatched == 1 {
		return lastMatched
	}

	return nil
}

func (btp *BTPlayer) Close() {
	close(btp.closing)

	askedToKeepDownloading := true
	if btp.askToKeepDownloading == true {
		if !xbmc.DialogConfirm("Quasar", "LOCALIZE[30146]") {
			askedToKeepDownloading = false
		}
	}

	askedToDelete := false
	if btp.askToDelete == true && (btp.askToKeepDownloading == false || askedToKeepDownloading == false) {
		if xbmc.DialogConfirm("Quasar", "LOCALIZE[30269]") {
			askedToDelete = true
		}
	}

	if askedToKeepDownloading == false || askedToDelete == true || btp.notEnoughSpace {
		// Delete torrent file
		if _, err := os.Stat(btp.torrentFile); err == nil {
			btp.log.Infof("Deleting torrent file at %s", btp.torrentFile)
			defer os.Remove(btp.torrentFile)
		}

		btp.bts.UpdateDB(Delete, btp.torrentHandle.infoHash, 0, "")
		btp.log.Infof("Removed %s from database", btp.torrentHandle.infoHash)

		if btp.deleteAfter || askedToDelete == true || btp.notEnoughSpace {
			btp.log.Info("Removing the torrent and deleting files...")
			btp.bts.RemoveTorrent(btp.torrentHandle, true)
		} else {
			btp.log.Info("Removing the torrent without deleting files...")
			btp.bts.RemoveTorrent(btp.torrentHandle, false)
		}
	}
}

func (btp *BTPlayer) bufferDialog() {
	halfSecond := time.NewTicker(500 * time.Millisecond)
	defer halfSecond.Stop()
	oneSecond := time.NewTicker(1 * time.Second)
	defer oneSecond.Stop()

	for {
		select {
		case <-halfSecond.C:
			if btp.dialogProgress.IsCanceled() || btp.notEnoughSpace {
				btp.log.Info("User cancelled the buffering or not enough space")
				btp.bufferEvents.Broadcast(errors.New("user cancelled the buffering or not enough space"))
				return
			}
		case <-oneSecond.C:
			if btp.chosenFile != nil {
				state := btp.chosenFile.GetState()
				// Handle "Checking" state for resumed downloads
				if state == StatusChecking {
					progress := btp.torrentHandle.GetCheckingProgress()
					line1, line2, line3 := btp.statusStrings(progress, state)
					btp.dialogProgress.Update(int(progress), line1, line2, line3)
				} else if btp.isRarArchive {
					progress := btp.torrentHandle.GetProgress()
					line1, line2, line3 := btp.statusStrings(progress, state)
					btp.dialogProgress.Update(int(progress), line1, line2, line3)

					if progress >= 100 {
						archivePath := filepath.Join(btp.bts.config.DownloadPath, btp.chosenFile.Path())
						destPath := filepath.Join(btp.bts.config.DownloadPath, btp.chosenFile.Path(), "extracted")

						if _, err := os.Stat(destPath); err == nil {
							btp.findExtracted(destPath)
							btp.setRateLimiting(true)
							btp.bufferEvents.Signal()
							return
						} else {
							os.MkdirAll(destPath, 0755)
						}

						cmdName := "unrar"
						cmdArgs := []string{"e", archivePath, destPath}
						cmd := exec.Command(cmdName, cmdArgs...)
						if platform := xbmc.GetPlatform(); platform.OS == "windows" {
							cmdName = "unrar.exe"
						}

						cmdReader, err := cmd.StdoutPipe()
						if err != nil {
							btp.log.Error(err)
							btp.bufferEvents.Broadcast(err)
							xbmc.Notify("Quasar", "LOCALIZE[30304]", config.AddonIcon())
							return
						}

						scanner := bufio.NewScanner(cmdReader)
						go func() {
							for scanner.Scan() {
								btp.log.Infof("unrar | %s", scanner.Text())
							}
						}()

						err = cmd.Start()
						if err != nil {
							btp.log.Error(err)
							btp.bufferEvents.Broadcast(err)
							xbmc.Notify("Quasar", "LOCALIZE[30305]", config.AddonIcon())
							return
						}

						err = cmd.Wait()
						if err != nil {
							btp.log.Error(err)
							btp.bufferEvents.Broadcast(err)
							xbmc.Notify("Quasar", "LOCALIZE[30306]", config.AddonIcon())
							return
						}

						btp.findExtracted(destPath)
						btp.setRateLimiting(true)
						btp.bufferEvents.Signal()
						return
					}
				} else {
					bufferProgress := btp.chosenFile.GetBufferingProgress()
					if bufferProgress < 100 {
						line1, line2, line3 := btp.statusStrings(bufferProgress, StatusBuffering)
						btp.dialogProgress.Update(int(bufferProgress), line1, line2, line3)
					} else {
						line1, line2, line3 := btp.statusStrings(100, StatusBuffering)
						btp.dialogProgress.Update(100, line1, line2, line3)
						btp.setRateLimiting(true)
						btp.bufferEvents.Signal()
						return
					}
				}
			}
		}
	}
}

func (btp *BTPlayer) findExtracted(destPath string) {
	files, err := ioutil.ReadDir(destPath)
	if err != nil {
		btp.log.Error(err)
		btp.bufferEvents.Broadcast(err)
		xbmc.Notify("Quasar", "LOCALIZE[30307]", config.AddonIcon())
		return
	}
	if len(files) == 1 {
		btp.log.Info("Extracted", files[0].Name())
		btp.extracted = files[0].Name()
	} else {
		for _, file := range files {
			fileName := file.Name()
			re := regexp.MustCompile("(?i).*\\.(mkv|mp4|mov|avi)")
			if re.MatchString(fileName) {
				btp.log.Info("Extracted", fileName)
				btp.extracted = fileName
				break
			}
		}
	}
}

func (btp *BTPlayer) setRateLimiting(enable bool) {
	if btp.bts.config.LimitAfterBuffering == true {
		if enable == true {
			if btp.bts.config.MaxDownloadRate > 0 {
				btp.log.Infof("Buffer filled, rate limiting download to %dkB/s", btp.bts.config.MaxDownloadRate/1024)
				btp.bts.clientConfig.DownloadRateLimiter = rate.NewLimiter(rate.Limit(btp.bts.config.MaxDownloadRate), DownloadRateBurst)
			}
			if btp.bts.config.MaxUploadRate > 0 {
				btp.log.Infof("Buffer filled, rate limiting upload to %dkB/s", btp.bts.config.MaxUploadRate/1024)
				btp.bts.clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(btp.bts.config.MaxUploadRate), UploadRateBurst)
			}
		} else {
			btp.log.Info("Resetting rate limiting")
			btp.bts.clientConfig.DownloadRateLimiter = rate.NewLimiter(rate.Inf, 0)
			btp.bts.clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Inf, 0)
		}
	}
}

func updateWatchTimes() {
	ret := xbmc.GetWatchTimes()
	err := ret["error"]
	if err == "" {
		WatchedTime, _ = strconv.ParseFloat(ret["watchedTime"], 64)
		VideoDuration, _ = strconv.ParseFloat(ret["videoDuration"], 64)
	}
}

func (btp *BTPlayer) playerLoop() {
	defer btp.Close()

	btp.log.Info("Buffer loop")

	buffered, bufferDone := btp.bufferEvents.Listen()
	defer close(bufferDone)

	go btp.bufferDialog()

	if err := <-buffered; err != nil {
		return
	}

	btp.log.Info("Waiting for playback...")
	oneSecond := time.NewTicker(1 * time.Second)
	defer oneSecond.Stop()
	playbackTimeout := time.After(playbackMaxWait)

playbackWaitLoop:
	for {
		if xbmc.PlayerIsPlaying() {
			break playbackWaitLoop
		}
		select {
		case <-playbackTimeout:
			btp.log.Warningf("Playback was unable to start after %d seconds. Aborting...", playbackMaxWait/time.Second)
			btp.bufferEvents.Broadcast(errors.New("playback was unable to start before timeout"))
			return
		case <-oneSecond.C:
		}
	}

	btp.log.Info("Playback loop")
	overlayStatusActive := false
	playing := true

	updateWatchTimes()

	btp.log.Infof("Got playback: %fs / %fs", WatchedTime, VideoDuration)
	if btp.scrobble {
		trakt.Scrobble("start", btp.contentType, btp.tmdbId, WatchedTime, VideoDuration)
	}

playbackLoop:
	for {
		if xbmc.PlayerIsPlaying() == false {
			break playbackLoop
		}
		select {
		case <-oneSecond.C:
			if Seeked {
				Seeked = false
				updateWatchTimes()
				if btp.scrobble {
					trakt.Scrobble("start", btp.contentType, btp.tmdbId, WatchedTime, VideoDuration)
				}
			} else if xbmc.PlayerIsPaused() {
				if playing == true {
					playing = false
					updateWatchTimes()
					if btp.scrobble {
						trakt.Scrobble("pause", btp.contentType, btp.tmdbId, WatchedTime, VideoDuration)
					}
				}
				if btp.overlayStatusEnabled == true {
					progress := btp.torrentHandle.GetProgress()
					line1, line2, line3 := btp.statusStrings(progress, btp.chosenFile.GetState())
					btp.overlayStatus.Update(int(progress), line1, line2, line3)
					if overlayStatusActive == false {
						btp.overlayStatus.Show()
						overlayStatusActive = true
					}
				}
			} else {
				updateWatchTimes()
				if playing == false {
					playing = true
					if btp.scrobble {
						trakt.Scrobble("start", btp.contentType, btp.tmdbId, WatchedTime, VideoDuration)
					}
				}
				if overlayStatusActive == true {
					btp.overlayStatus.Hide()
					overlayStatusActive = false
				}
			}
		}
	}

	if btp.scrobble {
		trakt.Scrobble("stop", btp.contentType, btp.tmdbId, WatchedTime, VideoDuration)
	}
	Paused = false
	Seeked = false
	Playing = false
	WasPlaying = true
	FromLibrary = false
	WatchedTime = 0
	VideoDuration = 0

	btp.overlayStatus.Close()
	btp.setRateLimiting(false)
}
