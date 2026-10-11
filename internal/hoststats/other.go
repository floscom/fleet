//go:build !linux && !darwin

package hoststats

import (
	"context"
	"runtime"
)

func readSystem(_ context.Context, s *Stats) { s.OSName = runtime.GOOS }

func diskOf(string) (Disk, string, bool) { return Disk{}, "", false }
