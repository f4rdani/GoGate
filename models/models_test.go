package models

import (
	"encoding/json"
	"testing"
)

func TestMessage_ContentString(t *testing.T) {
	// Plain JSON string
	m1 := Message{
		Role:    "user",
		Content: json.RawMessage(`"hello world"`),
	}
	if m1.ContentString() != "hello world" {
		t.Errorf("expected 'hello world', got %s", m1.ContentString())
	}

	// Empty content
	mEmpty := Message{}
	if mEmpty.ContentString() != "" {
		t.Errorf("expected empty string, got %s", mEmpty.ContentString())
	}
}

func TestMessage_HasImage(t *testing.T) {
	// Text only
	mText := Message{
		Role:    "user",
		Content: json.RawMessage(`"just some text"`),
	}
	if mText.HasImage() {
		t.Error("expected HasImage false for plain text")
	}

	// Multi-part with image_url
	mImage := Message{
		Role: "user",
		Content: json.RawMessage(`[
			{"type": "text", "text": "describe this image"},
			{"type": "image_url", "image_url": {"url": "https://example.com/cat.jpg"}}
		]`),
	}
	if !mImage.HasImage() {
		t.Error("expected HasImage true for message with image_url")
	}

	// Single object with type image
	mSingleImage := Message{
		Role: "user",
		Content: json.RawMessage(`{"type": "image", "source": "abc"}`),
	}
	if !mSingleImage.HasImage() {
		t.Error("expected HasImage true for message with single image object")
	}
}
