package api

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

// fakeStore keeps wallets and reservations in memory with the behaviour the Store
// interface promises, so the handlers can be tested without a database.
type fakeStore struct {
	mu           sync.Mutex
	available    map[string]int64
	reservations map[string]*fakeReservation // by reservation id
	finds        int
	fail         error // returned by every method when set
}

type fakeReservation struct {
	Reservation
	settled *Settled
	digest  []byte // of the settlement
}

func newFakeStore(wallets map[string]int64) *fakeStore {
	return &fakeStore{available: wallets, reservations: map[string]*fakeReservation{}}
}

func (s *fakeStore) byKey(callID string, attemptID int32) *fakeReservation {
	for _, r := range s.reservations {
		if r.CallID == callID && r.AttemptID == attemptID {
			return r
		}
	}
	return nil
}

func (s *fakeStore) FindReservation(_ context.Context, callID string, attemptID int32) (Reservation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finds++
	if s.fail != nil {
		return Reservation{}, false, s.fail
	}
	if r := s.byKey(callID, attemptID); r != nil {
		return r.Reservation, true, nil
	}
	return Reservation{}, false, nil
}

func (s *fakeStore) Reserve(_ context.Context, r Reservation) (Reservation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return Reservation{}, false, s.fail
	}
	if existing := s.byKey(r.CallID, r.AttemptID); existing != nil {
		return existing.Reservation, false, nil
	}
	available, ok := s.available[r.TenantID]
	if !ok {
		return Reservation{}, false, ErrWalletNotFound
	}
	if available < r.MaxCost {
		return Reservation{}, false, ErrInsufficientBalance
	}
	s.available[r.TenantID] = available - r.MaxCost
	r.ID = fmt.Sprintf("res_%d", len(s.reservations)+1)
	s.reservations[r.ID] = &fakeReservation{Reservation: r}
	return r, true, nil
}

func (s *fakeStore) Settle(_ context.Context, reservationID string, settlement Settlement) (Settled, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return Settled{}, s.fail
	}
	r, ok := s.reservations[reservationID]
	if !ok {
		return Settled{}, ErrReservationNotFound
	}
	if r.settled != nil {
		if !bytes.Equal(r.digest, settlement.Digest) {
			return Settled{}, ErrConflict
		}
		return *r.settled, nil
	}
	charged := billing.Charge(r.Prices, r.Bounds, r.MaxCost, settlement.Usage, settlement.Outcome)
	s.available[r.TenantID] += r.MaxCost - charged
	r.settled = &Settled{Charged: charged, LedgerEntryID: "led_" + reservationID}
	r.digest = settlement.Digest
	return *r.settled, nil
}

// fakePrices answers price lookups from a table and counts them.
type fakePrices struct {
	mu       sync.Mutex
	versions map[string]billing.Prices
	err      error
	lookups  int
	lastID   string // the request id of the last lookup
}

func (p *fakePrices) PriceVersion(_ context.Context, id, requestID string) (billing.Prices, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lookups++
	p.lastID = requestID
	if p.err != nil {
		return billing.Prices{}, p.err
	}
	prices, ok := p.versions[id]
	if !ok {
		return billing.Prices{}, fmt.Errorf("no price version %q in the test table", id)
	}
	return prices, nil
}
