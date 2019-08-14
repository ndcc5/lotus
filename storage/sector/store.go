package sector

import (
	"context"
	"github.com/filecoin-project/go-lotus/lib/sectorbuilder"
	"io"
	"io/ioutil"
	"os"
	"sync"
)

// TODO: eventually handle sector storage here instead of in rust-sectorbuilder
type Store struct {
	lk sync.Mutex
	sb *sectorbuilder.SectorBuilder

	waiting  map[uint64]chan struct{}
	incoming []chan sectorbuilder.SectorSealingStatus
	// TODO: outdated chan

	close chan struct{}
}

func NewStore(sb *sectorbuilder.SectorBuilder) *Store {
	return &Store{
		sb:      sb,
		waiting: map[uint64]chan struct{}{},
		close:   make(chan struct{}),
	}
}

func (s *Store) Service() {
	go s.service()
}

func (s *Store) service() {
	sealed := s.sb.SealedSectorChan()

	for {
		select {
		case sector := <-sealed:
			s.lk.Lock()
			watch, ok := s.waiting[sector.SectorID]
			if ok {
				close(watch)
				delete(s.waiting, sector.SectorID)
			}
			for _, c := range s.incoming {
				c <- sector // TODO: ctx!
			}
			s.lk.Unlock()
		case <-s.close:
			s.lk.Lock()
			for _, c := range s.incoming {
				close(c)
			}
			s.lk.Unlock()
			return
		}
	}
}

func (s *Store) AddPiece(ref string, size uint64, r io.Reader, keepAtLeast uint64) (sectorID uint64, err error) {
	err = withTemp(r, func(f string) (err error) {
		sectorID, err = s.sb.AddPiece(ref, size, f)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.lk.Lock()
	_, exists := s.waiting[sectorID]
	if !exists {
		s.waiting[sectorID] = make(chan struct{})
	}
	s.lk.Unlock()
	return sectorID, nil
}

func (s *Store) CloseIncoming(c <-chan sectorbuilder.SectorSealingStatus) {
	s.lk.Lock()
	var at = -1
	for i, ch := range s.incoming {
		if ch == c {
			at = i
		}
	}
	if at == -1 {
		s.lk.Unlock()
		return
	}
	if len(s.incoming) > 1 {
		s.incoming[at] = s.incoming[len(s.incoming)-1]
	}
	s.incoming = s.incoming[:len(s.incoming)-1]
	s.lk.Unlock()
}

func (s *Store) Incoming() <-chan sectorbuilder.SectorSealingStatus {
	ch := make(chan sectorbuilder.SectorSealingStatus, 8)
	s.lk.Lock()
	s.incoming = append(s.incoming, ch)
	s.lk.Unlock()
	return ch
}

func (s *Store) WaitSeal(ctx context.Context, sector uint64) (sectorbuilder.SectorSealingStatus, error) {
	s.lk.Lock()
	watch, ok := s.waiting[sector]
	s.lk.Unlock()
	if ok {
		select {
		case <-watch:
		case <-ctx.Done():
			return sectorbuilder.SectorSealingStatus{}, ctx.Err()
		}
	}

	return s.sb.SealStatus(sector)
}

func (s *Store) Stop() {
	close(s.close)
}

func withTemp(r io.Reader, cb func(string) error) error {
	f, err := ioutil.TempFile(os.TempDir(), "lotus-temp-")
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	err = cb(f.Name())
	if err != nil {
		os.Remove(f.Name())
		return err
	}

	return os.Remove(f.Name())
}
