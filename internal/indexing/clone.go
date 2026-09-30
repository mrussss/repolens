package indexing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"repolens/internal/platform/logger"
)

var (
	privateIPBlocks []*net.IPNet
)

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // Loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"169.254.0.0/16", // Link-local / Cloud metadata
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA
		"fe80::/10",      // IPv6 link-local
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPBlocks = append(privateIPBlocks, block)
		}
	}
}

func isPrivateOrLocalIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate()
}

type SafeGitCloner struct {
	allowHosts   []string
	maxDiskBytes int64
	cloneTimeout time.Duration
}

func NewSafeGitCloner(allowHosts []string, maxSizeMB int64, timeout time.Duration) *SafeGitCloner {
	if maxSizeMB <= 0 {
		maxSizeMB = 50
	}
	return NewSafeGitClonerWithDiskLimit(allowHosts, maxSizeMB*1024*1024, timeout)
}

func NewSafeGitClonerWithDiskLimit(allowHosts []string, maxDiskBytes int64, timeout time.Duration) *SafeGitCloner {
	if len(allowHosts) == 0 {
		allowHosts = []string{"github.com"}
	}
	if maxDiskBytes <= 0 {
		maxDiskBytes = 200 << 20
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &SafeGitCloner{
		allowHosts:   allowHosts,
		maxDiskBytes: maxDiskBytes,
		cloneTimeout: timeout,
	}
}

var ErrRepositoryCloneSizeLimit = errors.New("REPOSITORY_CLONE_SIZE_LIMIT")

type CloneDiskLimitError struct {
	SizeBytes  int64
	LimitBytes int64
}

func (e *CloneDiskLimitError) Error() string {
	return fmt.Sprintf("REPOSITORY_CLONE_SIZE_LIMIT: staging tree uses %d bytes; limit is %d bytes", e.SizeBytes, e.LimitBytes)
}

func (e *CloneDiskLimitError) Unwrap() error { return ErrRepositoryCloneSizeLimit }

// ResolveRef resolves a ref before a Snapshot row is created. This makes the
// natural identity repository_id + exact commit_sha available to the API and
// avoids concurrent requests colliding on the placeholder "pending" value.
func (c *SafeGitCloner) ResolveRef(ctx context.Context, gitURL, ref string) (string, error) {
	if err := c.ValidateGitURL(gitURL); err != nil {
		return "", err
	}
	if isFullCommitSHA(ref) {
		resolveCtx, cancel := context.WithTimeout(ctx, c.cloneTimeout)
		defer cancel()
		targetDir, err := os.MkdirTemp("", "repolens-resolve-")
		if err != nil {
			return "", fmt.Errorf("failed to create temporary git resolution directory: %w", err)
		}
		defer os.RemoveAll(targetDir)
		resolved, err := c.CloneCommitTo(resolveCtx, gitURL, ref, targetDir)
		if err != nil {
			if errors.Is(resolveCtx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("git commit resolution timed out after %v", c.cloneTimeout)
			}
			return "", fmt.Errorf("git commit %s is not fetchable: %w", ref, err)
		}
		return resolved, nil
	}
	resolveCtx, cancel := context.WithTimeout(ctx, c.cloneTimeout)
	defer cancel()
	args := []string{"ls-remote", gitURL}
	if ref != "" && !isFullCommitSHA(ref) {
		args = append(args, ref, ref+"^{}", "refs/heads/"+ref, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")
	}
	cmd := exec.CommandContext(resolveCtx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(resolveCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git ref resolution timed out after %v", c.cloneTimeout)
		}
		return "", fmt.Errorf("git ref resolution failed: %w", err)
	}
	commitSHA := resolveCommitFromLsRemote(string(out), ref)
	if commitSHA == "" {
		return "", fmt.Errorf("git ref %q did not resolve to an exact commit SHA", ref)
	}
	return commitSHA, nil
}

func isFullCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func resolveCommitFromLsRemote(output, requestedRef string) string {
	requestedRef = strings.TrimSpace(requestedRef)
	var fallback string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) != 40 {
			continue
		}
		name := fields[1]
		sha := fields[0]
		if strings.HasSuffix(name, "^{}") {
			base := strings.TrimSuffix(name, "^{}")
			if requestedRef == "" || base == requestedRef || base == "refs/tags/"+requestedRef {
				return sha
			}
		}
		if requestedRef != "" && (name == requestedRef || name == "refs/heads/"+requestedRef || name == "refs/tags/"+requestedRef) {
			fallback = sha
		}
		if isFullCommitSHA(requestedRef) && sha == requestedRef {
			return sha
		}
	}
	return fallback
}

func (c *SafeGitCloner) ValidateGitURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid git url: %w", err)
	}

	if strings.ToLower(u.Scheme) != "https" {
		return fmt.Errorf("only HTTPS git urls are allowed, got: %s", u.Scheme)
	}

	host := strings.ToLower(u.Hostname())
	allowed := false
	for _, h := range c.allowHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("git host %s is not in allowlist %v", host, c.allowHosts)
	}

	// Resolve IPs to prevent DNS rebinding / SSRF to private addresses
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve git host %s: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateOrLocalIP(ip) {
			return fmt.Errorf("git host resolved to private or link-local address: %s (%s)", host, ip.String())
		}
	}

	return nil
}

func (c *SafeGitCloner) CloneTo(ctx context.Context, gitURL, ref, targetDir string) (string, error) {
	if err := c.ValidateGitURL(gitURL); err != nil {
		return "", err
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create target dir: %w", err)
	}

	cloneCtx, cancel := context.WithTimeout(ctx, c.cloneTimeout)
	defer cancel()

	// Shallow clone with depth=1
	cmdArgs := []string{"clone", "--depth", "1"}
	if ref != "" {
		cmdArgs = append(cmdArgs, "--branch", ref)
	}
	cmdArgs = append(cmdArgs, gitURL, targetDir)

	out, err := c.runGitCommandWithDiskGuard(cloneCtx, targetDir, cmdArgs...)
	if err != nil {
		_ = os.RemoveAll(targetDir)
		if errors.Is(err, ErrRepositoryCloneSizeLimit) {
			return "", err
		}
		if errors.Is(cloneCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git clone timed out after %v", c.cloneTimeout)
		}
		return "", fmt.Errorf("git clone failed: %w, output: %s", err, string(out))
	}
	// Get commit sha
	shaCmd := exec.CommandContext(ctx, "git", "-C", targetDir, "rev-parse", "HEAD")
	shaOut, err := shaCmd.Output()
	if err != nil {
		_ = os.RemoveAll(targetDir)
		return "", fmt.Errorf("failed to resolve exact cloned commit: %w", err)
	}
	commitSHA := strings.TrimSpace(string(shaOut))
	if len(commitSHA) != 40 {
		_ = os.RemoveAll(targetDir)
		return "", fmt.Errorf("git returned invalid commit SHA %q", commitSHA)
	}

	// Remove .git directory to keep source clean and prevent git-hook execution
	gitDir := filepath.Join(targetDir, ".git")
	_ = os.RemoveAll(gitDir)

	logger.L(ctx).Info("cloned repository snapshot successfully", "commit", commitSHA, "target", targetDir)
	return commitSHA, nil
}

// CloneCommitTo materializes an immutable commit without using a movable
// branch or tag. The caller can compare the returned HEAD with the resolved
// commit before publishing the directory.
func (c *SafeGitCloner) CloneCommitTo(ctx context.Context, gitURL, commitSHA, targetDir string) (string, error) {
	if !isFullCommitSHA(commitSHA) {
		return "", fmt.Errorf("invalid commit SHA %q", commitSHA)
	}
	if err := c.ValidateGitURL(gitURL); err != nil {
		return "", err
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create target dir: %w", err)
	}
	cloneCtx, cancel := context.WithTimeout(ctx, c.cloneTimeout)
	defer cancel()
	commands := [][]string{
		{"init", targetDir},
		{"-C", targetDir, "remote", "add", "origin", gitURL},
		{"-C", targetDir, "fetch", "--depth", "1", "origin", commitSHA},
		{"-C", targetDir, "checkout", "--detach", "FETCH_HEAD"},
		{"-C", targetDir, "rev-parse", "HEAD"},
	}
	for index, args := range commands {
		output, err := c.runGitCommandWithDiskGuard(cloneCtx, targetDir, args...)
		if err != nil {
			_ = os.RemoveAll(targetDir)
			if errors.Is(err, ErrRepositoryCloneSizeLimit) {
				return "", err
			}
			if errors.Is(cloneCtx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("git exact commit materialization timed out after %v", c.cloneTimeout)
			}
			return "", fmt.Errorf("git exact commit command %d failed: %w, output: %s", index+1, err, string(output))
		}
		if index == len(commands)-1 {
			resolved := strings.TrimSpace(string(output))
			if resolved != commitSHA {
				_ = os.RemoveAll(targetDir)
				return "", fmt.Errorf("git returned %s for requested commit %s", resolved, commitSHA)
			}
		}
	}
	_ = os.RemoveAll(filepath.Join(targetDir, ".git"))
	return commitSHA, nil
}

type concurrentCommandOutput struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *concurrentCommandOutput) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(value)
}

func (b *concurrentCommandOutput) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.Buffer.Bytes()...)
}

// runGitCommandWithDiskGuard samples the complete staging tree while a git
// operation is running. The configured size is an actively polled budget,
// with bounded overshoot between samples, rather than a filesystem quota.
func (c *SafeGitCloner) runGitCommandWithDiskGuard(ctx context.Context, targetDir string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "git", args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	var output concurrentCommandOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return output.snapshot(), err
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- cmd.Wait() }()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	stopAndClean := func() error {
		cancel()
		<-waitResult
		return os.RemoveAll(targetDir)
	}
	for {
		select {
		case waitErr := <-waitResult:
			size, overLimit, measureErr := measureCloneDiskUsage(targetDir, c.maxDiskBytes)
			if measureErr != nil {
				_ = os.RemoveAll(targetDir)
				return output.snapshot(), fmt.Errorf("failed to measure clone staging size: %w", measureErr)
			}
			if overLimit {
				limitErr := &CloneDiskLimitError{SizeBytes: size, LimitBytes: c.maxDiskBytes}
				if cleanupErr := os.RemoveAll(targetDir); cleanupErr != nil {
					return output.snapshot(), fmt.Errorf("%w; failed to remove staging directory: %v", limitErr, cleanupErr)
				}
				return output.snapshot(), limitErr
			}
			return output.snapshot(), waitErr
		case <-ticker.C:
			size, overLimit, measureErr := measureCloneDiskUsage(targetDir, c.maxDiskBytes)
			if measureErr != nil {
				cleanupErr := stopAndClean()
				resultErr := fmt.Errorf("failed to measure clone staging size: %w", measureErr)
				if cleanupErr != nil {
					resultErr = fmt.Errorf("%w; failed to remove staging directory: %v", resultErr, cleanupErr)
				}
				return output.snapshot(), resultErr
			}
			if overLimit {
				limitErr := &CloneDiskLimitError{SizeBytes: size, LimitBytes: c.maxDiskBytes}
				if cleanupErr := stopAndClean(); cleanupErr != nil {
					return output.snapshot(), fmt.Errorf("%w; failed to remove staging directory: %v", limitErr, cleanupErr)
				}
				return output.snapshot(), limitErr
			}
		case <-commandCtx.Done():
			cancel()
			waitErr := <-waitResult
			return output.snapshot(), waitErr
		}
	}
}

func measureCloneDiskUsage(targetDir string, limit int64) (int64, bool, error) {
	if limit <= 0 {
		return 0, false, nil
	}
	var total int64
	overLimit := false
	err := filepath.WalkDir(targetDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Size() > limit-total {
			total = limit + 1
			overLimit = true
			return filepath.SkipAll
		}
		total += info.Size()
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return total, overLimit, err
	}
	return total, overLimit, nil
}

func (c *SafeGitCloner) checkCloneDiskLimit(targetDir string) error {
	total, overLimit, err := measureCloneDiskUsage(targetDir, c.maxDiskBytes)
	if err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("failed to measure clone staging size: %w", err)
	}
	if !overLimit {
		return nil
	}
	_ = os.RemoveAll(targetDir)
	return &CloneDiskLimitError{SizeBytes: total, LimitBytes: c.maxDiskBytes}
}
