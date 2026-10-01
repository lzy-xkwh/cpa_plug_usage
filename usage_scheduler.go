package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

var usageScheduler struct {
	mu   sync.Mutex
	stop chan struct{}
	wg   sync.WaitGroup
}

func startUsageScheduler() {
	usageScheduler.mu.Lock()
	if usageScheduler.stop != nil {
		close(usageScheduler.stop)
		usageScheduler.stop = nil
	}
	stop := make(chan struct{})
	usageScheduler.stop = stop
	usageScheduler.wg.Add(1)
	usageScheduler.mu.Unlock()
	go runUsageScheduler(stop)
}

func stopUsageScheduler() {
	usageScheduler.mu.Lock()
	stop := usageScheduler.stop
	if stop != nil {
		close(stop)
		usageScheduler.stop = nil
	}
	usageScheduler.mu.Unlock()
	if stop != nil {
		usageScheduler.wg.Wait()
	}
}

func runUsageScheduler(stop <-chan struct{}) {
	defer usageScheduler.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var lastRun time.Time
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			cfg := currentConfig()
			if !cfg.Enabled {
				continue
			}
			localNow := now.Local()
			start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), cfg.DailyQueryHour, cfg.DailyQueryMinute, 0, 0, localNow.Location())
			if localNow.Before(start) {
				continue
			}
			interval := time.Duration(cfg.DailyQueryIntervalMinutes) * time.Minute
			if interval <= 0 {
				interval = time.Hour
			}
			if !lastRun.IsZero() && localNow.Sub(lastRun) < interval {
				continue
			}
			lastRun = localNow
			if err := runDailyUsageCollection(); err != nil {
				hostLog("error", "api-balance 定时采样失败: "+err.Error())
			}
		}
	}
}

func runDailyUsageCollection() error {
	cfg := currentConfig()
	_, credentials, note := listAllProviders(cfg.ManagementKey)
	if note != "" && len(credentials) == 0 {
		return fmt.Errorf("无法获取账号列表: %s", note)
	}
	selected := map[string]struct{}{}
	for _, key := range cfg.SelectedCredentials {
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			selected[key] = struct{}{}
		}
	}
	if cfg.SelectedCredentials != nil && len(selected) == 0 {
		return nil
	}
	var firstErr error
	for _, credential := range credentials {
		if credential.Disabled {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(credential.ProfileKey))
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(credential.AuthIndex))
		}
		if cfg.SelectedCredentials != nil {
			if _, ok := selected[key]; !ok {
				continue
			}
		}
		result := fetchProviderBalance(credential.Provider, key)
		if !result.OK {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %s", credential.Provider, result.Message)
			}
			continue
		}
		if err := saveProviderSnapshot(credential.Provider, key, result, "scheduled", time.Now()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
