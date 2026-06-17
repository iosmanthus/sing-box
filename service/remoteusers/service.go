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
	done           chan struct{}
	logger         log.ContextLogger
	url            string
	token          string
	interval       time.Duration
	requestTimeout time.Duration
	cachePath      string
	downloadDetour string
	targets        []userUpdater
	httpClient     *http.Client
	closeTransport func()
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
		if result.etag != "" {
			s.lastEtag = result.etag
		}
		return nil
	}
	if len(result.users) > 0 {
		if err = applyUsers(s.targets, result.users); err != nil {
			return err
		}
	}
	s.lastHash = newHash
	if result.etag != "" {
		s.lastEtag = result.etag
	}
	s.currentCount = len(result.users)
	if err = saveCache(s.cachePath, &cachedUsers{Users: result.users, Etag: result.etag, LastUpdated: time.Now()}); err != nil {
		s.logger.Error(E.Cause(err, "save user cache"))
	}
	s.logger.Info("updated to ", s.currentCount, " users")
	return nil
}

func RegisterService(registry *boxService.Registry) {
	boxService.Register[option.RemoteUsersServiceOptions](registry, C.TypeRemoteUsers, NewService)
}

func NewService(ctx context.Context, logger log.ContextLogger, tag string, options option.RemoteUsersServiceOptions) (adapter.Service, error) {
	if options.URL == "" {
		return nil, E.New("missing url")
	}
	if options.Servers == nil || options.Servers.Size() == 0 {
		return nil, E.New("missing servers")
	}
	inboundManager := service.FromContext[adapter.InboundManager](ctx)
	if inboundManager == nil {
		return nil, E.New("inbound manager not available")
	}
	var targets []userUpdater
	for i, entry := range options.Servers.Entries() {
		inbound, loaded := inboundManager.Get(entry.Value)
		if !loaded {
			return nil, E.New("remote_users server[", i, "]: inbound ", entry.Value, " not found")
		}
		managed, isManaged := inbound.(adapter.ManagedSSMServer)
		if !isManaged {
			return nil, E.New("remote_users server[", i, "]: inbound/", inbound.Type(), "[", inbound.Tag(), "] is not a managed (SSM) server")
		}
		targets = append(targets, managed)
	}
	interval := defaultInterval
	if options.Interval > 0 {
		interval = time.Duration(options.Interval)
	}
	requestTimeout := defaultRequestTimeout
	if options.RequestTimeout > 0 {
		requestTimeout = time.Duration(options.RequestTimeout)
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	return &Service{
		Adapter:        boxService.NewAdapter(C.TypeRemoteUsers, tag),
		ctx:            serviceCtx,
		cancel:         cancel,
		logger:         logger,
		url:            options.URL,
		token:          options.Token,
		interval:       interval,
		requestTimeout: requestTimeout,
		cachePath:      options.CachePath,
		downloadDetour: options.DownloadDetour,
		targets:        targets,
	}, nil
}

func (s *Service) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	transport, closeTransport, err := s.resolveTransport()
	if err != nil {
		return E.Cause(err, "create remote_users http client")
	}
	s.httpClient = &http.Client{Timeout: s.requestTimeout, Transport: transport}
	s.closeTransport = closeTransport

	// Floor: apply the on-disk last-good cache before the first fetch, so the
	// node comes up with users even while the SOT is unreachable.
	if cache, cacheErr := loadCache(s.cachePath); cacheErr != nil {
		s.logger.Error(E.Cause(cacheErr, "load user cache"))
	} else if cache != nil && len(cache.Users) > 0 {
		if applyErr := applyUsers(s.targets, cache.Users); applyErr != nil {
			s.logger.Error(E.Cause(applyErr, "apply cached users"))
		} else {
			s.lastHash = hashUsers(cache.Users)
			s.lastEtag = cache.Etag
			s.currentCount = len(cache.Users)
			s.logger.Info("loaded ", s.currentCount, " users from cache")
		}
	}

	// Synchronous initial fetch, but non-fatal: never block startup on the SOT.
	fetchCtx, cancel := context.WithTimeout(s.ctx, s.requestTimeout)
	err = s.update(fetchCtx)
	cancel()
	if err != nil {
		s.logger.Error(E.Cause(err, "initial user fetch (continuing with cached/empty set)"))
	}

	s.ticker = time.NewTicker(s.interval)
	s.done = make(chan struct{})
	go s.loopUpdate()
	return nil
}

func (s *Service) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.ticker != nil {
		s.ticker.Stop()
	}
	if s.done != nil {
		<-s.done
	}
	if s.closeTransport != nil {
		s.closeTransport()
		s.closeTransport = nil
	}
	return nil
}

func (s *Service) loopUpdate() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.ticker.C:
			fetchCtx, cancel := context.WithTimeout(s.ctx, s.requestTimeout)
			err := s.update(fetchCtx)
			cancel()
			if err != nil {
				s.logger.Error(E.Cause(err, "update users (keeping previous set)"))
			}
		}
	}
}

// resolveTransport returns Go's default transport for a direct egress fetch, or
// a detour-bound transport from the HTTP client manager when download_detour is
// set. The returned cleanup func closes idle connections of a detour-bound
// transport on shutdown; it is nil for the shared default transport (which must
// not be closed).
func (s *Service) resolveTransport() (http.RoundTripper, func(), error) {
	if s.downloadDetour == "" {
		return http.DefaultTransport, nil, nil
	}
	httpClientManager := service.FromContext[adapter.HTTPClientManager](s.ctx)
	if httpClientManager == nil {
		return nil, nil, E.New("download_detour set but http client manager unavailable")
	}
	transport, err := httpClientManager.ResolveTransport(s.ctx, s.logger, option.HTTPClientOptions{
		DialerOptions: option.DialerOptions{
			Detour: s.downloadDetour,
		},
		DisableEmptyDirectCheck: true,
	})
	if err != nil {
		return nil, nil, err
	}
	return transport, transport.CloseIdleConnections, nil
}
