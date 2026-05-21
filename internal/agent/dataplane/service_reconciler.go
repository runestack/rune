package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// reconcileServicesFromStore registers VIP listeners for every
// non-deleted service in the store. Called at dataplane start and on
// each service watch event.
func (s *Subsystem) reconcileServicesFromStore(ctx context.Context) error {
	if s.cfg.Store == nil {
		return nil
	}
	var services []types.Service
	if err := s.cfg.Store.ListAll(ctx, types.ResourceTypeService, &services); err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	for i := range services {
		svc := services[i]
		if err := s.reconcileOneService(ctx, &svc, false); err != nil {
			s.log.Warn("Initial service dataplane reconcile failed",
				log.Str("namespace", svc.Namespace),
				log.Str("service", svc.Name),
				log.Err(err))
		}
	}
	return nil
}

func (s *Subsystem) runServiceWatch(ctx context.Context, initial <-chan store.WatchEvent) {
	defer s.svcWg.Done()
	ch := initial
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				var err error
				ch, err = s.cfg.Store.Watch(ctx, types.ResourceTypeService, "")
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					s.log.Warn("Service watch reconnect failed", log.Err(err))
					continue
				}
				continue
			}
			s.handleServiceWatchEvent(ctx, ev)
		}
	}
}

func (s *Subsystem) handleServiceWatchEvent(ctx context.Context, ev store.WatchEvent) {
	switch ev.Type {
	case store.WatchEventDeleted:
		svc, ok := ev.Resource.(*types.Service)
		if !ok || svc == nil {
			return
		}
		s.unregisterServiceDataplane(svc)
		return
	case store.WatchEventCreated, store.WatchEventUpdated:
		svc, ok := ev.Resource.(*types.Service)
		if !ok || svc == nil {
			return
		}
		if svc.Status == types.ServiceStatusDeleted {
			s.unregisterServiceDataplane(svc)
			return
		}
		if err := s.reconcileOneService(ctx, svc, true); err != nil {
			s.log.Warn("Service dataplane reconcile failed",
				log.Str("namespace", svc.Namespace),
				log.Str("service", svc.Name),
				log.Err(err))
		}
	}
}

func (s *Subsystem) reconcileOneService(ctx context.Context, svc *types.Service, backfillStore bool) error {
	if svc == nil || svc.ID == "" {
		return nil
	}
	if len(svc.Ports) == 0 {
		return nil
	}
	hadVIP := svc.Discovery != nil && svc.Discovery.VIP != ""
	if err := enrichServiceVIP(ctx, svc, s.cfg.VIPResolver); err != nil {
		return err
	}
	if backfillStore && !hadVIP && svc.Discovery != nil && svc.Discovery.VIP != "" && s.cfg.Store != nil {
		if err := s.cfg.Store.Update(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc); err != nil {
			s.log.Warn("Failed to backfill discovery.vip on service row",
				log.Str("namespace", svc.Namespace),
				log.Str("service", svc.Name),
				log.Err(err))
		}
	}
	return s.registerServiceDataplane(svc)
}

func (s *Subsystem) registerServiceDataplane(svc *types.Service) error {
	newVIP := ""
	if svc.Discovery != nil {
		newVIP = svc.Discovery.VIP
	}

	s.svcRegMu.Lock()
	oldVIP, wasRegistered := s.svcRegistered[svc.ID]
	if wasRegistered && oldVIP != newVIP {
		s.svcRegMu.Unlock()
		s.UnregisterService(svc.ID)
		if oldVIP != "" && s.cfg.Mode == ModeProduction {
			s.vipHost.remove(net.ParseIP(oldVIP))
		}
		s.svcRegMu.Lock()
		delete(s.svcRegistered, svc.ID)
	}
	s.svcRegMu.Unlock()

	if err := s.RegisterService(svc); err != nil {
		return err
	}

	needVIPAdd := !wasRegistered || oldVIP != newVIP
	if needVIPAdd && s.cfg.Mode == ModeProduction && newVIP != "" {
		ip := net.ParseIP(newVIP)
		if ip != nil {
			if err := s.vipHost.add(ip); err != nil {
				s.log.Warn("Failed to add service VIP on loopback",
					log.Str("service", svc.Name),
					log.Str("vip", newVIP),
					log.Err(err))
			}
		}
	}

	s.svcRegMu.Lock()
	s.svcRegistered[svc.ID] = newVIP
	s.svcRegMu.Unlock()
	return nil
}

func (s *Subsystem) teardownServiceVIPs() {
	s.svcRegMu.Lock()
	registered := s.svcRegistered
	s.svcRegistered = make(map[string]string)
	s.svcRegMu.Unlock()
	if s.cfg.Mode != ModeProduction {
		return
	}
	for _, vip := range registered {
		if vip != "" {
			s.vipHost.remove(net.ParseIP(vip))
		}
	}
}

func (s *Subsystem) unregisterServiceDataplane(svc *types.Service) {
	if svc == nil {
		return
	}
	s.svcRegMu.Lock()
	vip, wasRegistered := s.svcRegistered[svc.ID]
	if wasRegistered {
		delete(s.svcRegistered, svc.ID)
	} else if svc.Discovery != nil {
		vip = svc.Discovery.VIP
	}
	s.svcRegMu.Unlock()

	s.UnregisterService(svc.ID)
	if wasRegistered && vip != "" && s.cfg.Mode == ModeProduction {
		s.vipHost.remove(net.ParseIP(vip))
	}
}
