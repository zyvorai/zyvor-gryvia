package llmgateway

import (
	"encoding/json"
	"strings"
)

// IntentPolicy rewrites the request model from message shape and optional burst rules.
// Loaded from a ConfigMap (JSON) or set on the Gateway; empty policy is a no-op.
type IntentPolicy struct {
	// SimpleModel receives short / simple prompts when the requested model is LargeModel (or any).
	SimpleModel string `json:"simpleModel,omitempty"`
	// LargeModel is the default / complex target.
	LargeModel string `json:"largeModel,omitempty"`
	// MaxSimpleChars: prompts with total user text at or below this use SimpleModel (default 256).
	MaxSimpleChars int `json:"maxSimpleChars,omitempty"`
	// BurstURL is an optional OpenAI-compatible base URL used when local GPUs are saturated
	// (requested route is ScaledToZero / Deploying) and BurstModels contains the model.
	BurstURL string `json:"burstURL,omitempty"`
	// BurstModels lists model names (or "*") allowed to burst externally.
	BurstModels []string `json:"burstModels,omitempty"`
}

// ParseIntentPolicy decodes JSON; nil/empty input yields an empty policy.
func ParseIntentPolicy(raw []byte) IntentPolicy {
	var p IntentPolicy
	if len(raw) == 0 {
		return p
	}
	_ = json.Unmarshal(raw, &p)
	return p
}

func (p IntentPolicy) maxSimple() int {
	if p.MaxSimpleChars > 0 {
		return p.MaxSimpleChars
	}
	return 256
}

// ResolveModel picks the served model name for intent routing.
func (p IntentPolicy) ResolveModel(requested string, messagesJSON []byte) string {
	if p.SimpleModel == "" {
		return requested
	}
	if p.LargeModel != "" && requested != p.LargeModel && requested != p.SimpleModel {
		return requested
	}
	n := userTextLen(messagesJSON)
	if n <= p.maxSimple() {
		return p.SimpleModel
	}
	if p.LargeModel != "" {
		return p.LargeModel
	}
	return requested
}

func userTextLen(messagesJSON []byte) int {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(messagesJSON, &body); err != nil {
		return len(messagesJSON)
	}
	if body.Prompt != "" {
		return len(body.Prompt)
	}
	total := 0
	for _, m := range body.Messages {
		if m.Role != "user" && m.Role != "" {
			continue
		}
		switch c := m.Content.(type) {
		case string:
			total += len(c)
		default:
			b, _ := json.Marshal(c)
			total += len(b)
		}
	}
	return total
}

// AllowBurst reports whether the model may use BurstURL.
func (p IntentPolicy) AllowBurst(model string) bool {
	if p.BurstURL == "" {
		return false
	}
	for _, m := range p.BurstModels {
		if m == "*" || strings.EqualFold(m, model) {
			return true
		}
	}
	return len(p.BurstModels) == 0
}
