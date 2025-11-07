// Package process implements a Runner interface for local processes
package process

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/process/security"
	"github.com/runestack/rune/pkg/types"
)

// Validate that ProcessRunner implements the runner.Runner interface
var _ runner.Runner = &ProcessRunner{}

// ProcessRunner implements the runner.Runner interface for local processes
type ProcessRunner struct {
	baseDir   string
	logger    log.Logger
	processes map[string]*managedProcess
	mu        sync.RWMutex
}

// managedProcess holds internal state for a process managed by ProcessRunner
type managedProcess struct {
	instance     *types.Instance
	cmd          *exec.Cmd
	status       runner.InstanceStatus
	logFile      *os.File
	workDir      string
	mu           sync.RWMutex
	stopCh       chan struct{}
	cleanupFiles []string
	// resource cgroup controller (Linux-only)
	rc *resourceController
}

// ProcessOption is a function that configures a ProcessRunner
type ProcessOption func(*ProcessRunner)

// WithBaseDir sets the base directory for process workspaces and logs
func WithBaseDir(dir string) ProcessOption {
	return func(r *ProcessRunner) {
		r.baseDir = dir
	}
}

// WithLogger sets the logger for the runner
func WithLogger(logger log.Logger) ProcessOption {
	return func(r *ProcessRunner) {
		r.logger = logger
	}
}

// NewProcessRunner creates a new ProcessRunner with the given options
func NewProcessRunner(options ...ProcessOption) (*ProcessRunner, error) {
	// Create a default logger
	defaultLogger := log.NewLogger()

	runner := &ProcessRunner{
		baseDir:   os.TempDir(),
		logger:    defaultLogger,
		processes: make(map[string]*managedProcess),
	}

	for _, option := range options {
		option(runner)
	}

	// Ensure base directory exists
	if err := os.MkdirAll(runner.baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return runner, nil
}

func (r *ProcessRunner) Type() types.RunnerType {
	return types.RunnerTypeProcess
}

// Create creates a new process but does not start it
func (r *ProcessRunner) Create(ctx context.Context, instance *types.Instance) error {
	if instance == nil {
		return fmt.Errorf("instance cannot be nil")
	}

	if instance.ID == "" {
		instance.ID = uuid.New().String()
	}

	if instance.Namespace == "" {
		instance.Namespace = instance.ServiceID
	}

	if err := instance.Validate(); err != nil {
		return fmt.Errorf("invalid instance: %w", err)
	}

	// Ensure we have process spec
	if instance.Process == nil {
		return fmt.Errorf("process spec is required")
	}

	// Use our enhanced validation that also checks executable path
	if err := ValidateProcessSpec(instance.Process); err != nil {
		return fmt.Errorf("invalid process spec: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if instance already exists
	if _, exists := r.processes[instance.ID]; exists {
		return fmt.Errorf("instance with ID %s already exists", instance.ID)
	}

	// Create workspace directory
	workDir := filepath.Join(r.baseDir, "rune", instance.Namespace, instance.ID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("failed to create workspace directory: %w", err)
	}

	// Create log file
	logPath := filepath.Join(workDir, "process.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		// Clean up the workspace if we fail
		_ = os.RemoveAll(workDir)
		return fmt.Errorf("failed to create log file: %w", err)
	}

	// Materialize mounts (secrets/configs) if provided
	var createdFiles []string
	if instance.Metadata != nil {
		// Secrets
		if len(instance.Metadata.SecretMounts) > 0 {
			files, mErr := r.materializeSecretMounts(instance.Metadata.SecretMounts)
			if mErr != nil {
				_ = logFile.Close()
				_ = os.RemoveAll(workDir)
				return fmt.Errorf("failed to materialize secret mounts: %w", mErr)
			}
			createdFiles = append(createdFiles, files...)
		}
		// Configs
		if len(instance.Metadata.ConfigmapMounts) > 0 {
			files, mErr := r.materializeConfigMounts(instance.Metadata.ConfigmapMounts)
			if mErr != nil {
				_ = logFile.Close()
				_ = os.RemoveAll(workDir)
				return fmt.Errorf("failed to materialize config mounts: %w", mErr)
			}
			createdFiles = append(createdFiles, files...)
		}
	}

	// Prepare command (but don't start it)
	cmd := exec.CommandContext(ctx, instance.Process.Command, instance.Process.Args...)

	// Set environment variables
	if len(instance.Environment) > 0 {
		// Convert map to slice of KEY=VALUE strings
		env := os.Environ() // Include parent environment
		for key, value := range instance.Environment {
			env = append(env, fmt.Sprintf("%s=%s", key, value))
		}
		cmd.Env = env
	}

	// Set working directory if specified
	if instance.Process.WorkingDir != "" {
		cmd.Dir = instance.Process.WorkingDir
	} else {
		cmd.Dir = workDir
	}

	// Apply security context if specified
	if instance.Process.SecurityContext != nil {
		if err := security.ApplySecurityContext(cmd, instance.Process.SecurityContext, r.logger); err != nil {
			// Clean up resources if security context fails
			_ = logFile.Close()
			_ = os.RemoveAll(workDir)
			return fmt.Errorf("failed to apply security context: %w", err)
		}
	}

	// Record creation time
	now := time.Now()

	// Create managed process entry
	proc := &managedProcess{
		instance:     instance,
		cmd:          cmd,
		workDir:      workDir,
		logFile:      logFile,
		cleanupFiles: append([]string{logPath}, filterFilesUnder(workDir, createdFiles)...),
		stopCh:       make(chan struct{}),
		status: runner.InstanceStatus{
			State:      types.InstanceStatusCreated,
			InstanceID: instance.ID,
			CreatedAt:  now,
		},
	}

	// Store the process
	r.processes[instance.ID] = proc

	// Update instance status
	instance.Status = types.InstanceStatusCreated
	instance.UpdatedAt = now
	instance.CreatedAt = now

	r.logger.Info("Created process instance",
		log.Str("instance_id", instance.ID),
		log.Str("name", instance.Name),
		log.Str("command", instance.Process.Command))

	return nil
}

// Start starts an existing process instance
func (r *ProcessRunner) Start(ctx context.Context, instance *types.Instance) error {
	r.mu.RLock()
	proc, exists := r.processes[instance.ID]
	r.mu.RUnlock()

	if !exists {
		return fmt.Errorf("instance with ID %s not found", instance.ID)
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()

	// Ensure instance is in a valid state to start
	if proc.status.State != types.InstanceStatusCreated &&
		proc.status.State != types.InstanceStatusStopped {
		return fmt.Errorf("instance in state %s cannot be started", proc.status.State)
	}

	// Set up command stdout/stderr redirection to log file
	proc.cmd.Stdout = proc.logFile
	proc.cmd.Stderr = proc.logFile

	// Start the command
	if err := proc.cmd.Start(); err != nil {
		proc.status.State = types.InstanceStatusFailed
		proc.status.ErrorMessage = err.Error()
		return fmt.Errorf("failed to start process: %w", err)
	}

	// Apply resource limits (Linux cgroups v2) if specified
	if instance.Resources != nil {
		if rc, err := newResourceController(proc.cmd.Process.Pid, instance.Resources); err != nil {
			r.logger.Warn("Resource limits not applied", log.Err(err))
		} else {
			proc.rc = rc
		}
	}

	// Update status
	now := time.Now()
	proc.status.State = types.InstanceStatusRunning
	proc.status.StartedAt = now

	// Update instance status
	proc.instance.Status = types.InstanceStatusRunning
	proc.instance.UpdatedAt = now

	// Monitor the process completion to update status
	go func() {
		// Wait for process to complete
		err := proc.cmd.Wait()

		proc.mu.Lock()
		defer proc.mu.Unlock()

		// Get exit code and update status
		exitCode := 0
		if err != nil {
			// Check if it's an exit error
			if exitErr, ok := err.(*exec.ExitError); ok {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					exitCode = status.ExitStatus()
				}
			}
			proc.status.ErrorMessage = err.Error()
		}

		now := time.Now()
		proc.status.State = types.InstanceStatusStopped
		proc.status.ExitCode = exitCode
		proc.status.FinishedAt = now

		// Update instance
		proc.instance.Status = types.InstanceStatusStopped
		proc.instance.UpdatedAt = now

		r.logger.Info("Process completed",
			log.Str("instance_id", proc.instance.ID),
			log.Str("name", proc.instance.Name),
			log.Int("exit_code", exitCode))
	}()

	r.logger.Info("Started process instance",
		log.Str("instance_id", proc.instance.ID),
		log.Str("name", proc.instance.Name),
		log.Int("pid", proc.cmd.Process.Pid))

	return nil
}

// Stop stops a running process instance
func (r *ProcessRunner) Stop(ctx context.Context, instance *types.Instance, timeout time.Duration) error {
	r.mu.RLock()
	proc, exists := r.processes[instance.ID]
	r.mu.RUnlock()

	if !exists {
		return fmt.Errorf("instance with ID %s not found", instance.ID)
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()

	// Check if the process is running
	if proc.status.State != types.InstanceStatusRunning {
		return nil // Already stopped
	}

	if proc.cmd.Process == nil {
		return fmt.Errorf("process is not running")
	}

	// Try graceful shutdown first (SIGTERM)
	if err := proc.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		r.logger.Warn("Failed to send SIGTERM to process",
			log.Str("instance_id", proc.instance.ID),
			log.Err(err))
	}

	// Set up timer for timeout
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Channel to indicate process has exited
	done := make(chan error, 1)
	go func() {
		// Wait for the process to exit
		_, err := proc.cmd.Process.Wait()
		done <- err
	}()

	// Wait for process to exit or timeout
	select {
	case <-timer.C:
		// Timeout occurred, forcefully kill the process
		if err := proc.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process after timeout: %w", err)
		}

		r.logger.Warn("Process killed after timeout",
			log.Str("instance_id", proc.instance.ID),
			log.Str("name", proc.instance.Name))

	case err := <-done:
		if err != nil {
			r.logger.Warn("Process exited with error",
				log.Str("instance_id", proc.instance.ID),
				log.Err(err))
		} else {
			r.logger.Info("Process stopped gracefully",
				log.Str("instance_id", proc.instance.ID),
				log.Str("name", proc.instance.Name))
		}
	}

	// Update status
	now := time.Now()
	proc.status.State = types.InstanceStatusStopped
	proc.status.FinishedAt = now

	// Update instance
	proc.instance.Status = types.InstanceStatusStopped
	proc.instance.UpdatedAt = now

	return nil
}

// Remove removes a process instance
func (r *ProcessRunner) Remove(ctx context.Context, instance *types.Instance, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	proc, exists := r.processes[instance.ID]
	if !exists {
		return fmt.Errorf("instance with ID %s not found", instance.ID)
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()

	// If the process is still running and force is not set, return error
	if proc.status.State == types.InstanceStatusRunning && !force {
		return fmt.Errorf("cannot remove running instance, use force to override")
	}

	// If running and force is set, try to kill it
	if proc.status.State == types.InstanceStatusRunning && force {
		if proc.cmd.Process != nil {
			if err := proc.cmd.Process.Kill(); err != nil {
				r.logger.Warn("Failed to kill process during forced removal",
					log.Str("instance_id", proc.instance.ID),
					log.Err(err))
			}
		}
	}

	// Close log file
	if proc.logFile != nil {
		_ = proc.logFile.Close()
	}

	// Cleanup resource controller cgroup
	if proc.rc != nil {
		_ = proc.rc.cleanup()
		proc.rc = nil
	}

	// Remove files created under workDir
	for _, fp := range proc.cleanupFiles {
		// Best-effort remove only files inside workDir
		if strings.HasPrefix(fp, proc.workDir+string(os.PathSeparator)) || fp == proc.workDir {
			_ = os.Remove(fp)
		}
	}

	// Remove workspace directory
	if err := os.RemoveAll(proc.workDir); err != nil {
		r.logger.Warn("Failed to remove workspace directory",
			log.Str("instance_id", proc.instance.ID),
			log.Err(err))
	}

	// Remove from process map
	delete(r.processes, instance.ID)

	r.logger.Info("Removed process instance",
		log.Str("instance_id", proc.instance.ID),
		log.Str("name", proc.instance.Name))

	return nil
}

// GetLogs retrieves logs from a process instance
func (r *ProcessRunner) GetLogs(ctx context.Context, instance *types.Instance, options runner.LogOptions) (io.ReadCloser, error) {
	r.mu.RLock()
	proc, exists := r.processes[instance.ID]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("instance with ID %s not found", instance.ID)
	}

	proc.mu.RLock()
	defer proc.mu.RUnlock()

	// Ensure log file exists
	if proc.logFile == nil {
		return nil, fmt.Errorf("log file not found for instance %s", instance.ID)
	}

	// Get log file path
	logPath := proc.logFile.Name()

	// Open log file for reading
	file, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	// If no filtering options are set, just return the file
	if !options.Follow && options.Tail <= 0 && options.Since.IsZero() && options.Until.IsZero() {
		return file, nil
	}

	// Create a filtered reader
	pipeReader, pipeWriter := io.Pipe()

	go func() {
		defer pipeWriter.Close()
		defer file.Close()

		// Start at the beginning by default
		var startPos int64 = 0

		// Handle tail option
		if options.Tail > 0 {
			// Get file size
			info, err := file.Stat()
			if err != nil {
				r.logger.Error("Failed to get log file info", log.Err(err))
				return
			}

			// Find start position by counting lines from the end
			startPos = findStartPosition(file, info.Size(), options.Tail)
		}

		// Seek to the start position
		_, err = file.Seek(startPos, 0)
		if err != nil {
			r.logger.Error("Failed to seek log file", log.Err(err))
			return
		}

		// Use a scanner to read the file line by line
		scanner := bufio.NewScanner(file)
		buffer := make([]byte, 4096)
		scanner.Buffer(buffer, 1024*1024) // Increase buffer size for longer lines

		// Channel to signal stop
		stopCh := make(chan struct{})

		// Setup context cancellation
		go func() {
			<-ctx.Done()
			close(stopCh)
		}()

		// Continuously read and filter lines
		for scanner.Scan() {
			select {
			case <-stopCh:
				return
			default:
				line := scanner.Bytes()

				// Check if we should filter this line based on timestamps
				if shouldIncludeLine(line, options) {
					// Include this line
					_, writeErr := pipeWriter.Write(append(line, '\n'))
					if writeErr != nil {
						r.logger.Error("Failed to write to pipe", log.Err(writeErr))
						return
					}
				}
			}
		}

		// Check for scanner errors
		if err := scanner.Err(); err != nil {
			r.logger.Error("Error scanning log file", log.Err(err))
			return
		}

		// If follow mode is enabled, continue watching the file
		if options.Follow && proc.status.State == types.InstanceStatusRunning {
			// Create a file watcher to detect changes
			watcher := make(chan struct{})

			// Start a goroutine to poll the file for changes
			go func() {
				defer close(watcher)

				lastSize, err := file.Seek(0, io.SeekCurrent)
				if err != nil {
					r.logger.Error("Failed to get file position", log.Err(err))
					return
				}

				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()

				for {
					select {
					case <-stopCh:
						return
					case <-ticker.C:
						// Get current file size
						info, err := file.Stat()
						if err != nil {
							r.logger.Error("Failed to get file stats", log.Err(err))
							return
						}

						// If file has grown, signal the watcher
						if info.Size() > lastSize {
							lastSize = info.Size()
							watcher <- struct{}{}
						}

						// If process is no longer running, stop watching
						if proc.status.State != types.InstanceStatusRunning {
							return
						}
					}
				}
			}()

			// Continue reading when changes are detected
			for {
				select {
				case <-stopCh:
					return
				case _, ok := <-watcher:
					if !ok {
						return // Watcher closed
					}

					// Read new content
					scanner := bufio.NewScanner(file)
					for scanner.Scan() {
						line := scanner.Bytes()

						// Check if we should filter this line based on timestamps
						if shouldIncludeLine(line, options) {
							// Include this line
							_, writeErr := pipeWriter.Write(append(line, '\n'))
							if writeErr != nil {
								r.logger.Error("Failed to write to pipe", log.Err(writeErr))
								return
							}
						}
					}

					if err := scanner.Err(); err != nil {
						r.logger.Error("Error scanning log file", log.Err(err))
						return
					}
				}
			}
		}
	}()

	return pipeReader, nil
}

// shouldIncludeLine determines whether a log line should be included based on timestamp filters
func shouldIncludeLine(line []byte, options runner.LogOptions) bool {
	// If no timestamp filters are set, include the line
	if options.Since.IsZero() && options.Until.IsZero() {
		return true
	}

	// Try to extract timestamp from the line
	timestamp, ok := extractTimestamp(line)
	if !ok {
		// If we can't extract a timestamp, include the line by default
		return true
	}

	// For partial timestamps (without year or with zero year), use the current time's year
	// to avoid filtering based on incorrect dates
	if timestamp.Year() < 1000 {
		currentYear := time.Now().Year()
		// Create a new timestamp with the current year but preserve month, day, hour, etc.
		timestamp = time.Date(
			currentYear,
			timestamp.Month(),
			timestamp.Day(),
			timestamp.Hour(),
			timestamp.Minute(),
			timestamp.Second(),
			timestamp.Nanosecond(),
			timestamp.Location(),
		)
	}

	// Apply Since filter
	if !options.Since.IsZero() && timestamp.Before(options.Since) {
		return false
	}

	// Apply Until filter
	if !options.Until.IsZero() && timestamp.After(options.Until) {
		return false
	}

	return true
}

// extractTimestamp attempts to extract a timestamp from a log line
// It looks for common timestamp formats at the beginning of the line
func extractTimestamp(line []byte) (time.Time, bool) {
	// Common timestamp formats to try
	formats := []string{
		"2006-01-02T15:04:05.999999999Z07:00", // RFC3339Nano
		"2006-01-02T15:04:05Z07:00",           // RFC3339
		"2006-01-02 15:04:05",                 // Common format
		"2006/01/02 15:04:05",                 // Common format
		"Jan 2 15:04:05",                      // Common syslog format
		"Jan 02 15:04:05",                     // Common syslog format
		"15:04:05",                            // Time only
	}

	// Convert to string for parsing
	lineStr := string(line)

	// Try to find a timestamp at the beginning of the line
	for _, format := range formats {
		// Look for timestamps with brackets
		if len(lineStr) > 2 && lineStr[0] == '[' {
			closeBracket := strings.IndexByte(lineStr, ']')
			if closeBracket > 0 {
				potentialTimestamp := lineStr[1:closeBracket]
				timestamp, err := time.Parse(format, potentialTimestamp)
				if err == nil {
					return timestamp, true
				}
			}
		}

		// Try without brackets - directly at the beginning of the line
		if len(lineStr) >= len(format) {
			potentialTimestamp := lineStr
			if len(potentialTimestamp) > len(format) {
				potentialTimestamp = potentialTimestamp[:len(format)]
			}
			timestamp, err := time.Parse(format, potentialTimestamp)
			if err == nil {
				return timestamp, true
			}
		}
	}

	// Try to match timestamps anywhere in the string with common patterns
	timePatterns := []string{
		`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})`, // RFC3339/ISO8601
		`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`,                             // Common format
		`\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`,                             // Common format
		`[A-Z][a-z]{2} \d{1,2} \d{2}:\d{2}:\d{2}`,                         // Syslog format
		`\d{2}:\d{2}:\d{2}`,                                               // Time only
	}

	for _, pattern := range timePatterns {
		matches := regexp.MustCompile(pattern).FindAllStringSubmatch(lineStr, 1)
		if len(matches) > 0 && len(matches[0]) > 0 {
			match := matches[0][0]
			// Try to parse the matched time string with all formats
			for _, format := range formats {
				timestamp, err := time.Parse(format, match)
				if err == nil {
					return timestamp, true
				}
			}
		}
	}

	// Couldn't extract a timestamp
	return time.Time{}, false
}

// materializeSecretMounts writes secret files to their target paths.
// Returns a list of file paths created (best-effort; only files under the
// runner workDir are later cleaned automatically).
func (r *ProcessRunner) materializeSecretMounts(secretMounts []types.ResolvedSecretMount) ([]string, error) {
	var created []string
	for _, m := range secretMounts {
		files, err := r.writeSecretFiles(m)
		if err != nil {
			return created, err
		}
		created = append(created, files...)
	}
	return created, nil
}

// writeSecretFiles writes individual secret files for a single mount.
func (r *ProcessRunner) writeSecretFiles(mount types.ResolvedSecretMount) ([]string, error) {
	// Ensure target directory exists
	if err := os.MkdirAll(mount.MountPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", mount.MountPath, err)
	}

	var created []string
	// Helper to write a single key->path
	write := func(key, relPath, value string) error {
		dest := filepath.Join(mount.MountPath, relPath)
		// Ensure parent dirs
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return fmt.Errorf("failed to create parent dirs for %s: %w", dest, err)
		}
		// Decode base64 if value appears to be base64-encoded (e.g., PEMs)
		data := []byte(value)
		if decoded, ok := decodeIfBase64(value); ok {
			data = decoded
		}
		// Write with 0400
		if err := os.WriteFile(dest, data, 0400); err != nil {
			return fmt.Errorf("failed to write secret file %s: %w", dest, err)
		}
		created = append(created, dest)
		return nil
	}

	if len(mount.Items) > 0 {
		mapped := make(map[string]struct{}, len(mount.Items))
		for _, item := range mount.Items {
			val, ok := mount.Data[item.Key]
			if !ok {
				return created, fmt.Errorf("secret key %q not found for mount %q", item.Key, mount.Name)
			}
			if err := write(item.Key, item.Path, val); err != nil {
				return created, err
			}
			mapped[item.Key] = struct{}{}
		}
		// Default-map remaining keys not explicitly mapped
		for k, v := range mount.Data {
			if _, ok := mapped[k]; ok {
				continue
			}
			if err := write(k, k, v); err != nil {
				return created, err
			}
		}
		return created, nil
	}

	// Default mapping: each key -> file with same name
	for k, v := range mount.Data {
		if err := write(k, k, v); err != nil {
			return created, err
		}
	}
	return created, nil
}

// decodeIfBase64 attempts to decode s as standard base64 if it looks like base64 content.
// Returns (decoded, true) when decoding is performed, otherwise (nil, false).
func decodeIfBase64(s string) ([]byte, bool) {
	// Fast reject on length
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 || len(trimmed)%4 != 0 {
		return nil, false
	}
	// Check allowed characters
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			continue
		}
		return nil, false
	}
	// Try decode
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, false
	}
	// Accept all successful decodes to support binary secrets
	return decoded, true
}

// materializeConfigMounts writes config files to their target paths with 0644 perms.
func (r *ProcessRunner) materializeConfigMounts(configMounts []types.ResolvedConfigmapMount) ([]string, error) {
	var created []string
	for _, m := range configMounts {
		files, err := r.writeConfigFiles(m)
		if err != nil {
			return created, err
		}
		created = append(created, files...)
	}
	return created, nil
}

// writeConfigFiles writes individual config files for a single mount.
func (r *ProcessRunner) writeConfigFiles(mount types.ResolvedConfigmapMount) ([]string, error) {
	if err := os.MkdirAll(mount.MountPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", mount.MountPath, err)
	}

	var created []string
	write := func(key, relPath, value string) error {
		dest := filepath.Join(mount.MountPath, relPath)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return fmt.Errorf("failed to create parent dirs for %s: %w", dest, err)
		}
		if err := os.WriteFile(dest, []byte(value), 0o644); err != nil {
			return fmt.Errorf("failed to write config file %s: %w", dest, err)
		}
		created = append(created, dest)
		return nil
	}

	if len(mount.Items) > 0 {
		mapped := make(map[string]struct{}, len(mount.Items))
		for _, item := range mount.Items {
			val, ok := mount.Data[item.Key]
			if !ok {
				return created, fmt.Errorf("config key %q not found for mount %q", item.Key, mount.Name)
			}
			if err := write(item.Key, item.Path, val); err != nil {
				return created, err
			}
			mapped[item.Key] = struct{}{}
		}
		for k, v := range mount.Data {
			if _, ok := mapped[k]; ok {
				continue
			}
			if err := write(k, k, v); err != nil {
				return created, err
			}
		}
		return created, nil
	}

	for k, v := range mount.Data {
		if err := write(k, k, v); err != nil {
			return created, err
		}
	}
	return created, nil
}

// filterFilesUnder returns only paths under the given base directory.
func filterFilesUnder(base string, paths []string) []string {
	var out []string
	base = filepath.Clean(base)
	prefix := base + string(os.PathSeparator)
	for _, p := range paths {
		cp := filepath.Clean(p)
		if cp == base || strings.HasPrefix(cp, prefix) {
			out = append(out, cp)
		}
	}
	return out
}

// Status retrieves the current status of a process instance
func (r *ProcessRunner) Status(ctx context.Context, instance *types.Instance) (types.InstanceStatus, error) {
	r.mu.RLock()
	proc, exists := r.processes[instance.ID]
	r.mu.RUnlock()

	if !exists {
		return types.InstanceStatusFailed, fmt.Errorf("instance with ID %s not found", instance.ID)
	}

	proc.mu.RLock()
	defer proc.mu.RUnlock()

	return proc.status.State, nil
}

// List lists all process instances
func (r *ProcessRunner) List(ctx context.Context, namespace string) ([]*types.Instance, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	instances := make([]*types.Instance, 0, len(r.processes))
	for _, proc := range r.processes {
		proc.mu.RLock()
		// Make a copy of the instance to avoid concurrent access issues
		instance := *proc.instance
		proc.mu.RUnlock()
		instances = append(instances, &instance)
	}

	return instances, nil
}

// findStartPosition finds the position to start reading to get the last n lines
func findStartPosition(file *os.File, fileSize int64, numLines int) int64 {
	// Return to start after function
	defer file.Seek(0, 0)

	// If numLines is 0 or negative, return the start of the file
	if numLines <= 0 {
		return 0
	}

	// If the file is empty, return 0
	if fileSize == 0 {
		return 0
	}

	// Buffer for reading the file backwards
	buf := make([]byte, 1)
	lineCount := 0
	var pos int64

	// Start from the end of the file
	for pos = fileSize - 1; pos >= 0; pos-- {
		// Seek to the current position
		_, err := file.Seek(pos, 0)
		if err != nil {
			return 0
		}

		// Read one byte
		_, err = file.Read(buf)
		if err != nil {
			return 0
		}

		// Check if we found a newline
		if buf[0] == '\n' {
			lineCount++
			if lineCount > numLines {
				// We found the position, return the next byte
				return pos + 1
			}
		}
	}

	// If we reached here, we didn't find enough lines, return the start of the file
	return 0
}

// Exec creates an interactive exec session within a running process.
func (r *ProcessRunner) Exec(ctx context.Context, instance *types.Instance, options runner.ExecOptions) (runner.ExecStream, error) {
	r.mu.RLock()
	_, exists := r.processes[instance.ID]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("instance not found: %s", instance.ID)
	}

	// Verify instance is running
	status, err := r.Status(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance status: %w", err)
	}

	if status != types.InstanceStatusRunning {
		return nil, fmt.Errorf("instance is not running, status: %s", status)
	}

	// Create the exec stream
	// For a process, exec means starting a new process (we can't exec inside an existing process)
	// We could potentially use different methods like ptrace for more advanced use cases
	execStream, err := NewProcessExecStream(ctx, instance.ID, options, r.logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create exec stream: %w", err)
	}

	return execStream, nil
}
