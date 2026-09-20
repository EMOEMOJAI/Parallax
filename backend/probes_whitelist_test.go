package main

import (
	"slices"
	"testing"
)

// S7 — the server-side half of the four native probes is two whitelist entries,
// and the whole point of them is which list each type is *not* in. Every
// existing backend test file is out of this slice's scope and none of them
// enumerates either whitelist, so the acceptance lives here.
//
// Note the scope of these assertions: they are **default-configuration** claims,
// not mechanism claims. PUBLIC_COMMANDS is operator environment, so an operator
// can publish any command type; what is asserted is that the shipped defaults
// keep the new probes out of public mode, not that the server refuses them
// there under every configuration.

// s7NativeProbeTypes must stay in step with agent/main.go's nativeProbeTypes and
// the agent's nativeProbes table. It is written out literally rather than
// imported because the agent is a separate Go module.
var s7NativeProbeTypes = []string{"tcp", "tls", "dnsbench", "download"}

// s7SchedulableNativeTypes are the three cheap ones. `download` is excluded on
// purpose (F10): up to 100 MB per run on every tick.
var s7SchedulableNativeTypes = []string{"tcp", "tls", "dnsbench"}

func TestS7AllFourNativeProbesAreDispatchable(t *testing.T) {
	for _, cmdType := range s7NativeProbeTypes {
		if !allowedCommandTypes[cmdType] {
			t.Errorf("allowedCommandTypes is missing %q — the server would refuse the command before it reached any agent", cmdType)
		}
	}
	// And the pre-S7 set is untouched.
	for _, cmdType := range []string{"ping", "traceroute", "mtr", "nexttrace", "iperf3", "speedtest", "dns", "http"} {
		if !allowedCommandTypes[cmdType] {
			t.Errorf("allowedCommandTypes lost %q", cmdType)
		}
	}
	// "shell" stays out: interactive shells use the shell_start/shell_input
	// action pair, never a command type.
	if allowedCommandTypes["shell"] {
		t.Errorf(`allowedCommandTypes contains "shell"`)
	}
	if got, want := len(allowedCommandTypes), 12; got != want {
		t.Errorf("allowedCommandTypes has %d entries, want %d (8 pre-S7 + 4 native)", got, want)
	}
}

func TestS7DownloadIsNotSchedulableWhileTheOtherThreeAre(t *testing.T) {
	if scheduleAllowedCommands["download"] {
		t.Errorf(`scheduleAllowedCommands contains "download": a 100 MB transfer on every tick is not a monitoring probe (F10)`)
	}
	for _, cmdType := range s7SchedulableNativeTypes {
		if !scheduleAllowedCommands[cmdType] {
			t.Errorf("scheduleAllowedCommands is missing %q", cmdType)
		}
	}
	// The existing exclusions are unchanged.
	for _, cmdType := range []string{"iperf3", "speedtest", "shell", "nexttrace"} {
		if scheduleAllowedCommands[cmdType] {
			t.Errorf("scheduleAllowedCommands unexpectedly contains %q", cmdType)
		}
	}
	// Every schedulable type must also be dispatchable, or a schedule could
	// never run.
	for cmdType := range scheduleAllowedCommands {
		if !allowedCommandTypes[cmdType] {
			t.Errorf("scheduleAllowedCommands[%q] is not in allowedCommandTypes", cmdType)
		}
	}
	if got, want := len(scheduleAllowedCommands), 8; got != want {
		t.Errorf("scheduleAllowedCommands has %d entries, want %d (5 pre-S7 + tcp/tls/dnsbench)", got, want)
	}
}

func TestS7NoNativeProbeIsInThePublicDefaults(t *testing.T) {
	// Default configuration: PUBLIC_COMMANDS unset. t.Setenv("") is treated as
	// unset by splitEnvList, which is what the deployed default is.
	t.Setenv("PUBLIC_COMMANDS", "")
	t.Setenv("PUBLIC_TARGETS", "")

	defaults := publicAllowedCommands()
	want := []string{"ping", "traceroute", "mtr", "dns"}
	if !slices.Equal(defaults, want) {
		t.Fatalf("publicAllowedCommands() = %v, want the unchanged default %v", defaults, want)
	}
	for _, cmdType := range s7NativeProbeTypes {
		if publicCommandAllowed(cmdType) {
			t.Errorf("a public session can reach %q by default", cmdType)
		}
	}
	// The empty-by-default target allowlist is the second half of that
	// property: a public session has nothing to aim any command at.
	if len(publicAllowedTargets()) != 0 {
		t.Errorf("publicAllowedTargets() = %v, want empty by default", publicAllowedTargets())
	}
	if publicTargetAllowed("example.com") {
		t.Errorf("an arbitrary target is allowed for public sessions by default")
	}
}

// TestS7TheTwoWhitelistsCannotDriftSilently is the guard the slice is really
// after: it fails if `download` is ever added to the schedule whitelist, or if
// any of the four goes missing from allowedCommandTypes, or if a fifth native
// type is added on one side only.
func TestS7TheTwoWhitelistsCannotDriftSilently(t *testing.T) {
	for cmdType := range allowedCommandTypes {
		if !slices.Contains(s7NativeProbeTypes, cmdType) {
			continue
		}
		schedulable := scheduleAllowedCommands[cmdType]
		expected := slices.Contains(s7SchedulableNativeTypes, cmdType)
		if schedulable != expected {
			t.Errorf("native type %q: schedulable = %v, want %v", cmdType, schedulable, expected)
		}
	}
	for _, cmdType := range s7NativeProbeTypes {
		if !allowedCommandTypes[cmdType] {
			t.Errorf("native type %q vanished from allowedCommandTypes", cmdType)
		}
	}
	// A native type must never be a *schedule-only* entry either.
	for cmdType := range scheduleAllowedCommands {
		if slices.Contains(s7NativeProbeTypes, cmdType) && !slices.Contains(s7SchedulableNativeTypes, cmdType) {
			t.Errorf("native type %q became schedulable", cmdType)
		}
	}
}
