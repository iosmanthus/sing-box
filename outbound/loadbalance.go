package outbound

import (
	"context"
	"github.com/sagernet/gvisor/pkg/sync"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"net"
)

var (
	_ adapter.Outbound      = (*LoadBalance)(nil)
	_ adapter.OutboundGroup = (*LoadBalance)(nil)
)

type LoadBalance struct {
	myOutboundAdapter
	tags      []string
	outbounds []adapter.Outbound
	weights   []int64
	mu        struct {
		sync.RWMutex
		c   int64
		idx int64
	}
}

func (s *LoadBalance) next() adapter.Outbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.outbounds[s.mu.idx]
	if s.mu.c < s.weights[s.mu.idx] {
		s.mu.c++
		return next
	}
	s.mu.c = 0
	s.mu.idx = (s.mu.idx + 1) % int64(len(s.outbounds))
	return next
}

func NewLoadBalance(
	router adapter.Router,
	logger log.ContextLogger,
	tag string,
	options option.LoadBalanceOutboundOptions,
) (*LoadBalance, error) {
	if len(options.Outbounds) == 0 {
		return nil, E.New("missing tags")
	}

	lb := &LoadBalance{
		myOutboundAdapter: myOutboundAdapter{
			protocol:     C.TypeLoadBalance,
			router:       router,
			logger:       logger,
			tag:          tag,
			dependencies: options.Outbounds,
		},
		tags:      options.Outbounds,
		outbounds: make([]adapter.Outbound, len(options.Outbounds)),
		weights:   options.Weights,
	}

	return lb, nil
}

func (s *LoadBalance) Network() []string {
	return []string{N.NetworkTCP, N.NetworkUDP}
}

func (s *LoadBalance) Start() error {
	for i, tag := range s.tags {
		detour, ok := s.router.Outbound(tag)
		if !ok {
			return E.New("outbound not found: ", tag)
		}
		s.outbounds[i] = detour
	}

	return nil
}

func (s *LoadBalance) Now() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outbounds[s.mu.idx].Tag()
}

func (s *LoadBalance) All() []string {
	return s.tags
}

func (s *LoadBalance) DialContext(
	ctx context.Context,
	network string,
	destination M.Socksaddr,
) (net.Conn, error) {
	return s.next().DialContext(ctx, network, destination)
}

func (s *LoadBalance) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return s.next().ListenPacket(ctx, destination)
}

func (s *LoadBalance) NewConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	return s.next().NewConnection(ctx, conn, metadata)
}

func (s *LoadBalance) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	return s.next().NewPacketConnection(ctx, conn, metadata)
}
