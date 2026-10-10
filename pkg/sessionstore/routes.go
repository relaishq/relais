package sessionstore

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

// Route is authenticated routing evidence, never a cached worker address.
// Generation is the lease epoch. ExpiresAt is the consent deadline, independent
// of lease and snapshot TTLs. Restoring or moving must not renew this evidence.
type Route struct {
	Caller            netip.AddrPort `json:"caller"`
	SessionID         string         `json:"session_id"`
	Generation        uint64         `json:"generation"`
	ConfirmedAt       time.Time      `json:"confirmed_at"`
	LastAuthenticated time.Time      `json:"last_authenticated"`
	ExpiresAt         time.Time      `json:"expires_at"`
}

var ErrRouteLimit = errors.New("sessionstore: persisted route limit exceeded")

// Routes is the relay's optional persistence capability. It grants no snapshot
// access or lease mutations. Implemented by Memory, Redis and RedisOwners.
type Routes interface {
	Get(context.Context, string) (Lease, error)
	// PutRoute is fenced by the live lease epoch; a newer confirmation wins
	// over a delayed write. Nomination replaces other addresses for the session.
	PutRoute(context.Context, Route, bool) error
	// LoadRoutes is a bounded startup scan. An excess fails closed instead of
	// leaving active addresses unprotected. Expired evidence is omitted.
	LoadRoutes(context.Context, int) ([]Route, error)
	// DeleteRoute compares the whole record, so stale cleanup cannot remove
	// a newer confirmation, renewal or generation.
	DeleteRoute(context.Context, Route) error
	ForgetRoutes(context.Context, string) error
}

// RouteRetention is a cleanup margin only; it never extends consent.
const RouteRetention = time.Second

func validRoute(r Route) bool {
	return r.Caller.IsValid() && r.SessionID != "" && r.Generation > 0 &&
		!r.ConfirmedAt.IsZero() && !r.LastAuthenticated.IsZero() &&
		r.ExpiresAt.After(r.LastAuthenticated)
}

func newerRoute(a, b Route) bool {
	if a.SessionID != b.SessionID {
		return a.ConfirmedAt.After(b.ConfirmedAt)
	}
	if a.Generation != b.Generation {
		return a.Generation > b.Generation
	}
	if !a.ConfirmedAt.Equal(b.ConfirmedAt) {
		return a.ConfirmedAt.After(b.ConfirmedAt)
	}
	return !a.LastAuthenticated.Before(b.LastAuthenticated)
}

func (m *Memory) PutRoute(ctx context.Context, route Route, nominated bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRoute(route) {
		return errors.New("sessionstore: invalid route")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[route.SessionID]
	if !ok || lease.Epoch != route.Generation || !time.Now().Before(lease.ExpiresAt) {
		return ErrLeaseLost
	}
	if !time.Now().Before(route.ExpiresAt) {
		return ErrNotFound
	}
	if old, ok := m.routes[route.Caller]; ok && !newerRoute(route, old) {
		return nil
	}
	if marker, ok := m.routeNominations[route.SessionID]; ok && marker.Generation == route.Generation && marker.ConfirmedAt.After(route.ConfirmedAt) {
		return nil
	}
	if nominated {
		for caller := range m.routeSessions[route.SessionID] {
			old := m.routes[caller]
			if old.ConfirmedAt.After(route.ConfirmedAt) {
				return nil
			}
		}
		m.routeNominations[route.SessionID] = route
		for caller := range m.routeSessions[route.SessionID] {
			m.dropRoute(caller)
		}
	}
	m.dropRoute(route.Caller)
	m.routes[route.Caller] = route
	if m.routeSessions[route.SessionID] == nil {
		m.routeSessions[route.SessionID] = make(map[netip.AddrPort]struct{})
	}
	m.routeSessions[route.SessionID][route.Caller] = struct{}{}
	return nil
}

func (m *Memory) LoadRoutes(ctx context.Context, limit int) ([]Route, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, ErrRouteLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	routes := []Route{}
	for id, marker := range m.routeNominations {
		if !time.Now().Before(marker.ExpiresAt) {
			delete(m.routeNominations, id)
		}
	}
	for caller, route := range m.routes {
		lease, ok := m.leases[route.SessionID]
		if !time.Now().Before(route.ExpiresAt) || !ok || lease.Epoch != route.Generation || !time.Now().Before(lease.ExpiresAt) {
			m.dropRoute(caller)
			continue
		}
		if len(routes) >= limit {
			return nil, ErrRouteLimit
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func (m *Memory) DeleteRoute(ctx context.Context, route Route) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.routes[route.Caller] == route {
		m.dropRoute(route.Caller)
	}
	return nil
}

func (m *Memory) dropRoute(caller netip.AddrPort) {
	route, ok := m.routes[caller]
	if !ok {
		return
	}
	delete(m.routes, caller)
	delete(m.routeSessions[route.SessionID], caller)
	if len(m.routeSessions[route.SessionID]) == 0 {
		delete(m.routeSessions, route.SessionID)
	}
}
func (m *Memory) forgetRoutes(id string) {
	delete(m.routeNominations, id)
	for caller := range m.routeSessions[id] {
		m.dropRoute(caller)
	}
}

func (m *Memory) ForgetRoutes(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forgetRoutes(id)
	return nil
}
