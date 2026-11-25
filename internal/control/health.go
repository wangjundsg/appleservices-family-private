package control

import "time"

const PendingMaxAge = 5 * time.Minute
const SessionFreshFor = 15 * time.Minute

func PendingFresh(p *Pending, now time.Time) bool {
	return p != nil && !p.ReceivedAt.IsZero() && !p.ReceivedAt.After(now) && now.Sub(p.ReceivedAt) <= PendingMaxAge
}

func SessionFresh(state string, authenticated bool, checkedAt time.Time, now time.Time) bool {
	return state == "online" && authenticated && !checkedAt.IsZero() && !checkedAt.After(now) && now.Sub(checkedAt) <= SessionFreshFor
}

func (s *Server) RequestFresh(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PendingFresh(s.pending, time.Now()) && s.pending.ID == id
}
