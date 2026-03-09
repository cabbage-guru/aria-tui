package docker

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Container represents a running Docker container with WireGuard + aria2c.
type Container struct {
	ID        string
	Name      string
	VPNConfig string
	RPCPort   int
	Created   time.Time
}

// Manager handles Docker container lifecycle.
type Manager struct {
	mu         sync.Mutex
	image      string
	containers map[string]*Container // keyed by container name
	nextPort   int
	downloadDir string
}

func NewManager(image string, downloadDir string) *Manager {
	return &Manager{
		image:       image,
		containers:  make(map[string]*Container),
		nextPort:    6800,
		downloadDir: downloadDir,
	}
}

// EnsureImage checks if the Docker image exists and builds it if needed.
func (m *Manager) EnsureImage(ctx context.Context, dockerfilePath string) error {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", m.image)
	if cmd.Run() == nil {
		return nil // image exists
	}

	cmd = exec.CommandContext(ctx, "docker", "build", "-t", m.image, dockerfilePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("building docker image: %s: %w", string(output), err)
	}
	return nil
}

// StartContainer creates and starts a new container with the given WireGuard config.
func (m *Manager) StartContainer(ctx context.Context, name string, wgConfigContents string) (*Container, error) {
	m.mu.Lock()
	port := m.nextPort
	m.nextPort++
	m.mu.Unlock()

	containerName := fmt.Sprintf("aria-tui-%s", name)

	// Stop any existing container with this name
	_ = m.stopContainer(ctx, containerName)

	// Build docker run args
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--cap-add=NET_ADMIN",
		"--cap-add=SYS_MODULE",
		"--sysctl", "net.ipv4.conf.all.src_valid_mark=1",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=0",
		"-p", fmt.Sprintf("%d:6800", port),
		"-v", fmt.Sprintf("%s:/downloads", m.downloadDir),
	}

	// On Linux, we need --privileged for WireGuard kernel module access
	// On macOS (Docker Desktop), the Linux VM already has the module
	if runtime.GOOS == "linux" {
		args = append(args, "--privileged")
	}

	args = append(args, m.image)

	// Create container
	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("starting container: %s: %w", strings.TrimSpace(string(output)), err)
	}

	containerID := strings.TrimSpace(string(output))

	// Copy WireGuard config into the container
	// Write the config content via docker exec
	configCmd := exec.CommandContext(ctx, "docker", "exec", containerName,
		"sh", "-c", fmt.Sprintf("cat > /config/wg0.conf << 'WGEOF'\n%s\nWGEOF", wgConfigContents))
	if out, err := configCmd.CombinedOutput(); err != nil {
		_ = m.stopContainer(ctx, containerName)
		return nil, fmt.Errorf("writing WireGuard config: %s: %w", string(out), err)
	}

	c := &Container{
		ID:        containerID[:12],
		Name:      containerName,
		VPNConfig: name,
		RPCPort:   port,
		Created:   time.Now(),
	}

	m.mu.Lock()
	m.containers[containerName] = c
	m.mu.Unlock()

	return c, nil
}

// StartContainerWithConfig starts a container using a pre-existing config approach:
// 1. Create container without starting
// 2. Copy config in
// 3. Start the container
func (m *Manager) StartContainerWithConfig(ctx context.Context, name string, wgConfigContents string) (*Container, error) {
	m.mu.Lock()
	port := m.nextPort
	m.nextPort++
	m.mu.Unlock()

	containerName := fmt.Sprintf("aria-tui-%s", name)

	// Stop any existing container with this name
	_ = m.stopContainer(ctx, containerName)

	// Create the container (but don't start it yet)
	createArgs := []string{
		"create",
		"--name", containerName,
		"--cap-add=NET_ADMIN",
		"--cap-add=SYS_MODULE",
		"--sysctl", "net.ipv4.conf.all.src_valid_mark=1",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=0",
		"-p", fmt.Sprintf("%d:6800", port),
		"-v", fmt.Sprintf("%s:/downloads", m.downloadDir),
	}

	if runtime.GOOS == "linux" {
		createArgs = append(createArgs, "--privileged")
	}

	createArgs = append(createArgs, m.image)

	cmd := exec.CommandContext(ctx, "docker", createArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("creating container: %s: %w", strings.TrimSpace(string(output)), err)
	}

	containerID := strings.TrimSpace(string(output))

	// Copy the WireGuard config into the container using docker cp via stdin
	// We use a tar stream approach: write config to a temp location then cp
	cpCmd := exec.CommandContext(ctx, "docker", "cp", "/dev/stdin", containerName+":/config/wg0.conf")
	cpCmd.Stdin = strings.NewReader(wgConfigContents)
	if out, err := cpCmd.CombinedOutput(); err != nil {
		// Fallback: use a temp file approach
		writeCmd := exec.CommandContext(ctx, "sh", "-c",
			fmt.Sprintf("echo '%s' | docker cp /dev/stdin %s:/config/wg0.conf",
				strings.ReplaceAll(wgConfigContents, "'", "'\\''"), containerName))
		if out2, err2 := writeCmd.CombinedOutput(); err2 != nil {
			_ = m.stopContainer(ctx, containerName)
			return nil, fmt.Errorf("copying WireGuard config: %s / %s: %w", string(out), string(out2), err)
		}
	}

	// Start the container
	startCmd := exec.CommandContext(ctx, "docker", "start", containerName)
	if out, err := startCmd.CombinedOutput(); err != nil {
		_ = m.stopContainer(ctx, containerName)
		return nil, fmt.Errorf("starting container: %s: %w", string(out), err)
	}

	c := &Container{
		ID:        containerID[:12],
		Name:      containerName,
		VPNConfig: name,
		RPCPort:   port,
		Created:   time.Now(),
	}

	m.mu.Lock()
	m.containers[containerName] = c
	m.mu.Unlock()

	return c, nil
}

// StopContainer stops and removes a container.
func (m *Manager) StopContainer(ctx context.Context, name string) error {
	m.mu.Lock()
	containerName := fmt.Sprintf("aria-tui-%s", name)
	delete(m.containers, containerName)
	m.mu.Unlock()

	return m.stopContainer(ctx, containerName)
}

func (m *Manager) stopContainer(ctx context.Context, containerName string) error {
	// Stop with a timeout
	stopCmd := exec.CommandContext(ctx, "docker", "stop", "-t", "5", containerName)
	stopCmd.Run() // ignore error - might not exist

	// Remove
	rmCmd := exec.CommandContext(ctx, "docker", "rm", "-f", containerName)
	rmCmd.Run() // ignore error

	return nil
}

// IsRunning checks if a container is still running.
func (m *Manager) IsRunning(ctx context.Context, containerName string) bool {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", containerName)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "true"
}

// Logs returns recent logs from a container.
func (m *Manager) Logs(ctx context.Context, name string) (string, error) {
	containerName := fmt.Sprintf("aria-tui-%s", name)
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", "50", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// StopAll stops all managed containers.
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.Lock()
	names := make([]string, 0, len(m.containers))
	for name := range m.containers {
		names = append(names, name)
	}
	m.containers = make(map[string]*Container)
	m.mu.Unlock()

	for _, name := range names {
		m.stopContainer(ctx, name)
	}
}

// GetContainer returns a container by VPN config name.
func (m *Manager) GetContainer(name string) *Container {
	m.mu.Lock()
	defer m.mu.Unlock()
	containerName := fmt.Sprintf("aria-tui-%s", name)
	return m.containers[containerName]
}
