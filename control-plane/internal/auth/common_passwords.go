package auth

import "strings"

// commonPasswords is a small, embedded denylist of the passwords that show up
// over and over in public breach corpora (RockYou-style dumps, NCSC/Have I
// Been Pwned "most common" lists) and their obvious padded variants. It is
// deliberately not exhaustive — a few hundred entries catches the overwhelming
// majority of "someone just typed the minimum" attempts without pulling in an
// external dependency or a multi-megabyte wordlist (#513). Matching is exact,
// case-insensitive, against the whole password — not a substring scan — so a
// password that merely contains one of these words (e.g. as part of a longer
// passphrase) is not penalized.
//
// Entries are lowercase; validatePasswordFormat lowercases the candidate
// before lookup. Keep this list append-only and alphabetically loose (grouped
// by family) rather than curated for "the" canonical top-N — the exact
// membership is not contractual.
var commonPasswords = buildCommonPasswordSet([]string{
	// --- classic top-of-every-breach-list passwords ---
	"123456", "123456789", "12345678", "12345", "1234567", "1234567890",
	"qwerty", "qwerty123", "qwertyuiop", "password", "password1", "password!",
	"letmein", "letmein1", "welcome", "welcome1", "monkey", "dragon",
	"football", "baseball", "basketball", "master", "superman", "batman",
	"shadow", "sunshine", "princess", "iloveyou", "trustno1", "abc123",
	"abcd1234", "admin", "administrator", "root", "toor", "guest",
	"login", "changeme", "default", "passw0rd", "p@ssw0rd", "p@ssword",
	"starwars", "whatever", "freedom", "hello", "hunter2", "secret",
	"summer", "winter", "autumn", "spring", "flower", "ninja", "pokemon",
	"cheese", "chocolate", "michael", "jennifer", "jordan", "hunter",
	"tigger", "matthew", "andrew", "joshua", "daniel", "computer",
	"internet", "startup", "access", "biteme", "aaaaaa", "asdfgh",
	"asdf1234", "zxcvbn", "zxcvbnm", "qazwsx", "1q2w3e4r", "1qaz2wsx",
	"qwerty1", "qwerty12", "iloveyou1", "sunshine1", "letmein123",

	// --- keyboard walks / digit runs, short + padded ---
	"1q2w3e4r5t", "1qaz2wsx3edc", "zaq12wsx", "1q2w3e", "q1w2e3r4",
	"0987654321", "1111111111", "0000000000", "9999999999", "121212",
	"112233", "123123", "123321", "654321", "666666", "888888", "777777",
	"555555", "222222", "333333", "444444", "101010", "110110", "112211",

	// --- names / pets / sports padded to typical policy minimums ---
	"password12", "password123", "password1234",
	"welcome123", "welcome1234", "letmein12", "letmein123", "monkey123",
	"dragon123", "football1", "football12", "baseball1", "baseball12",
	"iloveyou12", "iloveyou123", "sunshine12", "sunshine123", "princess1",
	"princess12", "superman1", "superman12", "starwars1", "starwars12",
	"trustno1234", "shadow123", "ninja1234", "pokemon123", "cheese1234",
	"secret123", "secret1234", "hunter2hunt", "flower1234", "summer1234",
	"winter1234", "autumn1234", "spring1234",

	// --- generic "throwaway account" strings and their padded forms ---
	"changeme1", "changeme12", "changeme123", "changeme1234",
	"letmeinnow", "temporary", "temppass", "temppass1", "temppass123",
	"guestguest", "testtest", "testtest1", "testtest123", "test1234",
	"demo1234", "sample123", "newpassword", "newpassword1",
	"mypassword", "mypassword1", "mypassword123", "userpassword",
	"defaultpw", "defaultpassword", "changethis", "changeit",

	// --- admin / operator specific (relevant for founding-admin guard) ---
	"admin123", "admin1234", "admin12345", "administrator1",
	"administrator123", "rootroot", "rootpassword", "root12345",
	"superadmin", "superadmin1", "superuser", "superuser1", "sysadmin",
	"sysadmin1", "operator123", "letmeinadmin", "adminadmin",
	"password4admin",

	// --- "clever" leetspeak / substitution variants ---
	"p4ssword", "p4ssw0rd", "passw0rd1", "passw0rd123", "pa$$word",
	"pa$$w0rd", "l3tm31n", "adm1n", "adm1n123", "w3lc0me", "w3lc0me1",

	// --- qwerty-row and numpad variants padded to 12+ ---
	"qwertyuiop12", "qwertyasdfgh", "asdfghjkl123", "zxcvbnm12345",
	"1234qwerasdf", "qazxswedc123", "1a2b3c4d5e6f", "abcdefghijkl",
	"abcdefg12345",

	// --- long digit-only runs (still exact match, not a range scan) ---
	"12345678901", "123456789012", "1234567890123", "0123456789",
	"00000000000", "11111111111", "12341234", "13131313", "10101010",

	// --- month/season/year themed (common corporate rotation passwords) ---
	"january2024", "january2025", "february2024", "march2024",
	"summer2024", "summer2025", "winter2024", "winter2025",
	"autumn2024", "spring2024", "welcome2024", "welcome2025",
	"password2024", "password2025", "changeme2024", "changeme2025",

	// --- movie / franchise themed ---
	"starwars123", "startrek123", "harrypotter", "harrypotter1",
	"gameofthrones", "gameofthrones1", "jurassicpark", "backtothefuture",

	// --- misc common breach entries ---
	"letitgo", "letitgo123", "iloveu", "iloveu123", "loveyou123",
	"whatever123456", "nothing123", "blahblah", "blahblah123",
	"asdfasdf", "asdfasdf1", "qweqweqwe", "abcabcabc", "xxxxxxxx",
	"password!!", "password12!", "correcthorse", "trubador",
})

// buildCommonPasswordSet lowercases every entry once at init time and returns
// a set for O(1) membership checks — validatePasswordFormat runs on every
// register/change-password call, so this stays off the hot path's allocation
// count beyond the one lookup.
func buildCommonPasswordSet(entries []string) map[string]struct{} {
	set := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		set[strings.ToLower(e)] = struct{}{}
	}
	return set
}
