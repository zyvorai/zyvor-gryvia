package llmgateway

import "testing"

func TestResolveModelSimpleVsLarge(t *testing.T) {
	p := IntentPolicy{SimpleModel: "small", LargeModel: "large", MaxSimpleChars: 10}
	short := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if got := p.ResolveModel("large", short); got != "small" {
		t.Fatalf("short = %q", got)
	}
	long := []byte(`{"messages":[{"role":"user","content":"this is a longer prompt for intent"}]}`)
	if got := p.ResolveModel("large", long); got != "large" {
		t.Fatalf("long = %q", got)
	}
}

func TestAllowBurst(t *testing.T) {
	p := IntentPolicy{BurstURL: "https://api.example.com", BurstModels: []string{"chat"}}
	if !p.AllowBurst("chat") || p.AllowBurst("other") {
		t.Fatalf("burst filter failed")
	}
	p.BurstModels = nil
	if !p.AllowBurst("anything") {
		t.Fatalf("empty list should allow all when URL set")
	}
}
