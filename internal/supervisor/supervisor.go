// Package supervisor manages DuckDB+Quack child processes.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alitrack/quack-proxy/internal/config"
	"github.com/alitrack/quack-proxy/internal/health"
)

const quackBootSQL = `
INSTALL quack FROM core_nightly;
LOAD quack;
CALL quack_serve('quack:%s:%d', token = '%s', allow_other_hostname = true);
`

type Supervisor struct {
	cfg    *config.Config
	shards map[string]*ShardProcess
	mu     sync.RWMutex
	logger *slog.Logger
	cancel context.CancelFunc
}

type ShardProcess struct {
	Config    config.ShardConfig
	cmd       *exec.Cmd
	PID       int
	Status    string // "starting", "healthy", "unhealthy", "stopped"
	StartTime time.Time
	Restarts  int
	lastCheck time.Time
}

func New(cfg *config.Config, logger *slog.Logger) *Supervisor {
	return &Supervisor{
		cfg:    cfg,
		shards: make(map[string]*ShardProcess),
		logger: logger,
	}
}

func (s *Supervisor) StartAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx, s.cancel = context.WithCancel(ctx)

	for _, shardCfg := range s.cfg.Shards {
		if err := s.startShardLocked(ctx, shardCfg); err != nil {
			return fmt.Errorf("start %s: %w", shardCfg.Name, err)
		}
	}

	s.logger.Info("all shards started", "count", len(s.shards))
	return nil
}

func (s *Supervisor) startShardLocked(ctx context.Context, cfg config.ShardConfig) error {
	token := cfg.Token
	if token == "" {
		token = randomToken(32)
	}

	sql := fmt.Sprintf(quackBootSQL, s.cfg.Listener.BindHost, cfg.Port, token)

	// Use shell pipe trick: pipe init SQL then keep stdin open via
	// tail -f /dev/null so duckdb stays alive serving Quack indefinitely.
	shellCmd := fmt.Sprintf(
		`(echo '%s'; tail -f /dev/null) | duckdb '%s'`,
		strings.ReplaceAll(sql, "'", "'\\''"),
		cfg.Database,
	)

	cmd := exec.Command("bash", "-c", shellCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = nil // disable CommandContext's internal context kill goroutine

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start duckdb: %w", err)
	}

	sp := &ShardProcess{
		Config:    cfg,
		cmd:       cmd,
		PID:       cmd.Process.Pid,
		Status:    "starting",
		StartTime: time.Now(),
	}
	sp.Config.Token = token
	s.shards[cfg.Name] = sp
	return nil
}

func (s *Supervisor) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}

	for name, sp := range s.shards {
		s.stopShardLocked(name, sp)
	}
}

func (s *Supervisor) stopShardLocked(name string, sp *ShardProcess) {
	if sp.cmd != nil && sp.cmd.Process != nil {
		sp.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- sp.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			sp.cmd.Process.Kill()
		}
	}
	sp.Status = "stopped"
	s.logger.Info("shard stopped", "name", name)
}

func (s *Supervisor) HealthLoop(ctx context.Context) {
	// Initial grace period: give DuckDB time to install extensions and start Quack
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}

	ticker := time.NewTicker(s.cfg.Listener.HealthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkAll(ctx)
		}
	}
}

func (s *Supervisor) checkAll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for name, sp := range s.shards {
		if sp.Status == "stopped" {
			continue
		}
		ok := health.Check(
			s.cfg.Listener.BindHost,
			sp.Config.Port,
			s.cfg.Listener.HealthPath,
			2*time.Second,
		)
		sp.lastCheck = time.Now()
		if ok {
			sp.Status = "healthy"
		} else {
			sp.Status = "unhealthy"
			s.logger.Warn("shard unhealthy, restarting", "name", name)
			s.stopShardLocked(name, sp)
			if err := s.startShardLocked(ctx, sp.Config); err != nil {
				s.logger.Error("failed to restart shard", "name", name, "error", err)
			}
			sp.Restarts++
		}
	}
}

func (s *Supervisor) Status() []ShardProcess {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ShardProcess, 0, len(s.shards))
	for _, sp := range s.shards {
		result = append(result, *sp)
	}
	return result
}

// ManualSetShard is a test helper to inject shard state without starting a real DuckDB process.
func (s *Supervisor) ManualSetShard(name string, sp ShardProcess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spCopy := sp
	s.shards[name] = &spCopy
}

func (s *Supervisor) AttachSQL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sql string
	for _, sp := range s.shards {
		if sp.Status == "healthy" {
			sql += fmt.Sprintf("ATTACH 'quack:%s:%d' AS %s;\n",
				s.cfg.Listener.BindHost, sp.Config.Port, sp.Config.Name)
		}
	}
	return sql
}

func randomToken(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
	}
	return string(b)
}
