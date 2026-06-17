package remoteusers

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
)

const (
	defaultInterval       = time.Minute
	defaultRequestTimeout = 10 * time.Second
)

type Service struct {
	boxService.Adapter
	ctx            context.Context
	cancel         context.CancelFunc
	logger         log.ContextLogger
	url            string
	token          string
	interval       time.Duration
	requestTimeout time.Duration
	cachePath      string
	downloadDetour string
	targets        []userUpdater
	httpClient     *http.Client
	ticker         *time.Ticker
	access         sync.Mutex
	lastEtag       string
	lastHash       [32]byte
	currentCount   int
}

// update performs one reconcile: fetch -> (304 / unchanged / empty-over-nonempty
// -> no-op) -> apply -> persist. On any error it returns without mutating the
// live user set, so the last-good set is preserved.
func (s *Service) update(ctx context.Context) error {
	s.access.Lock()
	defer s.access.Unlock()

	result, err := fetchUsers(ctx, s.httpClient, s.url, s.token, s.lastEtag)
	if err != nil {
		return err
	}
	if result.notModified {
		return nil
	}
	// Safety floor: never wipe a non-empty live set with an empty response.
	if len(result.users) == 0 && s.currentCount > 0 {
		s.logger.Warn("remote user list is empty; keeping previous ", s.currentCount, " users")
		return nil
	}
	newHash := hashUsers(result.users)
	if newHash == s.lastHash {
		s.lastEtag = result.etag
		return nil
	}
	if len(result.users) > 0 {
		if err = applyUsers(s.targets, result.users); err != nil {
			return err
		}
	}
	s.lastHash = newHash
	s.lastEtag = result.etag
	s.currentCount = len(result.users)
	if err = saveCache(s.cachePath, &cachedUsers{Users: result.users, Etag: result.etag, LastUpdated: time.Now()}); err != nil {
		s.logger.Error(E.Cause(err, "save user cache"))
	}
	s.logger.Info("updated to ", s.currentCount, " users")
	return nil
}

// Ensure the package imports are all used until Task 6 fills in the rest.
var (
	_ = adapter.StartStateStart
	_ = boxService.NewAdapter
	_ = C.TypeRemoteUsers
	_ = option.RemoteUsersServiceOptions{}
	_ = service.FromContext[adapter.InboundManager]
)
