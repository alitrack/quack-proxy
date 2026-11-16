// Package supervisor manages DuckDB+Quack child processes.
package supervisor

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
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
CALL quack_serve('quack:%s:%d', token = '%s', allow_other_hostname = %t, disable_ssl = true);
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

	// probeConcurrency bounds simultaneous health probes. Probing N shards
	// serially at 2s each used to hold the supervisor mutex for 2*N seconds,
	// stalling status, reload and shutdown (R-03).
	probeConcurrency = 8
	probeTimeout     = 2 * time.Second
)

// connectHost maps a bind-only address to a dialable one. 0.0.0.0 and ::
// are valid listen addresses but cannot be used as client targets.
func connectHost(bind string) string {
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		return "127.0.0.1"
	}
	return bind
}

// lookupBin resolves bin the way the shell would (honouring PATH) and
// reports whether it is an executable file. It lets the supervisor fail
// loudly at boot instead of forking a bash that dies with "command not
// found".
func lookupBin(bin string) (string, bool) {
	// Executability is platform-specific (see attr_unix.go / attr_windows.go).
	// Using POSIX mode bits here made every absolute engine path fail on
	// Windows with a misleading "not executable" error (R-18).
	if filepath.IsAbs(bin) {
		if fi, err := os.Stat(bin); err == nil && executableFile(fi) {
			return bin, true
		}
		return "", false
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		full := filepath.Join(dir, bin)
		if fi, err := os.Stat(full); err == nil && executableFile(fi) {
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
func buildBootSQL(bindHost string, cfg config.ShardConfig, token string, allowOtherHostname bool) (string, error) {
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
	return fmt.Sprintf(quackBootSQL, init, bindHost, cfg.Port, token, allowOtherHostname), nil
}

// StatusFileRecord is one shard entry in the on-disk status file that the
// daemon writes so out-of-process commands (status, gen-proxy) can read live
// cluster state. Token is present ONLY when global.persist_tokens is true
// (PRD §4.2): by default no shard credential is written to disk at all, and
// omitempty keeps even the empty field out of the JSON.
type StatusFileRecord struct {
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Status    string `json:"status"`
	PID       int    `json:"pid"`
	StartTime string `json:"start_time"`
	Restarts  int    `json:"restarts"`
	Token     string `json:"token,omitempty"`
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

	// checkFn and startFn are injectable seams for the health probe and the
	// shard launcher. Regression tests use them to drive the state machine
	// without forking a real DuckDB process.
	checkFn func(host string, port int, path string, timeout time.Duration, expect string) bool
	startFn func(ctx context.Context, cfg config.ShardConfig) error
	// fallbackOpMu is only used for test-injected ShardProcess values that
	// predate opMu. Real shards always get their own mutex from startShard.
	fallbackOpMu sync.Mutex
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
	// A restart deliberately inherits the ORIGINAL lastStart: refreshing it
	// would reopen a 30s grace on every attempt and swallow the exponential
	// backoff entirely (R-17).
	lastStart time.Time

	// initSQLDigest is the sha256 of init_sql_file's CONTENTS at start time.
	// Reload compares it so editing a policy file restarts the shard even
	// though its path (and therefore the config struct) is unchanged (R-07).
	initSQLDigest string

	// opMu serialises lifecycle operations on this one shard (stop / restart)
	// WITHOUT holding s.mu. It is a pointer so Status() can copy a
	// ShardProcess value without go vet's copylocks warning.
	opMu *sync.Mutex
	// restarting is guarded by s.mu and prevents two health ticks from
	// claiming the same shard concurrently.
	restarting bool
}

func New(cfg *config.Config, logger *slog.Logger) *Supervisor {
	return &Supervisor{
		cfg:          cfg,
		shards:       make(map[string]*ShardProcess),
		logger:       logger,
		startupGrace: defaultStartupGrace,
	}
}

// processSpec captures the global settings baked into a running shard's
// command line or boot SQL. Changing any of them requires EVERY shard to be
// restarted: the running DuckDB process cannot adopt them. Health-only
// settings (health_path / health_interval / health_expect) are deliberately
// excluded because checkAll reads them from s.cfg on every tick.
type processSpec struct {
	duckdbBin               string
	bindHost                string
	allowOtherHostname      bool
	allowUnsignedExtensions bool
}

func processSpecOf(cfg *config.Config) processSpec {
	if cfg == nil {
		return processSpec{}
	}
	return processSpec{
		duckdbBin:               cfg.Global.DuckDBBin,
		bindHost:                cfg.Listener.BindHost,
		allowOtherHostname:      cfg.Listener.AllowOtherHostname,
		allowUnsignedExtensions: cfg.Global.AllowUnsignedExtensions,
	}
}

// initSQLFileDigest hashes the CONTENTS of init_sql_file. A policy file can
// be edited without changing its path; without this, reload would silently
// keep serving the old policy (R-07). An unreadable file maps to a stable
// sentinel so a transient read error does not cause a restart storm; the
// actual startup path (buildBootSQL) still fails loudly on that file.
func initSQLFileDigest(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable:" + path
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// shardSpecChanged reports whether a kept shard must be restarted to adopt
// the new config. startShard resolves an omitted token into a random value
// and stores it in Config.Token, so a naive struct comparison would report a
// change on EVERY reload for shards using generated credentials. Normalising
// the old token back to "unset" when the new config also omits it avoids
// that; an explicit token must match the one the process is really using.
func shardSpecChanged(running *ShardProcess, next config.ShardConfig) bool {
	old := running.Config
	if next.Token == "" {
		old.Token = ""
	}
	return old != next || running.initSQLDigest != initSQLFileDigest(next.InitSQLFile)
}

// ApplyConfig hot-swaps in a reloaded configuration:
//   - stops removed shards, starts added shards;
//   - revives shards parked in "stopped" (R-02);
//   - restarts kept shards whose effective process spec changed (R-07);
//   - persists the new status.
//
// No potentially blocking work (process signalling, Wait, fork) runs while
// s.mu is held: phase 1 computes the diff under the lock; phase 2 performs
// it outside. See R-03.
func (s *Supervisor) ApplyConfig(ctx context.Context, cfg *config.Config) (added, removed, restarted []string) {
	s.mu.Lock()
	globalChanged := processSpecOf(s.cfg) != processSpecOf(cfg)
	s.cfg = cfg
	wanted := make(map[string]config.ShardConfig, len(cfg.Shards))
	for _, sh := range cfg.Shards {
		wanted[sh.Name] = sh
	}
	type stopJob struct {
		name string
		sp   *ShardProcess
	}
	type reloadJob struct {
		name string
		sp   *ShardProcess
		sh   config.ShardConfig
	}
	var toStop []stopJob
	for name, sp := range s.shards {
		if _, ok := wanted[name]; !ok {
			toStop = append(toStop, stopJob{name, sp})
			delete(s.shards, name)
		}
	}
	var toStart []config.ShardConfig
	var toRevive []string
	var toRestart []reloadJob
	for _, sh := range cfg.Shards {
		sp := s.shards[sh.Name]
		if sp == nil {
			toStart = append(toStart, sh)
			continue
		}
		// A shard parked in "stopped" has exhausted its retry budget.
		// Reload is the documented way to revive it (R-02); it starts with
		// the NEW config, so a field change is applied here as well.
		if sp.Status == "stopped" && !sp.restarting {
			sp.restarting = true
			toRevive = append(toRevive, sh.Name)
			continue
		}
		// A health tick may already own this shard's lifecycle; do not
		// launch a second concurrent stop/start for the same process (R-03).
		if sp.restarting {
			continue
		}
		if globalChanged || shardSpecChanged(sp, sh) {
			sp.restarting = true
			toRestart = append(toRestart, reloadJob{sh.Name, sp, sh})
		}
	}
	s.mu.Unlock()

	for _, job := range toStop {
		s.logger.Info("shard removed by reload, stopping", "name", job.name)
		s.stopShard(job.name, job.sp)
		removed = append(removed, job.name)
	}
	// Restarts run AFTER removals, so a freed port can be reused by a changed
	// or newly added shard within the same reload (R-07).
	for _, job := range toRestart {
		s.logger.Info("shard config changed, restarting", "name", job.name)
		s.stopShard(job.name, job.sp)
		if err := s.launch(ctx, job.sh); err != nil {
			s.logger.Error("reload: failed to restart shard with new config", "name", job.name, "error", err)
			s.mu.Lock()
			if sp := s.shards[job.name]; sp != nil {
				sp.restarting = false
			}
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		if live := s.shards[job.name]; live != nil {
			live.Status = "starting"
			live.Restarts = 0
			live.backoff = initialBackoff
			live.lastUnhealthy = time.Time{}
			live.lastStart = time.Now()
			live.restarting = false
		}
		s.mu.Unlock()
		restarted = append(restarted, job.name)
		s.logger.Info("shard restarted with new config", "name", job.name)
	}
	for _, name := range toRevive {
		sh := wanted[name]
		if err := s.launch(ctx, sh); err != nil {
			s.logger.Error("reload: failed to revive stopped shard", "name", name, "error", err)
			s.mu.Lock()
			if sp := s.shards[name]; sp != nil {
				sp.restarting = false
			}
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		if live := s.shards[name]; live != nil {
			live.Status = "starting"
			live.Restarts = 0
			live.backoff = initialBackoff
			live.lastUnhealthy = time.Time{}
			live.lastStart = time.Now()
			live.restarting = false
		}
		s.mu.Unlock()
		s.logger.Info("stopped shard revived by reload", "name", name)
	}
	for _, sh := range toStart {
		if err := s.launch(ctx, sh); err != nil {
			s.logger.Error("reload: failed to start shard", "name", sh.Name, "error", err)
			continue
		}
		s.logger.Info("shard added by reload, started", "name", sh.Name)
		added = append(added, sh.Name)
	}

	s.WriteStatusFile()
	return
}

// WriteStatusFile persists cluster state. It takes a short RLock to snapshot
// the state, then performs all file I/O WITHOUT the lock: a slow disk must
// never stall health checks, reload or shutdown (R-03 / R-05 / R-11).
func (s *Supervisor) WriteStatusFile() {
	s.mu.RLock()
	sf := s.buildStatusLocked()
	path := s.cfg.Global.StatusFile
	s.mu.RUnlock()
	writeStatusAtomically(path, sf, s.logger)
}

// buildStatusLocked renders the status snapshot; callers must hold s.mu.
func (s *Supervisor) buildStatusLocked() StatusFile {
	sf := StatusFile{Updated: time.Now().Format(time.RFC3339)}
	for _, sp := range s.shards {
		rec := StatusFileRecord{
			Name:      sp.Config.Name,
			Port:      sp.Config.Port,
			Status:    sp.Status,
			PID:       sp.PID,
			Restarts:  sp.Restarts,
			Database:  sp.Config.Database,
			StartTime: sp.StartTime.Format(time.RFC3339),
		}
		// PRD §4.2 / R-05: credentials are off-disk by default. Only an
		// explicit opt-in re-enables the legacy status.json token channel,
		// still protected by the 0600 atomic write below.
		if s.cfg.Global.PersistTokens {
			rec.Token = sp.Config.Token
		}
		if !sp.lastCheck.IsZero() {
			rec.LastCheck = sp.lastCheck.Format(time.RFC3339)
		}
		if sp.Status == "healthy" {
			rec.AttachSQL = fmt.Sprintf("ATTACH 'quack:%s:%d' AS %s;",
				connectHost(s.cfg.Listener.BindHost), sp.Config.Port, sp.Config.Name)
		}
		sf.Shards = append(sf.Shards, rec)
	}
	return sf
}

// writeStatusAtomically serialises and renames a private (0600) temp file
// into place. Readers never observe half-written JSON, and an existing
// world-readable file is replaced rather than silently kept.
func writeStatusAtomically(path string, sf StatusFile, logger *slog.Logger) {
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		logger.Error("marshal status file", "error", err)
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".quack-proxy-status-*.tmp")
	if err != nil {
		logger.Error("create status temp file", "error", err)
		return
	}
	tmp := f.Name()
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		logger.Error("write status temp file", "error", err)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		logger.Error("close status temp file", "error", err)
		return
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		os.Remove(tmp)
		logger.Error("chmod status temp file", "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		logger.Error("rename status file", "error", err)
	}
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

// probe runs the health check, falling back to the real probe when no seam
// is injected. launch does the same for starting a shard. Both keep zero-value
// Supervisors (used by older tests) working.
func (s *Supervisor) probe(host string, port int, path string, timeout time.Duration, expect string) bool {
	if s.checkFn != nil {
		return s.checkFn(host, port, path, timeout, expect)
	}
	return health.CheckWithExpect(host, port, path, timeout, expect)
}

// launch starts a shard and installs it into the map. It must be called
// WITHOUT s.mu held: forking and process-group setup may block. Injected
// startFn seams are responsible for installing their own state.
func (s *Supervisor) launch(ctx context.Context, sh config.ShardConfig) error {
	if s.startFn != nil {
		return s.startFn(ctx, sh)
	}
	sp, err := s.startShard(ctx, sh)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.shards[sh.Name] = sp
	s.mu.Unlock()
	return nil
}

// buildShardCommand assembles the shell pipeline that feeds boot SQL into a
// DuckDB child. -unsigned is opt-in: it allows arbitrary unsigned native
// extensions, so it is only enabled after the operator pre-installs and
// verifies them (R-14). Extracted so this is testable without forking.
func buildShardCommand(cfg *config.Config, sh config.ShardConfig, sqlFile string) string {
	unsignedFlag := ""
	if cfg.Global.AllowUnsignedExtensions {
		unsignedFlag = "-unsigned "
	}
	return fmt.Sprintf(
		`(cat '%s'; tail -f /dev/null) | '%s' %s'%s'`,
		sqlFile,
		cfg.Global.DuckDBBin,
		unsignedFlag,
		sh.Database,
	)
}

func (s *Supervisor) StartAll(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	shards := append([]config.ShardConfig(nil), s.cfg.Shards...)
	s.mu.Unlock()

	for _, sh := range shards {
		if err := s.launch(ctx, sh); err != nil {
			return fmt.Errorf("start %s: %w", sh.Name, err)
		}
	}
	s.logger.Info("all shards started", "count", len(shards))
	s.WriteStatusFile()
	return nil
}

// configSnapshot returns the live config under a read lock. Reload swaps the
// whole pointer instead of mutating fields, so the snapshot stays internally
// consistent for the duration of one fork.
func (s *Supervisor) configSnapshot() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// startShardLocked is kept for the unit tests that drive a single shard.
func (s *Supervisor) startShardLocked(ctx context.Context, sh config.ShardConfig) error {
	return s.launch(ctx, sh)
}

// portInUse reports whether something is already LISTENING on a shard's
// port. Binding (rather than dialling) also catches listeners that would
// ignore a health probe. Go sets SO_REUSEADDR, so a port merely in TIME_WAIT
// is still bindable and is not mistaken for an occupied port.
func portInUse(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

// startShard forks a child WITHOUT touching s.shards. All blocking work is
// lock-free; the caller installs the returned process into the map.
func (s *Supervisor) startShard(ctx context.Context, sh config.ShardConfig) (*ShardProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := s.configSnapshot()
	// Fail loud if the configured engine is missing or not executable:
	// otherwise the forked bash dies with "command not found" and the shard
	// looks "started" until the health loop notices.
	if _, ok := lookupBin(cfg.Global.DuckDBBin); !ok {
		return nil, fmt.Errorf("duckdb_bin %q not found on PATH or not executable", cfg.Global.DuckDBBin)
	}
	// R-22: after a daemon crash the old shard can survive and keep the port
	// bound. A naive restart then reports "healthy" because the health probe
	// is answered by that ORPHAN, not by the new process (proved by
	// scripts/daemon-crash-drill.sh). Refuse to start rather than manufacture
	// a false healthy status. Cleaning up orphans is an operational action;
	// automatic takeover would require a real HA control plane.
	if portInUse(connectHost(cfg.Listener.BindHost), sh.Port) {
		return nil, fmt.Errorf("shard %q port %d is already occupied before start: a leftover shard from a crashed daemon may still own it; stop that process group before restarting", sh.Name, sh.Port)
	}

	token := sh.Token
	if token == "" {
		token = randomToken(32)
	}
	sql, err := buildBootSQL(cfg.Listener.BindHost, sh, token, cfg.Listener.AllowOtherHostname)
	if err != nil {
		return nil, err
	}

	// Feed boot SQL via a FIFO-safe pipeline: write the SQL to a temp file,
	// then `(cat file; tail -f /dev/null) | duckdb ...`. File contents pass
	// through VERBATIM — no shell quoting, so multi-line init_sql survives.
	sqlFile := filepath.Join(os.TempDir(), fmt.Sprintf("quack-proxy-%s-%d.sql", sh.Name, sh.Port))
	if err := os.WriteFile(sqlFile, []byte(sql), 0600); err != nil {
		return nil, fmt.Errorf("write boot sql: %w", err)
	}
	shellCmd := buildShardCommand(cfg, sh, sqlFile)

	cmd := exec.Command("bash", "-c", shellCmd)
	// Bound how long Wait() may block on inherited pipes after the process
	// group is gone. Without this, stopShard can wedge the whole supervisor.
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.Remove(sqlFile)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.Remove(sqlFile)
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.SysProcAttr = setProcessGroup()
	cmd.Cancel = nil // we manage the process group ourselves; see stopShard

	if err := cmd.Start(); err != nil {
		os.Remove(sqlFile)
		return nil, fmt.Errorf("start duckdb: %w", err)
	}

	go drainLog("shard="+sh.Name, stdout)
	go drainLog("shard="+sh.Name, stderr)

	now := time.Now()
	sp := &ShardProcess{
		Config:        sh,
		cmd:           cmd,
		PID:           cmd.Process.Pid,
		Status:        "starting",
		StartTime:     now,
		lastStart:     now,
		backoff:       initialBackoff,
		lastUnhealthy: time.Time{},
		initSQLDigest: initSQLFileDigest(sh.InitSQLFile),
		opMu:          &sync.Mutex{},
	}
	sp.Config.Token = token
	return sp, nil
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

// StopAll stops every shard CONCURRENTLY. Serial 10s stops made a 4-shard
// daemon exceed systemd's TimeoutStopSec=30 and get SIGKILLed mid-cleanup
// (R-12). Each shard has its own opMu, so parallel stops are safe.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	jobs := make(map[string]*ShardProcess, len(s.shards))
	for name, sp := range s.shards {
		jobs[name] = sp
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for name, sp := range jobs {
		wg.Add(1)
		go func(n string, p *ShardProcess) {
			defer wg.Done()
			s.stopShard(n, p)
		}(name, sp)
	}
	wg.Wait()
}

// stopShard stops one shard's whole process group. It never holds s.mu while
// waiting: a stuck Wait must not wedge status, reload or shutdown (R-03).
func (s *Supervisor) stopShard(name string, sp *ShardProcess) {
	mu := sp.opMu
	if mu == nil {
		mu = &s.fallbackOpMu // test-injected shard without an opMu
	}
	mu.Lock()
	defer mu.Unlock()

	if sp.cmd != nil && sp.cmd.Process != nil {
		pid := sp.cmd.Process.Pid
		signalProcessGroup(pid, syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- sp.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
			signalProcessGroup(pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				s.logger.Warn("shard Wait did not return after SIGKILL", "name", name)
			}
		}
		// cmd.Wait only reaps the group LEADER (the bash wrapper). DuckDB and
		// the `tail -f /dev/null` keep-alive can still be shutting down — or
		// ignoring SIGTERM — which leaves the Quack port bound even though the
		// daemon has already logged "shard stopped". Confirm the whole group
		// is gone, and hard-kill any survivors (R-21, found by the real
		// DuckDB E2E: a stop reported success while the port stayed open).
		if !waitGroupGone(pid, 2*time.Second) {
			s.logger.Warn("process group survived Wait, sending SIGKILL", "name", name)
			signalProcessGroup(pid, syscall.SIGKILL)
			if !waitGroupGone(pid, 2*time.Second) {
				s.logger.Error("process group could not be reaped", "name", name)
			}
		}
	}
	sqlFile := filepath.Join(os.TempDir(), fmt.Sprintf("quack-proxy-%s-%d.sql", sp.Config.Name, sp.Config.Port))
	os.Remove(sqlFile)
	sp.cmd = nil
	s.mu.Lock()
	sp.PID = 0
	sp.Status = "stopped"
	s.mu.Unlock()
	s.logger.Info("shard stopped", "name", name)
}

// waitGroupGone polls until the whole process group has exited. cmd.Wait()
// alone is not sufficient: it only waits for the direct child.
func waitGroupGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !groupAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// stopShardLocked is retained as a test-facing alias; it does not require or
// take s.mu (the old contract was that callers held it).
func (s *Supervisor) stopShardLocked(name string, sp *ShardProcess) {
	s.stopShard(name, sp)
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

type probeTarget struct {
	name   string
	host   string
	port   int
	path   string
	expect string
}

type restartJob struct {
	name     string
	sp       *ShardProcess
	attempts int
	backoff  time.Duration
}

// checkAll runs in four phases so no blocking work happens under s.mu:
//  1. snapshot probe targets (RLock, microseconds)
//  2. probe all shards concurrently, lock-free
//  3. apply results and claim failing shards (Lock, pure memory)
//  4. stop/restart claimed shards, lock-free, then persist status
//
// The old implementation held s.mu across 2s*N HTTP calls and a 10s process
// wait per shard, which could wedge status, reload and shutdown (R-03).
func (s *Supervisor) checkAll(ctx context.Context) {
	s.mu.RLock()
	cfg := s.cfg
	targets := make([]probeTarget, 0, len(s.shards))
	for name, sp := range s.shards {
		if sp.Status == "stopped" || sp.restarting {
			continue
		}
		targets = append(targets, probeTarget{
			name:   name,
			host:   cfg.Listener.BindHost,
			port:   sp.Config.Port,
			path:   cfg.Listener.HealthPath,
			expect: cfg.Listener.HealthExpect,
		})
	}
	s.mu.RUnlock()

	results := make([]bool, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeConcurrency)
	for i := range targets {
		wg.Add(1)
		go func(i int, t probeTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.probe(t.host, t.port, t.path, probeTimeout, t.expect)
		}(i, targets[i])
	}
	wg.Wait()

	s.mu.Lock()
	now := time.Now()
	var jobs []restartJob
	for i, t := range targets {
		sp := s.shards[t.name]
		if sp == nil || sp.Status == "stopped" || sp.restarting {
			continue
		}
		sp.lastCheck = now
		if results[i] {
			sp.Status = "healthy"
			// A healthy response clears the whole failure streak, not just
			// the backoff (R-02).
			sp.Restarts = 0
			sp.backoff = initialBackoff
			sp.lastUnhealthy = time.Time{}
			continue
		}

		// A fresh cold start is inside its startup grace: the engine may
		// still be installing/loading before quack_serve binds.
		if now.Sub(sp.lastStart) < s.startupGrace {
			sp.Status = "starting"
			continue
		}
		// Still inside the current backoff window.
		if !sp.lastUnhealthy.IsZero() && now.Sub(sp.lastUnhealthy) < sp.backoff {
			sp.Status = "unhealthy"
			continue
		}
		sp.lastUnhealthy = now
		attempts := sp.Restarts + 1
		s.logger.Warn("shard unhealthy, restarting", "name", t.name, "attempt", attempts)
		sp.restarting = true
		jobs = append(jobs, restartJob{
			name:     t.name,
			sp:       sp,
			attempts: attempts,
			backoff:  min(sp.backoff*2, maxBackoff),
		})
	}
	s.mu.Unlock()

	for _, job := range jobs {
		s.performRestart(ctx, job)
	}
	s.WriteStatusFile()
}

// performRestart stops and relaunches one shard WITHOUT s.mu held. It keeps
// the original lastStart so a fresh 30s grace does not swallow the backoff
// on every attempt (R-17), and it clears the claim when done.
func (s *Supervisor) performRestart(ctx context.Context, job restartJob) {
	old := job.sp
	originalStart := old.lastStart
	shardCfg := old.Config

	s.stopShard(job.name, old)
	if err := s.launch(ctx, shardCfg); err != nil {
		s.logger.Error("failed to restart shard", "name", job.name, "error", err)
		s.mu.Lock()
		if live := s.shards[job.name]; live != nil {
			live.Restarts = job.attempts
			live.backoff = job.backoff
			live.lastUnhealthy = time.Now()
			live.Status = "unhealthy"
			live.restarting = false
		}
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	live := s.shards[job.name]
	if live != nil {
		live.Status = "starting"
		live.Restarts = job.attempts
		live.backoff = job.backoff
		live.lastUnhealthy = time.Now()
		// Deliberately NOT refreshed: see R-17 above.
		live.lastStart = originalStart
		live.restarting = false
	}
	s.mu.Unlock()

	if job.attempts >= maxRestarts {
		s.logger.Error("shard failed to stabilize, stopping retries", "name", job.name, "attempts", job.attempts)
		if live == nil {
			s.mu.RLock()
			live = s.shards[job.name]
			s.mu.RUnlock()
		}
		if live != nil {
			s.stopShard(job.name, live)
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
	if spCopy.opMu == nil {
		spCopy.opMu = &sync.Mutex{}
	}
	s.shards[name] = &spCopy
}

func (s *Supervisor) AttachSQL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sql string
	for _, sp := range s.shards {
		if sp.Status == "healthy" {
			sql += fmt.Sprintf("ATTACH 'quack:%s:%d' AS %s;\n",
				connectHost(s.cfg.Listener.BindHost), sp.Config.Port, sp.Config.Name)
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
