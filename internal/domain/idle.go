package domain

import "fmt"

// ValidateIdleAfter checks a requested idle threshold in seconds. 0 is the
// explicit "not set" and is always valid: it hands the threshold back to the
// claim TTL. Anything else must lie inside [IdleAfterMin, IdleAfterMax].
//
// It refuses rather than clamps, unlike a lease: a lease is a request the
// server may shorten, but a threshold the caller typed and the server quietly
// moved would make the attention line disagree with what they configured.
func ValidateIdleAfter(seconds int) error {
	if seconds == 0 {
		return nil
	}
	// Compared in seconds, not as a Duration: a huge value multiplied into
	// nanoseconds would overflow and wrap into the valid range.
	lo, hi := int(IdleAfterMin.Seconds()), int(IdleAfterMax.Seconds())
	if seconds < lo || seconds > hi {
		return Invalid("settings.idle_after_seconds",
			fmt.Sprintf("idle_after_seconds must be between %d (%d minutes) and %d (%d days), or 0 to clear it; got %d",
				lo, int(IdleAfterMin.Minutes()), hi, int(IdleAfterMax.Hours()/24), seconds),
			"Send a threshold inside that range, e.g. 172800 for 48h, or 0 to fall back to claim_ttl_seconds.")
	}
	return nil
}
