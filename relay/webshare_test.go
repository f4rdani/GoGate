package relay

import (
	"testing"
	"time"

	"github.com/aigateway/config"
)

func TestWebshareAccountBandwidthTrackingAndSkip(t *testing.T) {
	pool := NewProxyPool(config.ProxyPoolConfig{
		Enabled: true,
	})

	proxyContent := "31.59.20.176:6754:bpjlxult:eqi2oa0onf9s\n45.38.107.97:6014:bpjlxult:eqi2oa0onf9s"
	accView, err := pool.AddWebshareAccount("Test Akun 1", true, 1000, proxyContent)
	if err != nil {
		t.Fatalf("AddWebshareAccount failed: %v", err)
	}

	if accView.ProxiesCount != 2 {
		t.Fatalf("expected 2 proxies, got %d", accView.ProxiesCount)
	}

	// Verify account exists
	accounts := pool.GetWebshareAccounts()
	if len(accounts) != 1 || accounts[0].Name != "Test Akun 1" {
		t.Fatalf("unexpected accounts: %+v", accounts)
	}
	if accounts[0].Status != "active" {
		t.Fatalf("expected status active, got %s", accounts[0].Status)
	}

	// Get proxy from pool
	p1 := pool.Next()
	if p1 == nil {
		t.Fatalf("expected proxy from pool, got nil")
	}

	// Record 600 bytes
	pool.RecordBandwidth(p1.URL, 600)
	accounts = pool.GetWebshareAccounts()
	if accounts[0].BandwidthUsed != 600 {
		t.Fatalf("expected 600 bytes used, got %d", accounts[0].BandwidthUsed)
	}
	if accounts[0].Status != "active" {
		t.Fatalf("expected status active at 600 bytes, got %s", accounts[0].Status)
	}

	// Record another 500 bytes (total 1100 > 1000 limit)
	pool.RecordBandwidth(p1.URL, 500)
	accounts = pool.GetWebshareAccounts()
	if accounts[0].BandwidthUsed != 1100 {
		t.Fatalf("expected 1100 bytes used, got %d", accounts[0].BandwidthUsed)
	}
	if accounts[0].Status != "exhausted" {
		t.Fatalf("expected status exhausted at 1100 bytes, got %s", accounts[0].Status)
	}

	// Now Next() must skip this exhausted account's proxies!
	// Since there are no other proxies in pool, Next() should return nil
	pExhausted := pool.Next()
	if pExhausted != nil {
		t.Fatalf("expected nil when all proxies are in exhausted accounts, got %v", pExhausted)
	}

	// Reset account
	if !pool.ResetWebshareAccount("Test Akun 1") {
		t.Fatalf("failed to reset account")
	}
	accounts = pool.GetWebshareAccounts()
	if accounts[0].BandwidthUsed != 0 || accounts[0].Status != "active" {
		t.Fatalf("expected 0 bytes and active after reset, got %+v", accounts[0])
	}

	// Next() should now work again
	pRevived := pool.Next()
	if pRevived == nil {
		t.Fatalf("expected proxy after reset, got nil")
	}

	// Delete account
	if !pool.DeleteWebshareAccount("Test Akun 1") {
		t.Fatalf("failed to delete account")
	}
	if len(pool.GetWebshareAccounts()) != 0 {
		t.Fatalf("expected 0 accounts after delete")
	}
}

func TestWebshareMonthlyRollover(t *testing.T) {
	cfg := config.ProxyPoolConfig{
		Enabled: true,
		WebshareAccounts: []config.WebshareAccountConfig{
			{
				Name:           "Old Month Account",
				IsFreeTier:     true,
				BandwidthLimit: 1000,
				BandwidthUsed:  9999,
				CurrentMonth:   "2020-01",
				Status:         "exhausted",
				Proxies:        []string{"31.59.20.176:6754:bpjlxult:eqi2oa0onf9s"},
			},
		},
	}

	pool := NewProxyPool(cfg)
	accounts := pool.GetWebshareAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	// Should auto-reset on initialization because month rolled over
	if accounts[0].CurrentMonth != time.Now().Format("2006-01") {
		t.Fatalf("expected current month, got %s", accounts[0].CurrentMonth)
	}
	if accounts[0].BandwidthUsed != 0 {
		t.Fatalf("expected 0 bytes used after month rollover, got %d", accounts[0].BandwidthUsed)
	}
	if accounts[0].Status != "active" {
		t.Fatalf("expected active status, got %s", accounts[0].Status)
	}
}
