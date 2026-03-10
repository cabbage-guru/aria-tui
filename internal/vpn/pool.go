package vpn

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cabbage-guru/aria-tui/internal/config"
)

// WireGuardConfig represents a WireGuard configuration file.
type WireGuardConfig struct {
	Name          string
	Path          string
	InUse         bool
	Disabled      bool
	CooldownUntil time.Time
	Contents      string
}

// Pool manages WireGuard configurations.
type Pool struct {
	mu       sync.Mutex
	configs  map[string]*WireGuardConfig
	inUse    map[string]bool      // config name -> in use
	disabled map[string]bool      // config name -> manually disabled
	cooldown map[string]time.Time // config name -> cooldown expiry
}

func NewPool() *Pool {
	return &Pool{
		configs:  make(map[string]*WireGuardConfig),
		inUse:    make(map[string]bool),
		disabled: make(map[string]bool),
		cooldown: make(map[string]time.Time),
	}
}

// LoadConfigs reads all .conf files from the wireguard config directory.
func (p *Pool) LoadConfigs() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	dir := config.WireGuardDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading wireguard dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".conf")
		path := filepath.Join(dir, entry.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		if existing, ok := p.configs[name]; ok {
			existing.Contents = string(contents)
			existing.Path = path
		} else {
			p.configs[name] = &WireGuardConfig{
				Name:     name,
				Path:     path,
				Contents: string(contents),
			}
		}
	}

	return nil
}

// AddConfig adds a WireGuard config from raw content.
func (p *Pool) AddConfig(name, contents string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !strings.HasSuffix(name, ".conf") {
		name += ".conf"
	}
	baseName := strings.TrimSuffix(name, ".conf")

	// Sanitize: use only the base filename to prevent path traversal (e.g. "../foo").
	name = filepath.Base(name)
	baseName = filepath.Base(baseName)
	if name == "." || name == ".." || baseName == "" {
		return fmt.Errorf("invalid config name")
	}

	dir := config.WireGuardDir()
	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	p.configs[baseName] = &WireGuardConfig{
		Name:     baseName,
		Path:     path,
		Contents: contents,
	}
	p.disabled[baseName] = true

	return nil
}

// ImportConfig imports a WireGuard .conf file from a given path.
func (p *Pool) ImportConfig(srcPath string) error {
	contents, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("reading source config: %w", err)
	}

	name := filepath.Base(srcPath)
	return p.AddConfig(name, string(contents))
}

// RemoveConfig deletes a WireGuard config.
func (p *Pool) RemoveConfig(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cfg, ok := p.configs[name]
	if !ok {
		return fmt.Errorf("config %q not found", name)
	}

	if p.inUse[name] {
		return fmt.Errorf("config %q is currently in use", name)
	}

	if err := os.Remove(cfg.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing config file: %w", err)
	}

	delete(p.configs, name)
	delete(p.disabled, name)
	delete(p.cooldown, name)
	return nil
}

// Acquire claims an available config for use. Returns nil if none available.
func (p *Pool) Acquire() *WireGuardConfig {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for name, cfg := range p.configs {
		if p.inUse[name] {
			continue
		}
		if p.disabled[name] {
			continue
		}
		if expiry, ok := p.cooldown[name]; ok && now.Before(expiry) {
			continue
		}
		// Clear expired cooldown
		delete(p.cooldown, name)
		p.inUse[name] = true
		cfg.InUse = true
		return cfg
	}
	return nil
}

// Release returns a config to the pool.
func (p *Pool) Release(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.inUse, name)
	if cfg, ok := p.configs[name]; ok {
		cfg.InUse = false
	}
}

// Disable manually disables a VPN config. If it's currently marked stale,
// this also clears any cooldown.
func (p *Pool) Disable(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disabled[name] = true
	delete(p.cooldown, name)
}

// Enable manually enables a VPN config and clears any cooldown.
func (p *Pool) Enable(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.disabled, name)
	delete(p.cooldown, name)
}

// IsDisabled returns whether a config is manually disabled.
func (p *Pool) IsDisabled(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disabled[name]
}

// SetCooldown puts a VPN on cooldown for the given duration (e.g., after 429).
func (p *Pool) SetCooldown(name string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldown[name] = time.Now().Add(d)
}

// GetCooldownUntil returns the cooldown expiry for a config, or zero time if none.
func (p *Pool) GetCooldownUntil(name string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cooldown[name]
}

// Available returns the number of configs not currently in use, disabled, or on cooldown.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	count := 0
	for name := range p.configs {
		if p.inUse[name] {
			continue
		}
		if p.disabled[name] {
			continue
		}
		if expiry, ok := p.cooldown[name]; ok && now.Before(expiry) {
			continue
		}
		count++
	}
	return count
}

// Total returns the total number of configs.
func (p *Pool) Total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.configs)
}

// InUseCount returns the number of configs currently in use.
func (p *Pool) InUseCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inUse)
}

// List returns all configs sorted by name with current state.
func (p *Pool) List() []*WireGuardConfig {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	result := make([]*WireGuardConfig, 0, len(p.configs))
	for _, cfg := range p.configs {
		c := *cfg
		c.InUse = p.inUse[cfg.Name]
		c.Disabled = p.disabled[cfg.Name]
		if expiry, ok := p.cooldown[cfg.Name]; ok && now.Before(expiry) {
			c.CooldownUntil = expiry
		}
		result = append(result, &c)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}
