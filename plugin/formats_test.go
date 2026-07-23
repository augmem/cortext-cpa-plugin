package main

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestExtractAndInjectOpenAI(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"Hello Bailey"}]}`)
	msgs := ExtractMessages(fmtOpenAI, body)
	if len(msgs) != 1 || msgs[0].Text != "Hello Bailey" {
		t.Fatalf("msgs = %+v", msgs)
	}
	out, err := InjectMemoryBlock(fmtOpenAI, body, memoryBlock("- likes tennis"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "<cortext_memory>") {
		t.Fatalf("missing memory block: %s", out)
	}
	if gjson.GetBytes(out, "messages.0.role").String() != "system" {
		t.Fatalf("expected prepended system, got %s", out)
	}
	if gjson.GetBytes(out, "messages.1.content").String() != "Hello Bailey" {
		t.Fatalf("user message lost: %s", out)
	}
}

func TestInjectOpenAIMergesExistingSystem(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}]}`)
	out, err := InjectMemoryBlock(fmtOpenAI, body, memoryBlock("- fact"))
	if err != nil {
		t.Fatal(err)
	}
	sys := gjson.GetBytes(out, "messages.0.content").String()
	if !strings.Contains(sys, "You are helpful.") || !strings.Contains(sys, "<cortext_memory>") {
		t.Fatalf("system = %q", sys)
	}
	// second inject should replace, not stack fences
	out2, err := InjectMemoryBlock(fmtOpenAI, out, memoryBlock("- other"))
	if err != nil {
		t.Fatal(err)
	}
	sys2 := gjson.GetBytes(out2, "messages.0.content").String()
	if strings.Count(sys2, "<cortext_memory>") != 1 {
		t.Fatalf("expected one fence, got %q", sys2)
	}
	if strings.Contains(sys2, "- fact") {
		t.Fatalf("old memory not replaced: %q", sys2)
	}
}

func TestExtractClaudeAndInject(t *testing.T) {
	body := []byte(`{"system":"base","messages":[{"role":"user","content":"Who am I?"}]}`)
	msgs := ExtractMessages(fmtClaude, body)
	if LatestUserText(msgs) != "Who am I?" {
		t.Fatalf("latest = %q", LatestUserText(msgs))
	}
	out, err := InjectMemoryBlock(fmtClaude, body, memoryBlock("- Gabe"))
	if err != nil {
		t.Fatal(err)
	}
	sys := gjson.GetBytes(out, "system").String()
	if !strings.Contains(sys, "base") || !strings.Contains(sys, "Gabe") {
		t.Fatalf("system = %q", sys)
	}
}

func TestExtractGemini(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`)
	msgs := ExtractMessages(fmtGemini, body)
	if len(msgs) != 1 || msgs[0].Text != "ping" {
		t.Fatalf("msgs = %+v", msgs)
	}
	out, err := InjectMemoryBlock(fmtGemini, body, memoryBlock("- z"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gjson.GetBytes(out, "systemInstruction.parts.0.text").String(), "cortext_memory") {
		t.Fatalf("out = %s", out)
	}
}

func TestExtractOpenAIResponse(t *testing.T) {
	body := []byte(`{"instructions":"sys","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	msgs := ExtractMessages(fmtOpenAIResponse, body)
	if LatestUserText(msgs) != "hi" {
		t.Fatalf("msgs = %+v", msgs)
	}
	out, err := InjectMemoryBlock(fmtOpenAIResponse, body, memoryBlock("- m"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gjson.GetBytes(out, "instructions").String(), "cortext_memory") {
		t.Fatalf("instructions = %s", out)
	}
}

func TestExtractAssistantOpenAI(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"Hello!"}}]}`)
	if got := ExtractAssistantText(fmtOpenAI, body); got != "Hello!" {
		t.Fatalf("got %q", got)
	}
}

func TestWindowOpenAI(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"1"},
		{"role":"assistant","content":"2"},
		{"role":"user","content":"3"},
		{"role":"assistant","content":"4"}
	]}`)
	out, err := WindowMessages(fmtOpenAI, body, 2)
	if err != nil {
		t.Fatal(err)
	}
	arr := gjson.GetBytes(out, "messages").Array()
	if len(arr) != 3 { // system + last 2
		t.Fatalf("len = %d body=%s", len(arr), out)
	}
	if arr[0].Get("role").String() != "system" {
		t.Fatalf("first = %s", arr[0].Raw)
	}
}

func TestStreamDeltaOpenAI(t *testing.T) {
	chunk := []byte(`data: {"choices":[{"delta":{"content":"Hi"}}]}`)
	text, reasoning := ExtractStreamTextDelta(fmtOpenAI, chunk)
	if text != "Hi" || reasoning {
		t.Fatalf("text=%q reasoning=%v", text, reasoning)
	}
}
