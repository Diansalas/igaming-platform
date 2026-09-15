package kyc

import (
	"errors"
	"strings"
	"testing"
)

var tinyPNGBytes = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

func TestValidateUpload_ValidPNGAccepted(t *testing.T) {
	contentType, filename, err := ValidateUpload("passport.png", tinyPNGBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contentType != "image/png" {
		t.Errorf("expected image/png, got %q", contentType)
	}
	if filename != "passport.png" {
		t.Errorf("expected filename unchanged, got %q", filename)
	}
}

func TestValidateUpload_RejectsEmptyContent(t *testing.T) {
	_, _, err := ValidateUpload("x.png", nil)
	if !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("expected ErrUploadInvalid for empty content, got %v", err)
	}
}

func TestValidateUpload_RejectsOversizedContent(t *testing.T) {
	oversized := make([]byte, MaxDocumentSizeBytes+1)
	copy(oversized, tinyPNGBytes)
	_, _, err := ValidateUpload("x.png", oversized)
	if !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("expected ErrUploadInvalid for oversized content, got %v", err)
	}
}

func TestValidateUpload_RejectsDisallowedContentType(t *testing.T) {
	_, _, err := ValidateUpload("script.txt", []byte("#!/bin/sh\necho hi\n"))
	if !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("expected ErrUploadInvalid for a disallowed content type, got %v", err)
	}
}

func TestValidateUpload_RejectsExtensionMismatch_ExtensionSpoofing(t *testing.T) {
	// Real PNG bytes, but a .exe extension - never trust the extension,
	// never trust a claimed type, only the sniffed content.
	_, _, err := ValidateUpload("totally-a-photo.exe", tinyPNGBytes)
	if !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("expected ErrUploadInvalid for extension/content mismatch, got %v", err)
	}
}

func TestValidateUpload_SanitizesPathTraversalFilename(t *testing.T) {
	_, filename, err := ValidateUpload("../../etc/passwd.png", tinyPNGBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") {
		t.Errorf("expected path traversal components stripped, got %q", filename)
	}
}

func TestValidateUpload_SanitizesUnsafeCharacters(t *testing.T) {
	_, filename, err := ValidateUpload(`weird<>:"|?*name.png`, tinyPNGBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range filename {
		switch r {
		case '<', '>', ':', '"', '|', '?', '*':
			t.Fatalf("expected unsafe character %q stripped from filename %q", r, filename)
		}
	}
}
