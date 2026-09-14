package service

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

const (
	PhonePlaceholderDomain = "phone.aino.invalid"
)

var (
	// CN mobile number pattern: 1[3-9]X XXXX XXXX
	cnPhonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)
)

// NormalizeCNPhone normalizes a Chinese mainland phone number to E.164 format (+8613900000000)
// Accepts: 13900000000, +8613900000000, 0086-139-0000-0000, +86 139 0000 0000
// Rejects: multiple numbers, invalid formats, non-CN prefixes
func NormalizeCNPhone(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("phone number cannot be empty")
	}

	// Accept only the conventional formatting separators.  In particular, do
	// not use \d or Unicode digit classes here: the canonical identity must be
	// made from ASCII digits and must never silently accept a second number or
	// confusable characters.
	var b strings.Builder
	plusSeen := false
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			_, _ = b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == '(' || r == ')':
			// Formatting only; discard it.
		case r == '+' && b.Len() == 0 && !plusSeen:
			plusSeen = true
			_, _ = b.WriteRune(r)
		default:
			return "", fmt.Errorf("invalid phone number characters")
		}
	}
	cleaned := b.String()
	if cleaned == "" || strings.Count(cleaned, "+") > 1 {
		return "", fmt.Errorf("invalid phone number format")
	}

	// Strip one of the accepted mainland country-code forms.  The bare 86
	// prefix is recognized only when the remainder is a valid mobile number,
	// avoiding the old length-based ambiguity.
	number := cleaned
	switch {
	case strings.HasPrefix(number, "+86"):
		number = number[3:]
	case strings.HasPrefix(number, "0086"):
		number = number[4:]
	case strings.HasPrefix(number, "+"):
		return "", fmt.Errorf("only Chinese mainland (+86) phone numbers are supported")
	case strings.HasPrefix(number, "86") && len(number) == 13 && cnPhonePattern.MatchString(number[2:]):
		number = number[2:]
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
	normalized := strings.ToLower(strings.TrimSpace(email))
	suffix := "@" + PhonePlaceholderDomain
	if !strings.HasSuffix(normalized, suffix) {
		return false
	}
	local := strings.TrimSuffix(normalized, suffix)
	_, err := uuid.Parse(local)
	return err == nil
}

// MaskPhone masks a phone number for display: +8613912345678 -> +86 139****5678
func MaskPhone(e164 string) string {
	normalized, err := NormalizeCNPhone(e164)
	if err != nil || len(normalized) != 14 {
		return "****"
	}

	// +86 139 1234 5678 -> +86 139****5678
	return fmt.Sprintf("+86 %s****%s", normalized[3:6], normalized[10:14])
}
