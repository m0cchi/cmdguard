package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// ============================================================
// cmdguard: A command wrapper/guardrail for Claude Code
//
// Two modes of operation:
//
// 1. Symlink mode (original):
//    Invoked as a symlink (e.g., /opt/cmdguard/bin/git)
//    Validates args against policy, then exec's the real binary.
//
// 2. Exec mode:
//    cmdguard exec [options] <command> [args...]
//    Sets up a guarded environment and launches the command.
//    - Creates tmpdir with symlinks for allowed commands only
//    - Sets PATH=tmpdir, ORIGINAL_PATH=original PATH
//    - Optionally uses Linux mount namespaces for full isolation
// ============================================================

const (
	policyFileName  = "cmdguard.yaml"
	envOriginalPath = "ORIGINAL_PATH"
)

// --- YAML policy structures ---

type Policy struct {
	Commands map[string]CommandPolicy `yaml:"commands"`
}

type CommandPolicy struct {
	GlobalOptions      []string                    `yaml:"global_options"`
	GlobalValueOptions []string                    `yaml:"global_value_options"`
	Subcommands        map[string]SubcommandPolicy `yaml:"subcommands"`
	AllowBare          bool                        `yaml:"allow_bare"`
	BareOptions        []string                    `yaml:"bare_options"`
	BareValueOptions   []string                    `yaml:"bare_value_options"`
}

type SubcommandPolicy struct {
	Allow        bool     `yaml:"allow"`
	Options      []string `yaml:"options"`
	ValueOptions []string `yaml:"value_options"`
	AllowAnyArgs bool     `yaml:"allow_any_args"`
}

func main() {
	invokedPath := os.Args[0]
	cmdName := filepath.Base(invokedPath)

	if cmdName == "cmdguard" {
		runGuardCommand()
		return
	}

	// Symlink mode
	runSymlinkMode(invokedPath, cmdName)
}

// ============================================================
// Guard command dispatcher
// ============================================================

func runGuardCommand() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "exec":
		runExecMode()
	case "list":
		runListMode()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "[cmdguard] unknown subcommand: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `cmdguard - Command guardrail for Claude Code

Usage:
  cmdguard exec [options] <command> [args...]
    Set up guarded environment and run <command>.

  cmdguard list
    Show allowed commands from policy.

  cmdguard help
    Show this help.

Exec options:
  --namespace        Use mount namespace to hide original binaries
                     (requires root or CAP_SYS_ADMIN)
  --keep-tmpdir      Don't clean up the tmpdir on exit (for debugging)
  --policy <path>    Use a specific policy file instead of auto-detecting

Examples:
  cmdguard exec claude         # launch Claude Code in guarded env
  cmdguard exec bash           # launch bash with only allowed commands
  cmdguard exec --namespace bash  # same, with mount namespace isolation
`)
}

// ============================================================
// Exec mode
// ============================================================

func runExecMode() {
	args := os.Args[2:]
	useNamespace := false
	keepTmpdir := false
	policyPath := ""

	// Parse exec options (stop at first non-option or --)
	idx := 0
	for idx < len(args) {
		switch args[idx] {
		case "--":
			idx++ // skip the -- itself, rest is command + args
			goto doneOpts
		case "--namespace":
			useNamespace = true
			idx++
		case "--keep-tmpdir":
			keepTmpdir = true
			idx++
		case "--policy":
			if idx+1 >= len(args) {
				fatal("--policy requires an argument")
			}
			policyPath = args[idx+1]
			idx += 2
		default:
			goto doneOpts
		}
	}
doneOpts:

	if idx >= len(args) {
		fatal("exec requires a command to run.\n\nUsage: cmdguard exec [options] <command> [args...]")
	}

	targetCmd := args[idx]
	targetArgs := args[idx+1:]

	// Load policy
	var policy *Policy
	var err error
	if policyPath != "" {
		policy, err = loadPolicyFrom(policyPath)
	} else {
		policy, err = loadPolicyFromSelf()
	}
	if err != nil {
		fatal("failed to load policy: %v", err)
	}

	// Save current PATH → becomes ORIGINAL_PATH for child
	currentPath := os.Getenv("PATH")
	if currentPath == "" {
		currentPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	// Resolve our own binary path (symlinks will point here)
	guardBin := resolveGuardBin()

	// Create tmpdir with guarded bin/ directory
	tmpDir, binDir, err := createGuardedBinDir(policy, guardBin, policyPath)
	if err != nil {
		fatal("%v", err)
	}
	if !keepTmpdir {
		defer os.RemoveAll(tmpDir)
	} else {
		fmt.Fprintf(os.Stderr, "[cmdguard] tmpdir: %s\n", tmpDir)
	}

	// Find target binary BEFORE any namespace shenanigans
	realTarget, err := findInPath(targetCmd, currentPath)
	if err != nil {
		fatal("target command %q not found in PATH: %v", targetCmd, err)
	}

	if useNamespace {
		execWithNamespace(realTarget, targetCmd, targetArgs, binDir, currentPath, policy)
		// does not return
	}

	// Non-namespace mode: just swap PATH and exec
	env := buildExecEnv(binDir, currentPath)
	execCmd(realTarget, targetCmd, targetArgs, env)
}

// resolveGuardBin returns the absolute, symlink-resolved path to this binary.
func resolveGuardBin() string {
	p, err := os.Executable()
	if err != nil {
		p, _ = os.Readlink("/proc/self/exe")
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return resolved
}

// createGuardedBinDir creates a tmpdir with a bin/ subdirectory containing:
//   - symlinks for each command in the policy → guard binary
//   - a copy of the policy file (read-only)
//
// Returns (tmpDir, binDir, error).
func createGuardedBinDir(policy *Policy, guardBin string, policyPath string) (string, string, error) {
	tmpDir, err := os.MkdirTemp("", "cmdguard-*")
	if err != nil {
		return "", "", fmt.Errorf("failed to create tmpdir: %v", err)
	}

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.Mkdir(binDir, 0755); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("failed to create bin dir: %v", err)
	}

	// Copy guard binary into binDir so that EvalSymlinks(binDir/cmd) resolves
	// to binDir/.cmdguard-bin, allowing loadPolicy to find cmdguard.yaml in binDir.
	guardCopy := filepath.Join(binDir, ".cmdguard-bin")
	if err := copyFile(guardBin, guardCopy); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("failed to copy guard binary: %v", err)
	}
	os.Chmod(guardCopy, 0755)

	// Symlink each policy command → local guard copy
	for cmdName := range policy.Commands {
		linkPath := filepath.Join(binDir, cmdName)
		if err := os.Symlink(guardCopy, linkPath); err != nil {
			fmt.Fprintf(os.Stderr, "[cmdguard] warning: symlink %s: %v\n", cmdName, err)
		}
	}

	// Copy policy file so symlinked invocations can find it
	policyData, err := readPolicyBytes(policyPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("failed to read policy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, policyFileName), policyData, 0444); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("failed to write policy to tmpdir: %v", err)
	}

	return tmpDir, binDir, nil
}

// execWithNamespace creates a mount namespace and bind-mounts the guarded bin
// directory over every directory in the original PATH. This prevents the child
// process from directly running unguarded binaries even with absolute paths,
// because the original bin directories now only contain our symlinks.
//
// The target binary is copied into the tmpdir BEFORE bind-mounting, so it
// remains accessible after the original PATH dirs are masked.
func execWithNamespace(realTarget string, targetCmd string, targetArgs []string, binDir string, origPath string, policy *Policy) {
	tmpDir := filepath.Dir(binDir)

	// --- 1. Create origbin/ with copies of real binaries ---
	// After bind-mounting, the original /usr/bin etc. are hidden.
	// The guard symlinks in binDir will look up ORIGINAL_PATH to find real
	// binaries, so we point ORIGINAL_PATH to this origbin/ directory.
	origBinDir := filepath.Join(tmpDir, "origbin")
	if err := os.Mkdir(origBinDir, 0755); err != nil {
		fatal("failed to create origbin dir: %v", err)
	}

	for cmdName := range policy.Commands {
		realPath, err := findInPath(cmdName, origPath)
		if err != nil {
			continue
		}
		dst := filepath.Join(origBinDir, cmdName)
		if err := copyFile(realPath, dst); err != nil {
			fmt.Fprintf(os.Stderr, "[cmdguard] warning: copy %s: %v\n", cmdName, err)
			continue
		}
		os.Chmod(dst, 0755)
	}

	// Also copy target command if not already in origbin (e.g. bash, claude)
	targetInOrigBin := filepath.Join(origBinDir, targetCmd)
	if _, err := os.Stat(targetInOrigBin); os.IsNotExist(err) {
		if err := copyFile(realTarget, targetInOrigBin); err != nil {
			fatal("failed to copy target %s: %v", targetCmd, err)
		}
		os.Chmod(targetInOrigBin, 0755)
	}

	// --- 2. Copy target binary for exec ---
	targetCopy := filepath.Join(tmpDir, ".exec-target")
	if err := copyFile(realTarget, targetCopy); err != nil {
		fatal("failed to copy target binary: %v", err)
	}
	os.Chmod(targetCopy, 0755)

	// --- 3. Create mount namespace ---
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		fatal("unshare(CLONE_NEWNS) failed: %v\n  Need root or CAP_SYS_ADMIN. Try without --namespace.", err)
	}

	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		fmt.Fprintf(os.Stderr, "[cmdguard] warning: MS_PRIVATE failed: %v\n", err)
	}

	// Directories that must NOT be masked
	essentialDirs := map[string]bool{
		"/lib": true, "/lib32": true, "/lib64": true, "/libx32": true,
		"/usr/lib": true, "/usr/lib32": true, "/usr/lib64": true,
		"/etc": true, "/tmp": true, "/dev": true, "/proc": true,
		"/sys": true, "/run": true, "/var": true, "/home": true,
		"/root": true, "/opt": true, "/mnt": true, "/media": true,
	}

	// --- 5. Bind-mount guarded binDir over each PATH directory ---
	for _, dir := range filepath.SplitList(origPath) {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if essentialDirs[absDir] {
			continue
		}
		if _, err := os.Stat(absDir); err != nil {
			continue
		}
		if err := syscall.Mount(binDir, absDir, "", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
			fmt.Fprintf(os.Stderr, "[cmdguard] warning: bind-mount over %s: %v\n", absDir, err)
		}
	}

	// ORIGINAL_PATH → origbin/ (where the real binary copies live)
	env := buildExecEnv(binDir, origBinDir)
	execCmd(targetCopy, targetCmd, targetArgs, env)
}

// execCmd does the final syscall.Exec (does not return on success).
func execCmd(realTarget string, cmdName string, args []string, env []string) {
	execArgs := append([]string{cmdName}, args...)
	if err := syscall.Exec(realTarget, execArgs, env); err != nil {
		fatal("exec %s failed: %v", cmdName, err)
	}
}

// ============================================================
// List mode
// ============================================================

func runListMode() {
	policy, err := loadPolicyFromSelf()
	if err != nil {
		fatal("failed to load policy: %v", err)
	}

	for cmdName, cmdPolicy := range policy.Commands {
		fmt.Printf("%s:\n", cmdName)
		if len(cmdPolicy.GlobalOptions) > 0 {
			fmt.Printf("  global options: %s\n", strings.Join(cmdPolicy.GlobalOptions, ", "))
		}
		if cmdPolicy.AllowBare {
			fmt.Printf("  allow bare: yes\n")
			if len(cmdPolicy.BareOptions) > 0 {
				fmt.Printf("  bare options: %s\n", strings.Join(cmdPolicy.BareOptions, ", "))
			}
		}
		for subName, subPolicy := range cmdPolicy.Subcommands {
			status := "DENY"
			if subPolicy.Allow {
				status = "ALLOW"
			}
			fmt.Printf("  %-20s %s\n", subName, status)
			if subPolicy.Allow && len(subPolicy.Options) > 0 {
				fmt.Printf("    options: %s\n", strings.Join(subPolicy.Options, ", "))
			}
		}
		fmt.Println()
	}
}

// ============================================================
// Symlink mode (original behavior)
// ============================================================

func runSymlinkMode(invokedPath string, cmdName string) {
	policy, err := loadPolicy(invokedPath)
	if err != nil {
		fatal("failed to load policy: %v", err)
	}

	cmdPolicy, ok := policy.Commands[cmdName]
	if !ok {
		fatal("command %q is not defined in policy", cmdName)
	}

	args := os.Args[1:]
	if err := validateArgs(cmdName, cmdPolicy, args); err != nil {
		fatal("blocked: %v", err)
	}

	realBin, err := findInOriginalPath(cmdName)
	if err != nil {
		fatal("%v", err)
	}

	execArgs := append([]string{cmdName}, args...)
	env := buildEnv()
	if err := syscall.Exec(realBin, execArgs, env); err != nil {
		fatal("exec failed: %v", err)
	}
}

// ============================================================
// Policy loading
// ============================================================

func loadPolicy(invokedPath string) (*Policy, error) {
	realPath, err := filepath.EvalSymlinks(invokedPath)
	if err != nil {
		realPath, err = os.Readlink("/proc/self/exe")
		if err != nil {
			return nil, fmt.Errorf("cannot determine binary location: %v", err)
		}
	}
	return loadPolicyFrom(filepath.Join(filepath.Dir(realPath), policyFileName))
}

func loadPolicyFromSelf() (*Policy, error) {
	p := resolveGuardBin()
	return loadPolicyFrom(filepath.Join(filepath.Dir(p), policyFileName))
}

func loadPolicyFrom(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", path, err)
	}
	var policy Policy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("invalid policy YAML: %v", err)
	}
	return &policy, nil
}

func readPolicyBytes(explicitPath string) ([]byte, error) {
	if explicitPath != "" {
		return os.ReadFile(explicitPath)
	}
	p := resolveGuardBin()
	return os.ReadFile(filepath.Join(filepath.Dir(p), policyFileName))
}

// ============================================================
// PATH search helpers
// ============================================================

func findInPath(cmdName string, pathStr string) (string, error) {
	for _, dir := range filepath.SplitList(pathStr) {
		candidate := filepath.Join(dir, cmdName)
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("not found in PATH")
}

func findInOriginalPath(cmdName string) (string, error) {
	origPath := os.Getenv(envOriginalPath)
	if origPath == "" {
		return "", fmt.Errorf("environment variable %s is not set", envOriginalPath)
	}

	selfResolved, _ := filepath.EvalSymlinks("/proc/self/exe")

	for _, dir := range filepath.SplitList(origPath) {
		candidate := filepath.Join(dir, cmdName)

		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if resolved == selfResolved {
			continue // skip ourselves
		}

		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("command %q not found in ORIGINAL_PATH", cmdName)
}

// ============================================================
// Environment helpers
// ============================================================

func buildExecEnv(guardBinDir string, origPath string) []string {
	var env []string
	for _, e := range os.Environ() {
		key := strings.SplitN(e, "=", 2)[0]
		if key == "PATH" || key == envOriginalPath {
			continue
		}
		env = append(env, e)
	}
	env = append(env, "PATH="+guardBinDir)
	env = append(env, envOriginalPath+"="+origPath)
	return env
}

func buildEnv() []string {
	return os.Environ()
}

// ============================================================
// Argument validation
// ============================================================

func validateArgs(cmdName string, policy CommandPolicy, args []string) error {
	if len(args) == 0 {
		if !policy.AllowBare {
			return fmt.Errorf("%s: running without a subcommand is not allowed", cmdName)
		}
		return nil
	}

	globalOpts := buildOptionSet(policy.GlobalOptions)
	globalValueOpts := buildOptionSet(policy.GlobalValueOptions)
	idx := 0

	knownSubcmds := make(map[string]bool, len(policy.Subcommands))
	for k := range policy.Subcommands {
		knownSubcmds[k] = true
	}

	for idx < len(args) {
		arg := args[idx]
		if arg == "--" {
			if !policy.AllowBare {
				return fmt.Errorf("%s: bare positional arguments not allowed", cmdName)
			}
			return validateBareOptions(cmdName, policy, args[:idx])
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		if !isAllowedOption(arg, globalOpts) {
			if policy.AllowBare {
				return validateBareArgs(cmdName, policy, args, globalOpts)
			}
			return fmt.Errorf("%s: global option %q is not allowed", cmdName, arg)
		}
		idx++
		if !strings.Contains(arg, "=") && idx < len(args) && !strings.HasPrefix(args[idx], "-") && globalValueOpts[arg] {
			if !knownSubcmds[args[idx]] {
				idx++
			}
		}
	}

	if idx >= len(args) {
		if !policy.AllowBare {
			return fmt.Errorf("%s: no subcommand specified", cmdName)
		}
		return nil
	}

	subCmd := args[idx]

	subPolicy, ok := policy.Subcommands[subCmd]
	if !ok {
		// Not a known subcommand. If allow_bare, treat everything as bare args.
		if policy.AllowBare {
			return validateBareArgs(cmdName, policy, args, globalOpts)
		}
		return fmt.Errorf("%s: subcommand %q is not allowed", cmdName, subCmd)
	}
	if !subPolicy.Allow {
		return fmt.Errorf("%s: subcommand %q is explicitly denied", cmdName, subCmd)
	}

	subArgs := args[idx+1:]
	subOpts := buildOptionSet(subPolicy.Options)
	mergedOpts := mergeOptionSets(globalOpts, subOpts)
	valueOpts := mergeOptionSets(globalValueOpts, buildOptionSet(subPolicy.ValueOptions))

	for i := 0; i < len(subArgs); i++ {
		arg := subArgs[i]
		if arg == "--" {
			if !subPolicy.AllowAnyArgs {
				return fmt.Errorf("%s %s: positional arguments after -- are not allowed", cmdName, subCmd)
			}
			break
		}
		if strings.HasPrefix(arg, "-") {
			if !isAllowedOption(arg, mergedOpts) {
				return fmt.Errorf("%s %s: option %q is not allowed", cmdName, subCmd, arg)
			}
			if !strings.Contains(arg, "=") && i+1 < len(subArgs) && !strings.HasPrefix(subArgs[i+1], "-") && valueOpts[arg] {
				i++
			}
		} else {
			if !subPolicy.AllowAnyArgs {
				return fmt.Errorf("%s %s: positional argument %q is not allowed (allow_any_args is false)", cmdName, subCmd, arg)
			}
		}
	}

	return nil
}

func validateBareArgs(cmdName string, policy CommandPolicy, args []string, globalOpts map[string]bool) error {
	bareOpts := buildOptionSet(policy.BareOptions)
	merged := mergeOptionSets(globalOpts, bareOpts)
	valueOpts := mergeOptionSets(buildOptionSet(policy.GlobalValueOptions), buildOptionSet(policy.BareValueOptions))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "-") {
			if !isAllowedOption(arg, merged) {
				return fmt.Errorf("%s: option %q is not allowed", cmdName, arg)
			}
			if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && valueOpts[arg] {
				i++
			}
		}
	}
	return nil
}

func validateBareOptions(cmdName string, policy CommandPolicy, args []string) error {
	return validateBareArgs(cmdName, policy, args, buildOptionSet(policy.GlobalOptions))
}

func buildOptionSet(opts []string) map[string]bool {
	set := make(map[string]bool, len(opts))
	for _, o := range opts {
		set[o] = true
	}
	return set
}

func mergeOptionSets(a, b map[string]bool) map[string]bool {
	merged := make(map[string]bool, len(a)+len(b))
	for k, v := range a {
		merged[k] = v
	}
	for k, v := range b {
		merged[k] = v
	}
	return merged
}

func isAllowedOption(arg string, allowed map[string]bool) bool {
	if allowed[arg] {
		return true
	}
	if strings.HasPrefix(arg, "--") {
		if eqIdx := strings.Index(arg, "="); eqIdx >= 0 {
			if allowed[arg[:eqIdx]] {
				return true
			}
		}
	}
	if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
		if allowed[arg[:2]] {
			return true
		}
	}
	return false
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "[cmdguard] "+format+"\n", a...)
	os.Exit(126)
}
