package usage

import (
	"testing"

	"github.com/aigateway/config"
)

func TestCostEstimation(t *testing.T) {
	tr := NewTracker()
	tr.SetPrices(map[string]config.ModelPrice{
		"gpt-4o": {InputPer1M: 2.5, OutputPer1M: 10},
	})
	tr.RecordUsage("sk-abcdef123456", "openai", "gpt-4o", 1000, 500, false)

	stats := tr.GetStats()
	// 1000 * 2.5/1e6 + 500 * 10/1e6 = 0.0025 + 0.005 = 0.0075
	if stats.EstimatedCostUSD < 0.00749 || stats.EstimatedCostUSD > 0.00751 {
		t.Fatalf("unexpected cost %v", stats.EstimatedCostUSD)
	}
	if stats.CostByProvider["openai"] != stats.EstimatedCostUSD {
		t.Fatalf("cost by provider mismatch: %+v", stats.CostByProvider)
	}
	if stats.MonthlyTokens == nil {
		t.Fatal("monthly tokens must be reported")
	}
}

func TestCostZeroWithoutPrices(t *testing.T) {
	tr := NewTracker()
	tr.RecordUsage("sk-abcdef123456", "openai", "unknown-model", 100, 100, false)
	if got := tr.GetStats().EstimatedCostUSD; got != 0 {
		t.Fatalf("expected zero cost without prices, got %v", got)
	}
}

func TestBudgetEnforcement(t *testing.T) {
	tr := NewTracker()
	tr.SetBudgets([]config.BudgetConfig{{Provider: "mistral", MonthlyTokens: 100}})
	if tr.OverBudget("mistral") {
		t.Fatal("fresh provider must not be over budget")
	}
	if tr.OverBudget("groq") {
		t.Fatal("unconfigured provider must be unlimited")
	}
	tr.RecordUsage("sk-abcdef123456", "mistral", "m", 60, 40, false)
	if !tr.OverBudget("mistral") {
		t.Fatal("100/100 tokens must trip the budget")
	}
	st := tr.GetBudgetStatus()["mistral"]
	if st.Budget != 100 || st.Used != 100 || st.Remaining != 0 || !st.Over {
		t.Fatalf("bad budget status: %+v", st)
	}
	if st.ResetsIn == "" {
		t.Fatal("budget status must include a reset countdown")
	}
}

func TestPersistLoadRoundtripsCostAndMonthly(t *testing.T) {
	tr := NewTracker()
	tr.SetPrices(map[string]config.ModelPrice{"m": {InputPer1M: 1, OutputPer1M: 2}})
	tr.RecordUsage("sk-abcdef123456", "p1", "m", 1000, 1000, false)

	path := t.TempDir() + "/usage.json"
	if err := tr.PersistUsage(path); err != nil {
		t.Fatal(err)
	}
	tr2 := NewTracker()
	if err := tr2.LoadUsage(path); err != nil {
		t.Fatal(err)
	}
	stats := tr2.GetStats()
	if stats.EstimatedCostUSD == 0 {
		t.Fatal("cost must survive persist/load")
	}
	if stats.MonthlyTokens == nil || len(stats.MonthlyTokens) == 0 {
		t.Fatal("monthly buckets must survive persist/load")
	}
	if tr2.MonthlyUsage("p1") != 2000 {
		t.Fatalf("expected 2000 restored tokens, got %d", tr2.MonthlyUsage("p1"))
	}
}
