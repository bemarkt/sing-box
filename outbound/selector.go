package outbound

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/interrupt"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

var (
	_ adapter.Outbound      = (*Selector)(nil)
	_ adapter.OutboundGroup = (*Selector)(nil)
)

type Selector struct {
	myOutboundAdapter
	myGroupAdapter
	defaultTag                   string
	outbounds                    []adapter.Outbound
	outboundByTag                map[string]adapter.Outbound
	selected                     adapter.Outbound
	interruptGroup               *interrupt.Group
	interruptExternalConnections bool
}

func NewSelector(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.SelectorOutboundOptions) (*Selector, error) {
	outbound := &Selector{
		myOutboundAdapter: myOutboundAdapter{
			protocol:     C.TypeSelector,
			router:       router,
			logger:       logger,
			tag:          tag,
			dependencies: options.Outbounds,
		},
		myGroupAdapter: myGroupAdapter{
			ctx:             ctx,
			tags:            options.Outbounds,
			uses:            options.Providers,
			useAllProviders: options.UseAllProviders,
			includes:        options.Includes,
			excludes:        options.Excludes,
			types:           options.Types,
			ports:           make(map[int]bool),
			providers:       make(map[string]adapter.OutboundProvider),
		},
		defaultTag:                   options.Default,
		outbounds:                    []adapter.Outbound{},
		outboundByTag:                make(map[string]adapter.Outbound),
		interruptGroup:               interrupt.NewGroup(),
		interruptExternalConnections: options.InterruptExistConnections,
	}
	if len(outbound.tags) == 0 && len(outbound.uses) == 0 && !outbound.useAllProviders {
		return nil, E.New("missing tags and uses")
	}
	portMap, err := CreatePortsMap(options.Ports)
	if err != nil {
		return nil, err
	}
	outbound.ports = portMap
	return outbound, nil
}

func (s *Selector) Network() []string {
	if s.selected == nil {
		return []string{N.NetworkTCP, N.NetworkUDP}
	}
	return s.selected.Network()
}

func (s *Selector) Start() error {
	if s.useAllProviders {
		uses := []string{}
		for _, provider := range s.router.OutboundProviders() {
			uses = append(uses, provider.Tag())
		}
		s.uses = uses
	}
	outbounds, outboundByTag, err := s.pickOutbounds()
	s.outbounds = outbounds
	s.outboundByTag = outboundByTag
	return err
}

func (s *Selector) pickOutbounds() ([]adapter.Outbound, map[string]adapter.Outbound, error) {
	outbounds := []adapter.Outbound{}
	outboundByTag := map[string]adapter.Outbound{}

	for i, tag := range s.tags {
		detour, loaded := s.router.Outbound(tag)
		if !loaded {
			return nil, nil, E.New("outbound ", i, " not found: ", tag)
		}
		outbounds = append(outbounds, detour)
		outboundByTag[tag] = detour
	}

	for i, tag := range s.uses {
		provider, loaded := s.router.OutboundProvider(tag)
		if !loaded {
			return nil, nil, E.New("outbound provider ", i, " not found: ", tag)
		}
		if _, ok := s.providers[tag]; !ok {
			s.providers[tag] = provider
		}
		for _, outbound := range provider.Outbounds() {
			if s.OutboundFilter(outbound) {
				tag := outbound.Tag()
				outbounds = append(outbounds, outbound)
				outboundByTag[tag] = outbound
			}
		}
	}

	if len(outbounds) == 0 {
		OUTBOUNDLESS, _ := s.router.Outbound("OUTBOUNDLESS")
		outbounds = append(outbounds, OUTBOUNDLESS)
		outboundByTag["OUTBOUNDLESS"] = OUTBOUNDLESS
		s.selected = OUTBOUNDLESS
		return outbounds, outboundByTag, nil
	}

	if s.tag != "" {
		cacheFile := service.FromContext[adapter.CacheFile](s.ctx)
		if cacheFile != nil {
			selected := cacheFile.LoadSelected(s.tag)
			if selected != "" {
				detour, loaded := outboundByTag[selected]
				if loaded {
					s.selected = detour
					return outbounds, outboundByTag, nil
				}
			}
		}
	}

	if s.defaultTag != "" {
		detour, loaded := outboundByTag[s.defaultTag]
		if !loaded {
			return nil, nil, E.New("default outbound not found: ", s.defaultTag)
		}
		s.selected = detour
		return outbounds, outboundByTag, nil
	}

	s.selected = outbounds[0]
	return outbounds, outboundByTag, nil
}

func (s *Selector) UpdateOutbounds(tag string) error {
	if _, ok := s.providers[tag]; ok {
		outbounds, outboundByTag, err := s.pickOutbounds()
		if err != nil {
			return E.New("update oubounds failed: ", s.tag)
		}
		s.outbounds = outbounds
		s.outboundByTag = outboundByTag
	}
	return nil
}

func (s *Selector) Now() string {
	return s.selected.Tag()
}

func (s *Selector) All() []string {
	all := []string{}
	for _, outbound := range s.outbounds {
		all = append(all, outbound.Tag())
	}
	return all
}

func (s *Selector) SelectOutbound(tag string) bool {
	detour, loaded := s.outboundByTag[tag]
	if !loaded {
		return false
	}
	if s.selected == detour {
		return true
	}
	s.selected = detour
	if s.tag != "" {
		cacheFile := service.FromContext[adapter.CacheFile](s.ctx)
		if cacheFile != nil {
			err := cacheFile.StoreSelected(s.tag, tag)
			if err != nil {
				s.logger.Error("store selected: ", err)
			}
		}
	}
	s.interruptGroup.Interrupt(s.interruptExternalConnections)
	return true
}

func (s *Selector) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	conn, err := s.selected.DialContext(ctx, network, destination)
	if err != nil {
		return nil, err
	}
	return s.interruptGroup.NewConn(conn, interrupt.IsExternalConnectionFromContext(ctx)), nil
}

func (s *Selector) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	conn, err := s.selected.ListenPacket(ctx, destination)
	if err != nil {
		return nil, err
	}
	return s.interruptGroup.NewPacketConn(conn, interrupt.IsExternalConnectionFromContext(ctx)), nil
}

func (s *Selector) NewConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	ctx = interrupt.ContextWithIsExternalConnection(ctx)
	return s.selected.NewConnection(ctx, conn, metadata)
}

func (s *Selector) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	ctx = interrupt.ContextWithIsExternalConnection(ctx)
	return s.selected.NewPacketConnection(ctx, conn, metadata)
}

func RealTag(detour adapter.Outbound) string {
	if group, isGroup := detour.(adapter.OutboundGroup); isGroup {
		return group.Now()
	}
	return detour.Tag()
}
