package main

import (
	"testing"
)

func testPolicy() CommandPolicy {
	return CommandPolicy{
		GlobalOptions: []string{"--no-pager", "-C", "-c"},
		AllowBare:     false,
		Subcommands: map[string]SubcommandPolicy{
			"status": {
				Allow:        true,
				Options:      []string{"-s", "--short", "--porcelain"},
				AllowAnyArgs: true,
			},
			"commit": {
				Allow:        true,
				Options:      []string{"-m", "--message", "-a", "--amend"},
				AllowAnyArgs: false,
			},
			"clean": {
				Allow: false,
			},
			"log": {
				Allow:        true,
				Options:      []string{"--oneline", "--graph", "-n", "--format"},
				AllowAnyArgs: true,
			},
		},
	}
}

func barePolicy() CommandPolicy {
	return CommandPolicy{
		AllowBare:   true,
		BareOptions: []string{"-l", "-a", "-la", "-R"},
	}
}

func TestAllowedSubcommand(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"status"})
	if err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}
}

func TestAllowedSubcommandWithOptions(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"status", "-s"})
	if err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}
}

func TestAllowedSubcommandWithArgs(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"status", "src/"})
	if err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}
}

func TestDeniedSubcommand(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"clean", "-fd"})
	if err == nil {
		t.Error("expected denied for 'clean'")
	}
}

func TestUnknownSubcommand(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"format-patch"})
	if err == nil {
		t.Error("expected denied for unknown subcommand")
	}
}

func TestDeniedOption(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"status", "--unknown-flag"})
	if err == nil {
		t.Error("expected denied for unknown option")
	}
}

func TestGlobalOption(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"--no-pager", "log", "--oneline"})
	if err != nil {
		t.Errorf("expected allowed with global option, got: %v", err)
	}
}

func TestGlobalOptionWithValue(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"-C", "/tmp/repo", "status"})
	if err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}
}

func TestNoSubcommandDenied(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{})
	if err == nil {
		t.Error("expected denied for bare invocation")
	}
}

func TestCommitNoPositionalArgs(t *testing.T) {
	p := testPolicy()
	// commit with -m is fine
	err := validateArgs("git", p, []string{"commit", "-m", "fix bug"})
	if err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}
}

func TestCommitDenyPositionalArg(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"commit", "somefile.txt"})
	if err == nil {
		t.Error("expected denied for positional arg when allow_any_args=false")
	}
}

func TestOptionWithEquals(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"log", "--format=oneline"})
	if err != nil {
		t.Errorf("expected allowed for --format=oneline, got: %v", err)
	}
}

func TestShortOptionWithValue(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"log", "-n5"})
	if err != nil {
		t.Errorf("expected allowed for -n5, got: %v", err)
	}
}

func TestBareCommand(t *testing.T) {
	p := barePolicy()
	err := validateArgs("ls", p, []string{"-la", "/tmp"})
	if err != nil {
		t.Errorf("expected allowed for bare command, got: %v", err)
	}
}

func TestBareCommandDeniedOption(t *testing.T) {
	p := barePolicy()
	err := validateArgs("ls", p, []string{"--forbidden"})
	if err == nil {
		t.Error("expected denied for unknown bare option")
	}
}

func TestBareCommandNoArgs(t *testing.T) {
	p := barePolicy()
	err := validateArgs("ls", p, []string{})
	if err != nil {
		t.Errorf("expected allowed for bare with no args, got: %v", err)
	}
}

func TestDeniedGlobalOption(t *testing.T) {
	p := testPolicy()
	err := validateArgs("git", p, []string{"--exec-path=/evil", "status"})
	if err == nil {
		t.Error("expected denied for disallowed global option")
	}
}

func TestIsAllowedOption(t *testing.T) {
	allowed := map[string]bool{
		"-v":       true,
		"--format": true,
		"-n":       true,
	}

	tests := []struct {
		arg    string
		expect bool
	}{
		{"-v", true},
		{"--format", true},
		{"--format=json", true},
		{"-n", true},
		{"-n5", true},
		{"--unknown", false},
		{"-x", false},
	}

	for _, tt := range tests {
		got := isAllowedOption(tt.arg, allowed)
		if got != tt.expect {
			t.Errorf("isAllowedOption(%q) = %v, want %v", tt.arg, got, tt.expect)
		}
	}
}
