package main

import (
	"context"
	"os"
	"time"
)

// contextWithTimeout is a thin alias so credential and catalog reads share one
// obvious timeout policy.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// commandEnviron returns the process environment for the app helper subprocess.
func commandEnviron() []string {
	return os.Environ()
}
