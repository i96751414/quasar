package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cloudflare/ahocorasick"
	"github.com/dustin/go-humanize"
	"github.com/gin-gonic/gin"
	"github.com/i96751414/quasar/bittorrent"
	"github.com/i96751414/quasar/config"
	"github.com/i96751414/quasar/util"
	"github.com/i96751414/quasar/xbmc"
	"github.com/op/go-logging"
)

var torrentsLog = logging.MustGetLogger("torrents")

type TorrentsWeb struct {
	Name          string  `json:"name"`
	Size          string  `json:"size"`
	Status        string  `json:"status"`
	Progress      float64 `json:"progress"`
	Ratio         float64 `json:"ratio"`
	TimeRatio     float64 `json:"time_ratio"`
	SeedingTime   string  `json:"seeding_time"`
	SeedTime      float64 `json:"seed_time"`
	SeedTimeLimit int     `json:"seed_time_limit"`
	DownloadRate  float64 `json:"download_rate"`
	UploadRate    float64 `json:"upload_rate"`
	Seeders       int     `json:"seeders"`
	SeedersTotal  int     `json:"seeders_total"`
	Peers         int     `json:"peers"`
	PeersTotal    int     `json:"peers_total"`
}

type TorrentMap struct {
	tmdbId  string
	torrent *bittorrent.Torrent
}

var TorrentsMap []*TorrentMap

func AddToTorrentsMap(tmdbId string, torrent *bittorrent.Torrent) {
	inTorrentsMap := false
	for _, torrentMap := range TorrentsMap {
		if tmdbId == torrentMap.tmdbId {
			inTorrentsMap = true
		}
	}
	if inTorrentsMap == false {
		torrentMap := &TorrentMap{
			tmdbId:  tmdbId,
			torrent: torrent,
		}
		TorrentsMap = append(TorrentsMap, torrentMap)
	}
}

func InTorrentsMap(tmdbId string) (torrents []*bittorrent.Torrent) {
	for index, torrentMap := range TorrentsMap {
		if tmdbId == torrentMap.tmdbId {
			if xbmc.DialogConfirm("Quasar", "LOCALIZE[30260]") {
				torrents = append(torrents, torrentMap.torrent)
			} else {
				TorrentsMap = append(TorrentsMap[:index], TorrentsMap[index+1:]...)
			}
		}
	}
	return torrents
}

func nameMatch(torrentName string, itemName string) bool {
	patterns := strings.FieldsFunc(strings.ToLower(itemName), func(r rune) bool {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsMark(r) {
			return true
		}
		return false
	})

	m := ahocorasick.NewStringMatcher(patterns)

	found := m.Match([]byte(strings.ToLower(torrentName)))

	return len(found) >= len(patterns)
}

func ExistingTorrent(btService *bittorrent.BTService, longName string) (existingTorrent string) {
	for _, torrentHandle := range btService.Torrents {
		if torrentHandle == nil {
			continue
		}
		info := torrentHandle.Info()
		if info == nil {
			continue
		}

		if nameMatch(info.Name, longName) {
			infoHash := torrentHandle.InfoHash().HexString()

			torrentFile := filepath.Join(config.Get().TorrentsPath, fmt.Sprintf("%s.torrent", infoHash))
			return torrentFile
		}
	}
	return ""
}

func ListTorrents(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		items := make(xbmc.ListItems, 0, len(btService.Torrents))

		torrentsLog.Info("Currently downloading:")
		for i, torrentHandle := range btService.Torrents {
			if torrentHandle == nil {
				continue
			}
			info := torrentHandle.Info()
			if info == nil {
				continue
			}

			torrentName := info.Name
			progress := torrentHandle.GetProgress()
			stats := torrentHandle.AllStats()

			ratio := float64(0)
			allTimeDownload := stats.BytesReadData.Int64()
			if allTimeDownload > 0 {
				ratio = float64(stats.BytesWrittenData.Int64()*100) / float64(allTimeDownload)
			}

			timeRatio := float64(0)
			finishedTime := stats.FinishedTime
			downloadTime := stats.ActiveTime - finishedTime
			if downloadTime > 1 {
				timeRatio = finishedTime / downloadTime
			}

			seedingTime := time.Duration(stats.SeedingTime) * time.Second
			if progress >= 100 && seedingTime == 0 {
				seedingTime = time.Duration(finishedTime) * time.Second
			}

			//sessionAction := []string{"LOCALIZE[30233]", fmt.Sprintf("XBMC.RunPlugin(%s)", UrlForXBMC("/torrents/pause"))}

			var torrentAction []string
			status := bittorrent.StatusStrings[torrentHandle.GetState()]
			if status == "Paused" {
				if progress >= 100 {
					status = "Finished"
				}
				torrentAction = []string{"LOCALIZE[30235]", fmt.Sprintf("XBMC.RunPlugin(%s)", UrlForXBMC("/torrents/resume/%d", i))}
			} else {
				torrentAction = []string{"LOCALIZE[30231]", fmt.Sprintf("XBMC.RunPlugin(%s)", UrlForXBMC("/torrents/pause/%d", i))}
			}

			color := "white"
			switch status {
			case "Paused":
				fallthrough
			case "Finished":
				color = "grey"
			case "Seeding":
				color = "green"
			case "Buffering":
				color = "blue"
			case "Finding":
				color = "orange"
			case "Checking":
				color = "teal"
			case "Queued":
				color = "black"
			}
			torrentsLog.Infof("- %.2f%% - %s - %.2f:1 / %.2f:1 (%s) - %s", progress, status, ratio, timeRatio, seedingTime.String(), torrentName)

			var (
				tmdb        string
				show        string
				season      string
				episode     string
				contentType string
			)
			infoHash := torrentHandle.InfoHash().HexString()
			dbItem := btService.GetDBItem(infoHash)
			if dbItem != nil && dbItem.Type != "" {
				contentType = dbItem.Type
				if contentType == "movie" {
					tmdb = strconv.Itoa(dbItem.ID)
				} else {
					show = strconv.Itoa(dbItem.ShowID)
					season = strconv.Itoa(dbItem.Season)
					episode = strconv.Itoa(dbItem.Episode)
				}
			}

			playUrl := UrlQuery(UrlForXBMC("/play"),
				"resume", strconv.Itoa(i),
				"type", contentType,
				"tmdb", tmdb,
				"show", show,
				"season", season,
				"episode", episode)

			item := xbmc.ListItem{
				Label: fmt.Sprintf("%.2f%% - [COLOR %s]%s[/COLOR] - %.2f:1 / %.2f:1 (%s) - %s", progress, color, status, ratio, timeRatio, seedingTime.String(), torrentName),
				Path:  playUrl,
				Info: &xbmc.ListItemInfo{
					Title: torrentName,
				},
			}
			item.ContextMenu = [][]string{
				{"LOCALIZE[30230]", fmt.Sprintf("XBMC.PlayMedia(%s)", playUrl)},
				torrentAction,
				{"LOCALIZE[30232]", fmt.Sprintf("XBMC.RunPlugin(%s)", UrlForXBMC("/torrents/delete/%d", i))},
				{"LOCALIZE[30276]", fmt.Sprintf("XBMC.RunPlugin(%s)", UrlForXBMC("/torrents/delete/%d?files=1", i))},
				{"LOCALIZE[30308]", fmt.Sprintf("XBMC.RunPlugin(%s)", UrlForXBMC("/torrents/move/%d", i))},
				//sessionAction,
			}
			item.IsPlayable = true
			items = append(items, &item)
		}

		ctx.JSON(200, xbmc.NewView("", items))
	}
}

func ListTorrentsWeb(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		torrents := make([]*TorrentsWeb, 0, len(btService.Torrents))
		seedTimeLimit := config.Get().SeedTimeLimit

		for _, torrentHandle := range btService.Torrents {
			if torrentHandle == nil {
				continue
			}
			info := torrentHandle.Info()
			if info == nil {
				continue
			}

			torrentName := info.Name
			progress := torrentHandle.GetProgress()

			status := bittorrent.StatusStrings[torrentHandle.GetState()]
			if status == "Paused" {
				if progress >= 100 {
					status = "Finished"
				}
			}

			stats := torrentHandle.AllStats()
			ratio := float64(0)
			allTimeDownload := stats.BytesReadData.Int64()
			if allTimeDownload > 0 {
				ratio = float64(stats.BytesWrittenData.Int64()*100) / float64(allTimeDownload)
			}

			timeRatio := float64(0)
			finishedTime := stats.FinishedTime
			downloadTime := stats.ActiveTime - finishedTime
			if downloadTime > 1 {
				timeRatio = finishedTime / downloadTime
			}

			seedingTime := time.Duration(stats.SeedingTime) * time.Second
			if progress >= 100 && seedingTime == 0 {
				seedingTime = time.Duration(finishedTime) * time.Second
			}

			size := humanize.Bytes(uint64(info.Length))
			downloadRate := stats.DownloadRate / 1024
			uploadRate := stats.UploadRate / 1024

			torrent := TorrentsWeb{
				Name:          torrentName,
				Size:          size,
				Status:        status,
				Progress:      progress,
				Ratio:         ratio,
				TimeRatio:     timeRatio,
				SeedingTime:   seedingTime.String(),
				SeedTime:      seedingTime.Seconds(),
				SeedTimeLimit: seedTimeLimit,
				DownloadRate:  downloadRate,
				UploadRate:    uploadRate,
				Seeders:       stats.ConnectedSeeders,
				SeedersTotal:  stats.TotalSeeders,
				Peers:         stats.ConnectedIncompletePeers,
				PeersTotal:    stats.TotalIncompletePeers,
			}
			torrents = append(torrents, &torrent)
		}

		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		ctx.JSON(200, torrents)
	}
}

/*func PauseSession(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		btService.client.GetHandle().Pause()
		xbmc.Refresh()
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		ctx.String(200, "")
	}
}

func ResumeSession(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		btService.client.GetHandle().Resume()
		xbmc.Refresh()
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		ctx.String(200, "")
	}
}*/

func AddTorrent(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		uri := ctx.Query("uri")
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")

		if uri == "" {
			ctx.String(404, "Missing torrent URI")
			return
		}
		torrentsLog.Infof("Adding torrent from %s", uri)

		_, err := btService.AddTorrent(uri)
		if err != nil {
			ctx.String(404, err.Error())
			return
		}

		xbmc.Refresh()
		ctx.String(200, "")
	}
}

func ResumeTorrent(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		torrentId := ctx.Params.ByName("torrentId")
		torrentIndex, _ := strconv.Atoi(torrentId)
		torrentHandle := btService.Torrents[torrentIndex]

		if torrentHandle == nil {
			ctx.Error(errors.New(fmt.Sprintf("Unable to resume torrent with index %d", torrentIndex)))
			return
		}

		info := torrentHandle.Info()
		if info != nil {
			torrentsLog.Infof("Resuming %s", info.Name)
		} else {
			torrentsLog.Infof("Resuming %s", torrentHandle.InfoHash().HexString())
		}

		torrentHandle.Resume()

		xbmc.Refresh()
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		ctx.String(200, "")
	}
}

func MoveTorrent(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		torrentId := ctx.Params.ByName("torrentId")
		torrentIndex, _ := strconv.Atoi(torrentId)
		torrentHandle := btService.Torrents[torrentIndex]
		if torrentHandle == nil {
			ctx.Error(errors.New("Invalid torrent handle"))
			return
		}

		info := torrentHandle.Info()
		if info != nil {
			torrentsLog.Infof("Marking %s to be moved...", info.Name)
		} else {
			torrentsLog.Infof("Marking %s to be moved...", torrentHandle.InfoHash().HexString())
		}
		btService.MarkedToMove = torrentIndex

		xbmc.Refresh()
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		ctx.String(200, "")
	}
}

func PauseTorrent(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		torrentId := ctx.Params.ByName("torrentId")
		torrentIndex, _ := strconv.Atoi(torrentId)
		torrentHandle := btService.Torrents[torrentIndex]
		if torrentHandle == nil {
			ctx.Error(errors.New("invalid torrent handle"))
			return
		}

		info := torrentHandle.Info()
		if info != nil {
			torrentsLog.Infof("Pausing torrent %s", info.Name)
		} else {
			torrentsLog.Infof("Pausing torrent %s", torrentHandle.InfoHash().HexString())
		}
		torrentHandle.Pause()

		xbmc.Refresh()
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		ctx.String(200, "")
	}
}

func RemoveTorrent(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		deleteFiles := ctx.Query("files")
		torrentId := ctx.Params.ByName("torrentId")
		torrentIndex, _ := strconv.Atoi(torrentId)

		torrentsPath := config.Get().TorrentsPath

		torrentHandle := btService.Torrents[torrentIndex]
		if torrentHandle == nil {
			ctx.Error(errors.New("Invalid torrent handle"))
			return
		}

		infoHash := torrentHandle.InfoHash().HexString()

		// Delete torrent file
		torrentFile := filepath.Join(torrentsPath, fmt.Sprintf("%s.torrent", infoHash))
		if _, err := os.Stat(torrentFile); err == nil {
			torrentsLog.Infof("Deleting torrent file at %s", torrentFile)
			defer os.Remove(torrentFile)
		}

		// Delete fast resume data
		fastResumeFile := filepath.Join(torrentsPath, fmt.Sprintf("%s.fastresume", infoHash))
		if _, err := os.Stat(fastResumeFile); err == nil {
			torrentsLog.Infof("Deleting fast resume data at %s", fastResumeFile)
			defer os.Remove(fastResumeFile)
		}

		btService.UpdateDB(bittorrent.Delete, infoHash, 0, "")
		torrentsLog.Infof("Removed %s from database", infoHash)

		askedToDelete := false
		if config.Get().KeepFilesAsk == true && deleteFiles == "" {
			if xbmc.DialogConfirm("Quasar", "LOCALIZE[30269]") {
				askedToDelete = true
			}
		}

		if config.Get().KeepFilesAfterStop == false || askedToDelete == true || deleteFiles == "true" {
			torrentsLog.Info("Removing the torrent and deleting files...")
			btService.RemoveTorrent(torrentHandle, true)
		} else {
			torrentsLog.Info("Removing the torrent without deleting files...")
			btService.RemoveTorrent(torrentHandle, true)
		}

		xbmc.Refresh()
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		ctx.String(200, "")
	}
}

func Versions(btService *bittorrent.BTService) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		type Versions struct {
			Version   string `json:"version"`
			UserAgent string `json:"user-agent"`
		}
		versions := Versions{
			Version:   util.GetVersion(),
			UserAgent: btService.UserAgent,
		}
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		ctx.JSON(200, versions)
	}
}
