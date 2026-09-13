package web

import "testing"

// safeRedirect decides where a successful login sends the browser, from a
// value the caller supplied in ?next=. Getting it wrong is an open redirect:
// the victim sees a link to the real board, signs in on the real board, and
// is then handed to the attacker's host still believing they are on it.
//
// The interesting cases are the ones that do NOT look like an absolute URL.
// "//evil.com" is the obvious one and was always rejected. "/\evil.com" was
// not: for http and https the URL standard tells browsers to treat "\"
// exactly as "/", so the Location header resolves to the protocol-relative
// "//evil.com". The same is true for the characters browsers STRIP before
// parsing — a tab or newline between the two slashes is removed, and what is
// left is protocol-relative again.
func TestSafeRedirect_RejectsEverythingButASameSitePath(t *testing.T) {
	safe := []struct{ in, want string }{
		{"/", "/"},
		{"/p/BMB", "/p/BMB"},
		{"/p/BMB?done=1#card", "/p/BMB?done=1#card"},
		{"  /p/BMB  ", "/p/BMB"},
		// A backslash deeper in the path is just a path character.
		{"/p/BMB\\x", "/p/BMB\\x"},
	}
	for _, tc := range safe {
		if got := safeRedirect(tc.in); got != tc.want {
			t.Errorf("safeRedirect(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	hostile := []string{
		"",
		"   ",
		"//evil.com",
		"/\\evil.com",  // backslash == slash for special schemes
		"/\\/evil.com", //
		"/\t/evil.com", // tab is stripped, leaving //evil.com
		"/\n/evil.com", // newline is stripped, leaving //evil.com
		"/\r/evil.com", // carriage return, same
		"https://evil.com",
		"http://evil.com",
		"evil.com",
		"javascript:alert(1)",
		"\\\\evil.com",
	}
	for _, in := range hostile {
		if got := safeRedirect(in); got != "/" {
			t.Errorf("safeRedirect(%q) = %q, want %q — this is an open redirect", in, got, "/")
		}
	}
}
