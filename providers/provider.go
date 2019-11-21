package providers

import (
	"github.com/i96751414/quasar/bittorrent"
	"github.com/i96751414/quasar/tmdb"
)

type Searcher interface {
	SearchLinks(query string) []*bittorrent.Torrent
}

type MovieSearcher interface {
	SearchMovieLinks(movie *tmdb.Movie) []*bittorrent.Torrent
}

type SeasonSearcher interface {
	SearchSeasonLinks(show *tmdb.Show, season *tmdb.Season) []*bittorrent.Torrent
}

type EpisodeSearcher interface {
	SearchEpisodeLinks(show *tmdb.Show, episode *tmdb.Episode) []*bittorrent.Torrent
}
