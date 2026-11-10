// Package supervisor manages DuckDB+Quack child processes.
package supervisor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alitrack/quack-proxy/internal/config"
	"github.com/alitrack/quack-proxy/internal/health"
)

const (
	quackBootSQL = `LOAD quack;
%s
CALL quack_serve('quack:%s:%d', token = '%s', allow_other_hostname = true, disable_ssl = true);
`

	// shutdownTimeout bounds the graceful (SIGTERM) stop of one shard before
	// the process group is hard-killed.
	shutdownTimeout = 10 * time.Second

	// maxRestarts is the number of failed restart attempts after which a
	// shard is parked in "stopped" instead of being relaunched forever. The
	// PRD promises backoff, not infinite relaunch: a shard that cannot come
	// up (bad token, missing extension, held port) must stop churning and be
	// reported as stopped.
	maxRestarts = 10
	// initialBackoff and maxBackoff bound the exponential delay between
	// failed restarts (1s, 2s, 4s, ... capped at 30s).
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second

	// defaultStartupGrace is how long after a (re)start a shard may stay
	// unhealthy without triggering another restart. DuckDB's first boot can
	// spend tens of seconds on INSTALL/LOAD before quack_serve binds;
	// restarting inside that window would kill a shard that is still coming
	// up and turn a slow boot into a restart loop.
	defaultStartupGrace = 30 * time.Second
)

// lookupBin resolves bin the way the shell would (honouring PATH) and
// reports whether it is an executable file. It lets the supervisor fail
// loudly at boot instead of forking a bash that dies with "command not
// found".
func lookupBin(bin string) (string, bool) {
	if filepath.IsAbs(bin) {
		if fi, err := os.Stat(bin); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return bin, true
		}
		return "", false
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		full := filepath.Join(dir, bin)
		if fi, err := os.Stat(full); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return full, true
		}
	}
	return "", false
}

// buildBootSQL assembles the SQL fed to a shard's DuckDB process:
// LOAD quack first, then the shard's init_sql (extensions/hooks), then
// quack_serve. init_sql MUST run after LOAD quack: the quack_* settings
// (e.g. quack_authentication_function) only exist once the extension is
// loaded — setting them earlier is silently dropped.
func buildBootSQL(bindHost string, cfg config.ShardConfig, token string) (string, error) {
	init := cfg.InitSQL
	if cfg.InitSQLFile != "" {
		// A missing init_sql_file is a configuration error: fail loudly at
		// boot instead of starting a shard without its intended hooks.
		data, err := os.ReadFile(cfg.InitSQLFile)
		if err != nil {
			return "", fmt.Errorf("read init_sql_file %s: %w", cfg.InitSQLFile, err)
		}
		if init != "" {
			init += "\n"
		}
		init += string(data)
	}
	return fmt.Sprintf(quackBootSQL, init, bindHost, cfg.Port, token), nil
}

// StatusFileRecord is one shard entry in the on-disk status file that the
// daemon writes so out-of-process commands (status, gen-proxy) can read live
// cluster state and — critically — the auto-generated tokens.
type StatusFileRecord struct {
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Status    string `json:"status"`
	PID       int    `json:"pid"`
	StartTime string `json:"start_time"`
	Restarts  int    `json:"restarts"`
	Token     string `json:"token"`
	Database  string `json:"database"`
	LastCheck string `json:"last_health_check,omitempty"`
	AttachSQL string `json:"coordinator_attach_sql"`
}

// StatusFile is the daemon's on-disk view of the cluster.
type StatusFile struct {
	Updated string             `json:"updated"`
	Shards  []StatusFileRecord `json:"shards"`
}

type Supervisor struct {
	cfg    *config.Config
	shards map[string]*ShardProcess
	mu     sync.RWMutex
	logger *slog.Logger
	cancel context.CancelFunc
	// startupGrace is how long a freshly (re)started shard gets to come up
	// before a health failure triggers another restart. It is a field (not a
	// bare constant) so tests can shorten or disable it.
	startupGrace time.Duration
}

type ShardProcess struct {
	Config    config.ShardConfig
	cmd       *exec.Cmd
	PID       int
	Status    string // "starting", "healthy", "unhealthy", "stopped"
	StartTime time.Time
	Restarts  int
	lastCheck time.Time
	// lastUnhealthy and backoff track the current consecutive-failure
	// streak so restarts space out instead of storming.
	lastUnhealthy time.Time
	backoff       time.Duration
	// lastStart bounds the per-shard startup grace (see Supervisor
	// .startupGrace) so a slow first boot is not killed mid-install.
	lastStart time.Time
}

func New(cfg *config.Config, logger *slog.Logger) *Supervisor {
	return &Supervisor{
		cfg:          cfg,
		shards:       make(map[string]*ShardProcess),
		logger:       logger,
		startupGrace: defaultStartupGrace,
	}
}

// ApplyConfig hot-swaps in a reloaded configuration: stops shards no longer
// configured and starts newly added ones, then persists the new status.
// Kept shards continue running with their existing process and token.
func (s *Supervisor) ApplyConfig(ctx context.Context, cfg *config.Config) (added, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Install the new config before touching any shard so that shards
	// (re)started below see the reloaded bind_host / duckdb_bin / ports.
	s.cfg = cfg

	removedSet := make(map[string]bool)
	for name := range s.shards {
		found := false
		for _, sh := range cfg.Shards {
			if sh.Name == name {
				found = true
				break
			}
		}
		if !found {
			removedSet[name] = true
		}
	}
	for name := range removedSet {
		s.logger.Info("shard removed by reload, stopping", "name", name)
		s.stopShardLocked(name, s.shards[name])
		delete(s.shards, name)
		removed = append(removed, name)
	}

	existing := make(map[string]bool, len(s.shards))
	for name := range s.shards {
		existing[name] = true
	}
	for _, sh := range cfg.Shards {
		if existing[sh.Name] {
			continue
		}
		if err := s.startShardLocked(ctx, sh); err != nil {
			s.logger.Error("reload: failed to start shard", "name", sh.Name, "error", err)
			continue
		}
		s.logger.Info("shard added by reload, started", "name", sh.Name)
		added = append(added, sh.Name)
	}

	s.writeStatusFileLocked()
	return
}

// WriteStatusFile persists the current cluster state to the configured
// status file so external commands can read live shards and tokens. It is a
// best-effort side effect: a failure to write is logged, not fatal.
func (s *Supervisor) WriteStatusFile() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.writeStatusFileLocked()
}

// writeStatusFileLocked does the actual write; callers must hold s.mu.
func (s *Supervisor) writeStatusFileLocked() {
	sf := StatusFile{Updated: time.Now().Format(time.RFC3339)}
	for _, sp := range s.shards {
		rec := StatusFileRecord{
			Name:      sp.Config.Name,
			Port:      sp.Config.Port,
			Status:    sp.Status,
			PID:       sp.PID,
			Restarts:  sp.Restarts,
			Token:     sp.Config.Token,
			Database:  sp.Config.Database,
			StartTime: sp.StartTime.Format(time.RFC3339),
		}
		if !sp.lastCheck.IsZero() {
			rec.LastCheck = sp.lastCheck.Format(time.RFC3339)
		}
		if sp.Status == "healthy" {
			rec.AttachSQL = fmt.Sprintf("ATTACH 'quack:%s:%d' AS %s;",
				s.cfg.Listener.BindHost, sp.Config.Port, sp.Config.Name)
		}
		sf.Shards = append(sf.Shards, rec)
	}
	data, _ := json.MarshalIndent(sf, "", "  ")
	_ = os.WriteFile(s.cfg.Global.StatusFile, append(data, '\n'), 0644)
}

// ReadStatusFile loads a cluster state file written by WriteStatusFile.
func ReadStatusFile(path string) (*StatusFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sf StatusFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &sf, nil
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
	s.writeStatusFileLocked()
	return nil
}

func (s *Supervisor) startShardLocked(ctx context.Context, cfg config.ShardConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Fail loud if the configured engine is missing or not executable:
	// otherwise the forked bash dies with "command not found" and the shard
	// looks "started" until the health loop notices (and may restart-loop).
	if _, ok := lookupBin(s.cfg.Global.DuckDBBin); !ok {
		return fmt.Errorf("duckdb_bin %q not found on PATH or not executable", s.cfg.Global.DuckDBBin)
	}

	token := cfg.Token
	if token == "" {
		token = randomToken(32)
	}

	sql, err := buildBootSQL(s.cfg.Listener.BindHost, cfg, token)
	if err != nil {
		return err
	}

	// Feed boot SQL via a FIFO-safe pipeline: write the SQL to a temp file,
	// then `(cat file; tail -f /dev/null) | duckdb ...`. File contents pass
	// through VERBATIM — no shell quoting, so multi-line init_sql (Lua
	// policies with comments, dollar-quoted sources) survives intact.
	// (An earlier `(echo '...'; tail)` variant collapsed every newline,
	// turning Lua `--` comments into "comment out the whole program".)
	sqlFile := filepath.Join(os.TempDir(), fmt.Sprintf("quack-proxy-%s-%d.sql", cfg.Name, cfg.Port))
	if err := os.WriteFile(sqlFile, []byte(sql), 0600); err != nil {
		return fmt.Errorf("write boot sql: %w", err)
	}
	shellCmd := fmt.Sprintf(
		`(cat '%s'; tail -f /dev/null) | '%s' -unsigned '%s'`,
		sqlFile,
		s.cfg.Global.DuckDBBin,
		cfg.Database,
	)

	cmd := exec.Command("bash", "-c", shellCmd)
	// Give the child its own stdout/stderr and drain them in goroutines.
	// Sharing os.Stdout/os.Stderr (the previous behaviour) left a pipe held
	// open for the life of `tail -f /dev/null`, so the parent's own stdout
	// could never be closed — which wedged the process and failed the test
	// suite via exec.ErrWaitDelay.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.Remove(sqlFile)
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.Remove(sqlFile)
		return fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.SysProcAttr = setProcessGroup()
	cmd.Cancel = nil // we manage the process group ourselves; see stopShardLocked

	if err := cmd.Start(); err != nil {
		os.Remove(sqlFile)
		return fmt.Errorf("start duckdb: %w", err)
	}

	go drainLog("shard="+cfg.Name, stdout)
	go drainLog("shard="+cfg.Name, stderr)

	sp := &ShardProcess{
		Config:        cfg,
		cmd:           cmd,
		PID:           cmd.Process.Pid,
		Status:        "starting",
		StartTime:     time.Now(),
		lastStart:     time.Now(),
		backoff:       initialBackoff,
		lastUnhealthy: time.Time{},
	}
	sp.Config.Token = token
	s.shards[cfg.Name] = sp
	return nil
}

// drainLog copies a child's output to the daemon log, prefixing each line so
// per-shard output stays attributable. It runs until the child's pipe
// closes. It reads line-by-line with no fixed line cap (a Scanner would
// silently drop lines over its token size and the pipe would then fill and
// stall the child): every byte the child writes is consumed and forwarded.
func drainLog(prefix string, r io.Reader) {
	rd := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			fmt.Fprintf(os.Stderr, "[%s] %s\n", prefix, strings.TrimRight(line, "\n"))
		}
		if err != nil { // io.EOF or broken pipe: the child's stream is over
			return
		}
	}
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

// stopShardLocked stops one shard's whole process group, waits for graceful
// exit, then hard-kills whatever is left. It is always called with s.mu held.
func (s *Supervisor) stopShardLocked(name string, sp *ShardProcess) {
	if sp.cmd != nil && sp.cmd.Process != nil {
		pid := sp.cmd.Process.Pid
		// Ask the whole group (bash + duckdb + tail) to exit.
		signalProcessGroup(pid, syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- sp.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
			// Graceful stop timed out; kill the entire group.
			signalProcessGroup(pid, syscall.SIGKILL)
			<-done
		}
	}
	// Remove the per-shard boot SQL temp file.
	sqlFile := filepath.Join(os.TempDir(), fmt.Sprintf("quack-proxy-%s-%d.sql", sp.Config.Name, sp.Config.Port))
	os.Remove(sqlFile)
	sp.cmd = nil
	sp.PID = 0
	sp.Status = "stopped"
	s.logger.Info("shard stopped", "name", name)
}

// healthInterval returns the configured health-check interval; callers must
// not hold s.mu.
func (s *Supervisor) healthInterval() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d := s.cfg.Listener.HealthInterval; d > 0 {
		return d
	}
	return 5 * time.Second
}

func (s *Supervisor) HealthLoop(ctx context.Context) {
	// Initial grace period: give DuckDB time to install extensions and start Quack
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}

	cur := s.healthInterval()
	ticker := time.NewTicker(cur)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkAll(ctx)
			// A SIGHUP reload may have changed health_interval; pick it up
			// without restarting the daemon.
			if next := s.healthInterval(); next != cur {
				ticker.Stop()
				cur = next
				ticker = time.NewTicker(cur)
			}
		}
	}
}

// checkAll health-checks every running shard. Unhealthy shards are restarted,
// but with a bounded, exponentially-spaced retry (maxRestarts, backoff) so a
// persistently-failing shard parks itself instead of storming forever.
func (s *Supervisor) checkAll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
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
		sp.lastCheck = now
		if ok {
			sp.Status = "healthy"
			sp.backoff = initialBackoff
			sp.lastUnhealthy = time.Time{}
			continue
		}

		// A fresh (re)start is inside its startup grace: the engine may
		// still be installing/loading before quack_serve binds. Let it
		// finish instead of restarting it mid-boot.
		if now.Sub(sp.lastStart) < s.startupGrace {
			sp.Status = "starting"
			continue
		}

		// Unhealthy: is it time to (re)try, given the current streak?
		if !sp.lastUnhealthy.IsZero() && now.Sub(sp.lastUnhealthy) < sp.backoff {
			sp.Status = "unhealthy"
			continue // still inside the current backoff window
		}
		sp.lastUnhealthy = now
		attempts := sp.Restarts + 1

		s.logger.Warn("shard unhealthy, restarting", "name", name, "attempt", attempts)
		s.stopShardLocked(name, sp)
		// startShardLocked installs a fresh ShardProcess in the map and
		// returns before touching this one on error, so re-fetch the live
		// entry to keep the retry count and backoff on the real process.
		err := s.startShardLocked(ctx, sp.Config)
		var live *ShardProcess
		if err != nil {
			s.logger.Error("failed to restart shard", "name", name, "error", err)
			live = sp // the stopped entry remains in the map
			live.Status = "unhealthy"
		} else {
			live = s.shards[name]
			live.Status = "starting"
		}
		live.Restarts = attempts
		live.backoff = min(sp.backoff*2, maxBackoff)
		live.lastUnhealthy = now

		if attempts >= maxRestarts {
			s.logger.Error("shard failed to stabilize, stopping retries", "name", name, "attempts", attempts)
			s.stopShardLocked(name, live)
			live.Status = "stopped"
		}
	}

	s.writeStatusFileLocked()
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

// randomToken returns n cryptographically random characters from a
// 62-symbol alphabet. (The previous implementation derived every character
// from time.Now().UnixNano(), which made tokens a short predictable
// arithmetic run with only a few thousand distinct values — an unsafe
// credential for the sole auth token of a 0.0.0.0-bound service.)
func randomToken(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	max := big.NewInt(int64(len(letters)))
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(fmt.Sprintf("randomToken: %v", err))
		}
		b[i] = letters[idx.Int64()]
	}
	return string(b)
}
