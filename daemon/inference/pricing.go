package inference

import "strings"

// ModelPricing is one model's $/million-token rate, matching every
// provider's own published billing unit (per-token pricing quoted per
// 1M tokens) so this table's numbers are directly copyable from a
// vendor's pricing page without conversion.
type ModelPricing struct {
	InputPerMillionUSD  float64
	OutputPerMillionUSD float64
}

// modelPricing is a static, manually-maintained snapshot of published
// per-token pricing for the models this codebase's own tests and
// documentation name (daemon/inference's doc comment, inference_test.go).
// It is deliberately NOT fetched live from any provider: no vendor here
// publishes a stable, unauthenticated pricing API, so "static table,
// updated by whoever notices it drifted" is the only real option — the
// same posture this package already takes for OAuth refresh endpoints
// (oauthToken's doc comment: "this package does not hardcode a vendor's
// token endpoint").
//
// Keyed on the exact model string a Request.Model carries (what the
// caller passes straight through to the provider's own API — see
// Request's doc comment). A model not listed here is not an error: see
// CostUSD.
var modelPricing = map[string]ModelPricing{
	// Anthropic direct (https://www.anthropic.com/pricing) and Z.ai's
	// Anthropic-compatible endpoint (same model names, same request
	// shape — see this package's doc comment).
	"claude-opus-5":             {InputPerMillionUSD: 15, OutputPerMillionUSD: 75},
	"claude-sonnet-5":           {InputPerMillionUSD: 3, OutputPerMillionUSD: 15},
	"claude-haiku-4-5-20251001": {InputPerMillionUSD: 1, OutputPerMillionUSD: 5},

	// OpenAI direct (https://openai.com/api/pricing) — openai_compatible
	// kind, base_url https://api.openai.com/v1.
	"gpt-4o":      {InputPerMillionUSD: 2.5, OutputPerMillionUSD: 10},
	"gpt-4o-mini": {InputPerMillionUSD: 0.15, OutputPerMillionUSD: 0.6},

	// Z.ai GLM Coding Plan, openai_compatible kind (see this package's doc
	// comment) — https://docs.z.ai/guides/llm/glm-4.6 pricing page.
	"glm-4.6": {InputPerMillionUSD: 0.6, OutputPerMillionUSD: 2.2},

	// xAI Grok, openai_compatible kind — https://docs.x.ai/docs/models.
	"grok-4": {InputPerMillionUSD: 3, OutputPerMillionUSD: 15},
}

// CostUSD computes usage's real dollar cost from modelPricing, keyed on
// model exactly as given (case-sensitive, no fuzzy/prefix matching — a
// vendor's own model strings are exact identifiers, and guessing at a
// near-match would silently misprice a real bill). known is false, and
// cost is always 0, for a model this table has no entry for — matching
// how Usage itself is already "zero-valued when a provider's response
// carried no usage block, not an error" (see Usage's doc comment): an
// unpriced model is a real, expected gap (this table can never be
// exhaustive over every model any operator might register a provider
// account for), not a bug to panic or error on.
func CostUSD(model string, usage Usage) (cost float64, known bool) {
	pricing, ok := modelPricing[strings.TrimSpace(model)]
	if !ok {
		return 0, false
	}
	cost = float64(usage.InputTokens)/1_000_000*pricing.InputPerMillionUSD +
		float64(usage.OutputTokens)/1_000_000*pricing.OutputPerMillionUSD
	return cost, true
}
