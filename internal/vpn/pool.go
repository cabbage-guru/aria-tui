package vpn

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cabbage-guru/aria-tui/internal/config"
)

// WireGuardConfig represents a WireGuard configuration file.
type WireGuardConfig struct {
	Name     string
	Path     string
	InUse    bool
	Contents string
}

// Pool manages WireGuard configurations.
type Pool struct {
	mu      sync.Mutex
	configs map[string]*WireGuardConfig
	inUse   map[string]bool // config name -> in use
}

func NewPool() *Pool {
	return &Pool{
		configs: make(map[string]*WireGuardConfig),
		inUse:   make(map[string]bool),
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
	return nil
}

// Acquire claims an available config for use. Returns nil if none available.
func (p *Pool) Acquire() *WireGuardConfig {
	p.mu.Lock()
	defer p.mu.Unlock()

	for name, cfg := range p.configs {
		if !p.inUse[name] {
			p.inUse[name] = true
			cfg.InUse = true
			return cfg
		}
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

// Available returns the number of configs not currently in use.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	count := 0
	for name := range p.configs {
		if !p.inUse[name] {
			count++
		}
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

// List returns all configs sorted by name.
func (p *Pool) List() []*WireGuardConfig {
	p.mu.Lock()
	defer p.mu.Unlock()

	result := make([]*WireGuardConfig, 0, len(p.configs))
	for _, cfg := range p.configs {
		c := *cfg
		c.InUse = p.inUse[cfg.Name]
		result = append(result, &c)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}
