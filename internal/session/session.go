package session

import "sync"

type Step int

const (
	StepIdle Step = iota
	StepMinPrice
	StepMaxPrice
	StepMinArea
	StepMaxArea
	StepRooms
	StepMinScore
)

type Session struct {
	Step     Step
	MinPrice int
	MaxPrice int
	MinArea  float64
	MaxArea  float64
	Rooms    []int32
	MinScore int
}

type Store struct {
	mu       sync.Mutex
	sessions map[int64]*Session
}

func NewStore() *Store {
	return &Store{sessions: make(map[int64]*Session)}
}

func (s *Store) Get(chatID int64) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[chatID]
}

func (s *Store) Set(chatID int64, sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[chatID] = sess
}

func (s *Store) Delete(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, chatID)
}