package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestRequestInjectsMemoryAfterIngest(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.AutoConsolidate = false
	svc := NewService(cfg)

	// Seed via first request
	body1 := []byte(`{"messages":[{"role":"user","content":"Bailey loves tennis balls."}]}`)
	h := http.Header{}
	h.Set("X-Cortext-Session", "sess-1")
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Body:         body1,
		Headers:      h,
	})
	// Second request should recall
	body2 := []byte(`{"messages":[{"role":"user","content":"What does Bailey love?"}]}`)
	resp = svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Body:         body2,
		Headers:      h,
	})
	if len(resp.Body) == 0 {
		t.Fatal("expected rewritten body with memory")
	}
	sys := gjson.GetBytes(resp.Body, "messages.0.content").String()
	if !strings.Contains(sys, "<cortext_memory>") {
		t.Fatalf("expected memory injection, got %s", resp.Body)
	}
	// stub engine keyword recall should surface tennis
	if !strings.Contains(strings.ToLower(sys), "tennis") {
		t.Fatalf("expected recalled tennis fact in %q", sys)
	}
}

func TestScopeIsolation(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	svc := NewService(cfg)

	h1 := http.Header{}
	h1.Set("X-Cortext-Session", "A")
	h2 := http.Header{}
	h2.Set("X-Cortext-Session", "B")

	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h1,
		Body:         []byte(`{"messages":[{"role":"user","content":"Secret for session A only: red-balloon-42"}]}`),
	})

	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h2,
		Body:         []byte(`{"messages":[{"role":"user","content":"What is the secret?"}]}`),
	})
	// Session B should not see A's fact via stub keyword match of "secret" alone maybe —
	// the durable text is only in scope A. Stub recall searches only its own rows.
	if len(resp.Body) > 0 && strings.Contains(string(resp.Body), "red-balloon-42") {
		t.Fatalf("session B leaked session A memory: %s", resp.Body)
	}
}

func TestResponseIngest(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "r1")

	svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		Body:           []byte(`{"choices":[{"message":{"content":"I am the assistant reply about zebras."}}]}`),
	})

	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"zebras"}]}`),
	})
	if len(resp.Body) == 0 || !strings.Contains(strings.ToLower(string(resp.Body)), "zebra") {
		t.Fatalf("expected assistant fact recalled, got %s", resp.Body)
	}
}
