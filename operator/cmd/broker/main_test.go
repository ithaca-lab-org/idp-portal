package main

import (
	"os"
	"strings"
	"testing"
)

// controller-runtime panics ("close of closed channel") when SetupSignalHandler is called twice.
// The enabled broker did exactly that on its first start (2026-11-08); the idle path never reached
// the second call, so no other test saw it. Keep a single call in this file.
func TestSignalHandlerIsInstalledOnce(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "SetupSignalHandler("); n != 1 {
		t.Fatalf("main.go calls SetupSignalHandler %d times; it must be called exactly once and the context shared", n)
	}
}
