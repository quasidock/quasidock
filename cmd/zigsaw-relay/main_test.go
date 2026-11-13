package main

import (
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func TestParseArg(t *testing.T) {
	// Service
	arg, ok := parseArg("412=23817")
	if !ok || arg.Kind != ArgService || arg.Socket != 412 || arg.Port != 23817 {
		t.Fatalf("expected socket 412, port 23817, got %v", arg)
	}

	arg, ok = parseArg("508=23817")
	if !ok || arg.Kind != ArgService || arg.Socket != 508 || arg.Port != 23817 {
		t.Fatalf("expected socket 508, port 23817, got %v", arg)
	}

	// Forward
	arg, ok = parseArg("forward=600:604")
	if !ok || arg.Kind != ArgForward || arg.Forward.In != win32.HANDLE(600) || arg.Forward.Out != win32.HANDLE(604) {
		t.Fatalf("expected forward in=600, out=604, got %v", arg)
	}

	// Use
	arg, ok = parseArg("use=31000")
	if !ok || arg.Kind != ArgUse || arg.Use != 31000 {
		t.Fatalf("expected use=31000, got %v", arg)
	}

	// Bare service
	arg, ok = parseArg("1=2")
	if !ok || arg.Kind != ArgService || arg.Port != 2 {
		t.Fatalf("expected port 2, got %v", arg)
	}

	// Invalid cases
	badCases := []string{
		"1=0", "1=65536", "=5", "x=5", "5",
		"use=0", "use=x", "forward=1", "forward=0:1", "forward=1:",
	}
	for _, bad := range badCases {
		if _, ok := parseArg(bad); ok {
			t.Errorf("expected %q to fail parsing", bad)
		}
	}
}
