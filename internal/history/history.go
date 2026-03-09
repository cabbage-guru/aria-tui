package history

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/cabbage-guru/aria-tui/internal/config"
)

// Entry represents a completed/failed download in history.
type Entry struct {
	URL         string    `json:"url"`
	Filename    string    `json:"filename"`
	Status      string    `json:"status"` // complete, error, cancelled
	Error       string    `json:"error,omitempty"`
	VPNConfig   string    `json:"vpn_config"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	TotalSize   int64     `json:"total_size"`
}

// Store manages download history persistence.
type Store struct {
	mu      sync.Mutex
	entries []Entry
}

func NewStore() *Store {
	s := &Store{}
	s.Load()
	return s
}

// Load reads history from disk.
func (s *Store) Load() {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(config.HistoryPath())
	if err != nil {
		s.entries = make([]Entry, 0)
		return
	}

	if err := json.Unmarshal(data, &s.entries); err != nil {
		s.entries = make([]Entry, 0)
	}
}

// Save writes history to disk.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(config.HistoryPath(), data, 0600)
}

// Add appends a new entry and saves.
func (s *Store) Add(entry Entry) {
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	s.mu.Unlock()
	s.Save()
}

// All returns all history entries (newest first).
func (s *Store) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]Entry, len(s.entries))
	copy(result, s.entries)

	// Reverse for newest-first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// Completed returns only completed entries.
func (s *Store) Completed() []Entry {
	all := s.All()
	result := make([]Entry, 0)
	for _, e := range all {
		if e.Status == "complete" {
			result = append(result, e)
		}
	}
	return result
}

// Failed returns only failed/error entries.
func (s *Store) Failed() []Entry {
	all := s.All()
	result := make([]Entry, 0)
	for _, e := range all {
		if e.Status == "error" || e.Status == "cancelled" {
			result = append(result, e)
		}
	}
	return result
}

// Clear removes all history.
func (s *Store) Clear() {
	s.mu.Lock()
	s.entries = make([]Entry, 0)
	s.mu.Unlock()
	s.Save()
}
