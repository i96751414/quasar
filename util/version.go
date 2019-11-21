package util

import (
	"fmt"
	"github.com/anacrolix/torrent"
)

var (
	Version = "development"
)

func UserAgent() string {
	return fmt.Sprintf("Quasar/%s %s", GetVersion(), torrent.DefaultHTTPUserAgent)
}

func GetVersion() string {
	return Version //[1 : len(Version)-1]
}

func DefaultPeerID() string {
	return "-GT0001-"
}
