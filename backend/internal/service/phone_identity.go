package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const (
	PhonePlaceholderDomain = "phone.aino.invalid"
)

var (
	// CN mobile number pattern: 1[3-9]X XXXX XXXX
	cnPhonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)
	// Extract digits and optional prefix
	phoneDigitsPattern = regexp.MustCompile(`[\d+]+`)
)

// NormalizeCNPhone normalizes a Chinese mainland phone number to E.164 format (+8613900000000)
// Accepts: 13900000000, +8613900000000, 0086-139-0000-0000, +86 139 0000 0000
// Rejects: multiple numbers, invalid formats, non-CN prefixes
func NormalizeCNPhone(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("phone number cannot be empty")
	}
	
	// Remove whitespace, hyphens, parentheses
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '(' || r == ')' {
			return -1
		}
		return r
	}, raw)
	
	// Check for multiple numbers (comma, semicolon)
	if strings.ContainsAny(cleaned, ",;") {
		return "", fmt.Errorf("multiple phone numbers not supported")
	}
	
	// Extract the number part
	var number string
	if strings.HasPrefix(cleaned, "+86") {
		number = cleaned[3:]
	} else if strings.HasPrefix(cleaned, "0086") {
		number = cleaned[4:]
	} else if strings.HasPrefix(cleaned, "86") && len(cleaned) > 13 {
		// Ambiguous: could be 86XXXXXXXXXXX or just a number starting with 86
		// Only treat as prefix if total length suggests it
		number = cleaned[2:]
	} else if strings.HasPrefix(cleaned, "+") {
		// Non-86 prefix
		return "", fmt.Errorf("only Chinese mainland (+86) phone numbers are supported")
	} else {
		number = cleaned
	}
	
	// Validate CN mobile pattern
	if !cnPhonePattern.MatchString(number) {
		return "", fmt.Errorf("invalid Chinese mainland mobile number format")
	}
	
	return "+86" + number, nil
}

// NewPhonePlaceholderEmail generates a random placeholder email for phone-only accounts
// Format: <uuid>@phone.aino.invalid (RFC 2606 reserved domain)
// Does NOT encode the phone number
func NewPhonePlaceholderEmail() (string, error) {
	id := uuid.New()
	return fmt.Sprintf("%s@%s", id.String(), PhonePlaceholderDomain), nil
}

// IsPhonePlaceholderEmail checks if an email is a phone placeholder
func IsPhonePlaceholderEmail(email string) bool {
	return strings.HasSuffix(email, "@"+PhonePlaceholderDomain)
}

// MaskPhone masks a phone number for display: +8613912345678 -> +86 139****5678
func MaskPhone(e164 string) string {
	if !strings.HasPrefix(e164, "+86") || len(e164) != 14 {
		return "****"
	}
	
	// +86 139 1234 5678 -> +86 139****5678
	return fmt.Sprintf("+86 %s****%s", e164[3:6], e164[10:14])
}
