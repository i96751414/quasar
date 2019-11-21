// +build !arm

package bittorrent

import (
	lt "github.com/anacrolix/torrent"
)

// Nothing to do on regular devices
func setPlatformSpecificSettings(c *lt.ClientConfig) {
}
