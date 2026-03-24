/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
package metrics

import (
    "os"
    "testing"
    "time"

    "github.com/fdefilippo/resman/config"
)

func TestUserMetricsStruct(t *testing.T) {
    um := &UserMetrics{
        UID:          1000,
        Username:     "testuser",
        CPUUsage:     25.5,
        MemoryUsage:  104857600,
        ProcessCount: 10,
    }

    if um.UID != 1000 {
        t.Errorf("UID: got %d, expected 1000", um.UID)
    }
    if um.Username != "testuser" {
        t.Errorf("Username: got %s, expected testuser", um.Username)
    }
    if um.CPUUsage != 25.5 {
        t.Errorf("CPUUsage: got %f, expected 25.5", um.CPUUsage)
    }
    if um.MemoryUsage != 104857600 {
        t.Errorf("MemoryUsage: got %d, expected 104857600", um.MemoryUsage)
    }
    if um.ProcessCount != 10 {
        t.Errorf("ProcessCount: got %d, expected 10", um.ProcessCount)
    }
}

func TestNewCollector(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)

    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }
    if collector == nil {
        t.Fatal("NewCollector() returned nil")
    }
    if collector.cfg != cfg {
        t.Error("collector.cfg not set correctly")
    }
    if collector.cache == nil {
        t.Error("collector.cache not initialized")
    }
    if collector.cacheTimestamps == nil {
        t.Error("collector.cacheTimestamps not initialized")
    }
}

func TestGetTotalCores(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    cores := collector.GetTotalCores()
    if cores < 1 {
        t.Errorf("GetTotalCores() returned %d, expected >= 1", cores)
    }
}

func TestGetTotalCPUUsage(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    usage := collector.GetTotalCPUUsage()
    // CPU usage should be between 0 and 100+ (can exceed 100 on multi-core)
    if usage < 0 {
        t.Errorf("GetTotalCPUUsage() returned %f, expected >= 0", usage)
    }
}

func TestGetMemoryUsage(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    usage := collector.GetMemoryUsage()
    if usage < 0 {
        t.Errorf("GetMemoryUsage() returned %f, expected >= 0", usage)
    }
}

func TestGetAllUsers(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    users := collector.GetAllUsers()
    // Should return at least root user
    if len(users) < 1 {
        t.Errorf("GetAllUsers() returned %d users, expected >= 1", len(users))
    }

    // Verify all users are in valid range
    for _, uid := range users {
        if uid < cfg.SystemUIDMin || uid > cfg.SystemUIDMax {
            t.Errorf("GetAllUsers() returned invalid UID %d", uid)
        }
    }
}

func TestGetAllUserMetrics(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    userMetrics := collector.GetAllUserMetrics()
    if userMetrics == nil {
        t.Error("GetAllUserMetrics() returned nil")
    }

    // Verify structure of returned metrics
    for uid, metrics := range userMetrics {
        if metrics == nil {
            t.Errorf("GetAllUserMetrics() returned nil metrics for UID %d", uid)
            continue
        }
        if metrics.UID != uid {
            t.Errorf("Metrics UID mismatch: got %d, expected %d", metrics.UID, uid)
        }
        if metrics.CPUUsage < 0 {
            t.Errorf("CPUUsage for UID %d is negative: %f", uid, metrics.CPUUsage)
        }
        if metrics.ProcessCount < 0 {
            t.Errorf("ProcessCount for UID %d is negative: %d", uid, metrics.ProcessCount)
        }
    }
}

func TestGetUserMemoryUsage(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    // Test with a valid UID (root = 0, might not be in range)
    // Test with current user if available
    currentUID := os.Getuid()
    memory := collector.GetUserMemoryUsage(currentUID)
    
    // Memory should be non-negative
    if memory < 0 {
        t.Errorf("GetUserMemoryUsage() returned %d, expected >= 0", memory)
    }
}

func TestGetUserProcessCount(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    currentUID := os.Getuid()
    count := collector.GetUserProcessCount(currentUID)
    
    // Process count should be non-negative
    if count < 0 {
        t.Errorf("GetUserProcessCount() returned %d, expected >= 0", count)
    }
}

func TestCacheFunctions(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    // Test set and get from cache
    collector.setInCache("test_key", "test_value")
    
    val, valid := collector.getFromCache("test_key", 1*time.Second)
    if !valid {
        t.Error("getFromCache() returned invalid for existing key")
    }
    if val != "test_value" {
        t.Errorf("getFromCache() returned %v, expected test_value", val)
    }

    // Test expired cache
    val, valid = collector.getFromCache("test_key", 0*time.Second)
    if valid {
        t.Error("getFromCache() should return invalid for expired key")
    }

    // Test non-existent key
    val, valid = collector.getFromCache("nonexistent_key", 1*time.Second)
    if valid {
        t.Error("getFromCache() should return invalid for non-existent key")
    }
}

func TestClearCache(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    collector.setInCache("key1", "value1")
    collector.setInCache("key2", "value2")
    
    collector.ClearCache()
    
    val, valid := collector.getFromCache("key1", 1*time.Second)
    if valid {
        t.Errorf("ClearCache() did not clear key1: got %v", val)
    }
    
    val, valid = collector.getFromCache("key2", 1*time.Second)
    if valid {
        t.Errorf("ClearCache() did not clear key2: got %v", val)
    }
}

func TestIsValidUserUID(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    tests := []struct {
        uid      int
        expected bool
    }{
        {500, false},   // System user
        {999, false},   // Still system user
        {1000, true},   // Valid user
        {1001, true},   // Valid user
        {-1, false},    // Negative
        {cfg.SystemUIDMax, true},      // Max valid (dynamic)
        {cfg.SystemUIDMax + 1, false}, // Above max (dynamic)
    }

    for _, tt := range tests {
        got := collector.isValidUserUID(tt.uid)
        if got != tt.expected {
            t.Errorf("isValidUserUID(%d): got %v, expected %v (SystemUIDMax=%d)", tt.uid, got, tt.expected, cfg.SystemUIDMax)
        }
    }
}

func TestGetSystemLoad(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    load, err := collector.GetSystemLoad()
    if err != nil {
        t.Errorf("GetSystemLoad() error: %v", err)
    }
    if load < 0 {
        t.Errorf("GetSystemLoad() returned %f, expected >= 0", load)
    }
}

func TestIsSystemUnderLoad(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    // This test depends on actual system state
    underLoad := collector.IsSystemUnderLoad()
    // Just verify it returns without error
    _ = underLoad
}

func TestGetDetailedMetrics(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    metrics := collector.GetDetailedMetrics()
    if metrics == nil {
        t.Error("GetDetailedMetrics() returned nil")
    }

    expectedKeys := []string{
        "total_cores",
        "total_cpu_usage",
        "all_users_cpu_usage",
        "all_users_memory_usage",
		"limited_users_cpu_usage",
		"limited_users_memory_usage",
		"limited_users_count",
		"memory_usage_mb",
        "system_under_load",
        "all_users_count",
        "user_cpu_usage",
        "cache_size",
    }

    for _, key := range expectedKeys {
        if _, exists := metrics[key]; !exists {
            t.Errorf("GetDetailedMetrics() missing key: %s", key)
        }
    }
}

func TestCollectorConcurrency(t *testing.T) {
    cfg := config.DefaultConfig()
    collector, err := NewCollector(cfg)
    if err != nil {
        t.Fatalf("NewCollector() error: %v", err)
    }

    // Test concurrent access to cache
    done := make(chan bool)
    
    for i := 0; i < 10; i++ {
        go func(id int) {
            for j := 0; j < 100; j++ {
                collector.setInCache("key", id)
                collector.getFromCache("key", 1*time.Second)
            }
            done <- true
        }(i)
    }

    for i := 0; i < 10; i++ {
        <-done
    }
}
