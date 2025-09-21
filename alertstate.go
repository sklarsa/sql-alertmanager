package main

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/alertmanager/api/v2/models"
)

func key(labels models.LabelSet) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	b := strings.Builder{}
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(labels[k])
	}
	return b.String()
}

type Manager struct {
	mu       sync.Mutex
	state    map[string]time.Time
	filePath string
}

func NewManager(filePath string) *Manager {
	return &Manager{
		state:    make(map[string]time.Time),
		filePath: filePath,
	}
}

func (m *Manager) Load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // empty state is fine
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.state)
}

func (m *Manager) flushLocked() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, m.filePath) // atomic replace
}

// MarkActive sets firstSeen for a key if not already set
func (m *Manager) MarkActive(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.state[key]; !exists {
		m.state[key] = time.Now()
		return m.flushLocked()
	}
	return nil
}

// MarkResolved deletes the key and persists
func (m *Manager) MarkResolved(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.state[key]; exists {
		delete(m.state, key)
		return m.flushLocked()
	}
	return nil
}

// ShouldFire returns true if now >= firstSeen + forDuration
func (m *Manager) ShouldFire(key string, forDuration time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	first, exists := m.state[key]
	if !exists {
		return false
	}
	return time.Since(first) >= forDuration
}
