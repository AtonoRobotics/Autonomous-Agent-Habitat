package inference

import "testing"

func TestCostUSD_KnownModel_ComputesRealDollarAmount(t *testing.T) {
	cost, known := CostUSD("claude-sonnet-5", Usage{InputTokens: 2_000_000, OutputTokens: 100_000})
	if !known {
		t.Fatalf("expected claude-sonnet-5 to be a known model")
	}
	// $3/M input * 2M + $15/M output * 0.1M = $6 + $1.5 = $7.5
	if cost != 7.5 {
		t.Fatalf("expected cost 7.5, got %v", cost)
	}
}

func TestCostUSD_UnknownModel_ReturnsZeroAndNotKnown(t *testing.T) {
	cost, known := CostUSD("some-model-nobody-registered-pricing-for", Usage{InputTokens: 1000, OutputTokens: 1000})
	if known {
		t.Fatalf("expected an unlisted model to be reported as not known")
	}
	if cost != 0 {
		t.Fatalf("expected cost 0 for an unknown model, got %v", cost)
	}
}

func TestCostUSD_ZeroUsage_IsZeroCostEvenForAKnownModel(t *testing.T) {
	cost, known := CostUSD("claude-sonnet-5", Usage{})
	if !known {
		t.Fatalf("expected claude-sonnet-5 to be a known model")
	}
	if cost != 0 {
		t.Fatalf("expected cost 0 for zero usage, got %v", cost)
	}
}

func TestCostUSD_TrimsWhitespaceInModelName(t *testing.T) {
	_, known := CostUSD("  claude-sonnet-5  ", Usage{InputTokens: 1})
	if !known {
		t.Fatalf("expected a whitespace-padded known model name to still match")
	}
}
