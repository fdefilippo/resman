package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"runtime"
	"time"
)

const memoryFixtureBytes = 96 * 1024 * 1024

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s {cpu|memory|io} duration\n", os.Args[0])
		os.Exit(2)
	}
	duration, err := time.ParseDuration(os.Args[2])
	if err != nil || duration <= 0 {
		fmt.Fprintf(os.Stderr, "invalid duration %q\n", os.Args[2])
		os.Exit(2)
	}

	deadline := time.Now().Add(duration)
	switch os.Args[1] {
	case "cpu":
		runCPU(deadline)
	case "memory":
		runMemory(deadline)
	case "io":
		if err := runIO(deadline); err != nil {
			fmt.Fprintf(os.Stderr, "I/O fixture failed: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown workload %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runCPU(deadline time.Time) {
	data := make([]byte, 64*1024)
	var digest [sha256.Size]byte
	for time.Now().Before(deadline) {
		digest = sha256.Sum256(append(data, digest[:]...))
	}
	runtime.KeepAlive(digest)
}

func runMemory(deadline time.Time) {
	data := make([]byte, memoryFixtureBytes)
	for time.Now().Before(deadline) {
		for offset := 0; offset < len(data); offset += os.Getpagesize() {
			data[offset]++
		}
		runtime.Gosched()
	}
	runtime.KeepAlive(data)
}

func runIO(deadline time.Time) (returnErr error) {
	file, err := os.Create("payload")
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("close payload: %w", err)
		}
	}()

	block := make([]byte, 1024*1024)
	for time.Now().Before(deadline) {
		if err := file.Truncate(0); err != nil {
			return fmt.Errorf("truncate payload: %w", err)
		}
		if _, err := file.Seek(0, 0); err != nil {
			return fmt.Errorf("seek payload: %w", err)
		}
		for written := 0; written < 32; written++ {
			if _, err := file.Write(block); err != nil {
				return fmt.Errorf("write payload block %d: %w", written, err)
			}
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync payload: %w", err)
		}
	}
	return nil
}
