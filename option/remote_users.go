package option

import (
	"github.com/sagernet/sing/common/json/badjson"
	"github.com/sagernet/sing/common/json/badoption"
)

type RemoteUsersServiceOptions struct {
	URL            string                            `json:"url"`
	Token          string                            `json:"token,omitempty"`
	Interval       badoption.Duration                `json:"interval,omitempty"`
	RequestTimeout badoption.Duration                `json:"request_timeout,omitempty"`
	CachePath      string                            `json:"cache_path,omitempty"`
	DownloadDetour string                            `json:"download_detour,omitempty"`
	Servers        *badjson.TypedMap[string, string] `json:"servers"`
}
