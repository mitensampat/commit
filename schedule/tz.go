package schedule

import "strings"

// tzByPrefix maps phone country-code prefixes to a representative IANA
// timezone plus a human-readable assumption note. This is deliberately a
// guess-with-disclosure system (hardening req 5): the inferred timezone is
// always stated in the draft so the counterpart or user can correct it, and
// a per-contact override (schedule_tz_overrides) beats this table.
var tzByPrefix = []struct {
	prefix string
	tz     string
	note   string
}{
	// Longer prefixes must come first.
	{"1204", "America/Winnipeg", "assuming Winnipeg from the +1-204 number"},
	{"44", "Europe/London", "assuming UK from the +44 number"},
	{"91", "Asia/Kolkata", "assuming India from the +91 number"},
	{"971", "Asia/Dubai", "assuming UAE from the +971 number"},
	{"65", "Asia/Singapore", "assuming Singapore from the +65 number"},
	{"852", "Asia/Hong_Kong", "assuming Hong Kong from the +852 number"},
	{"81", "Asia/Tokyo", "assuming Japan from the +81 number"},
	{"82", "Asia/Seoul", "assuming Korea from the +82 number"},
	{"86", "Asia/Shanghai", "assuming China from the +86 number"},
	{"61", "Australia/Sydney", "assuming Sydney from the +61 number"},
	{"64", "Pacific/Auckland", "assuming New Zealand from the +64 number"},
	{"49", "Europe/Berlin", "assuming Germany from the +49 number"},
	{"33", "Europe/Paris", "assuming France from the +33 number"},
	{"34", "Europe/Madrid", "assuming Spain from the +34 number"},
	{"39", "Europe/Rome", "assuming Italy from the +39 number"},
	{"31", "Europe/Amsterdam", "assuming Netherlands from the +31 number"},
	{"41", "Europe/Zurich", "assuming Switzerland from the +41 number"},
	{"46", "Europe/Stockholm", "assuming Sweden from the +46 number"},
	{"47", "Europe/Oslo", "assuming Norway from the +47 number"},
	{"45", "Europe/Copenhagen", "assuming Denmark from the +45 number"},
	{"353", "Europe/Dublin", "assuming Ireland from the +353 number"},
	{"351", "Europe/Lisbon", "assuming Portugal from the +351 number"},
	{"972", "Asia/Jerusalem", "assuming Israel from the +972 number"},
	{"966", "Asia/Riyadh", "assuming Saudi Arabia from the +966 number"},
	{"27", "Africa/Johannesburg", "assuming South Africa from the +27 number"},
	{"234", "Africa/Lagos", "assuming Nigeria from the +234 number"},
	{"254", "Africa/Nairobi", "assuming Kenya from the +254 number"},
	{"20", "Africa/Cairo", "assuming Egypt from the +20 number"},
	{"55", "America/Sao_Paulo", "assuming Brazil from the +55 number"},
	{"52", "America/Mexico_City", "assuming Mexico from the +52 number"},
	{"54", "America/Argentina/Buenos_Aires", "assuming Argentina from the +54 number"},
	{"57", "America/Bogota", "assuming Colombia from the +57 number"},
	{"56", "America/Santiago", "assuming Chile from the +56 number"},
	{"92", "Asia/Karachi", "assuming Pakistan from the +92 number"},
	{"880", "Asia/Dhaka", "assuming Bangladesh from the +880 number"},
	{"94", "Asia/Colombo", "assuming Sri Lanka from the +94 number"},
	{"62", "Asia/Jakarta", "assuming Indonesia from the +62 number"},
	{"63", "Asia/Manila", "assuming Philippines from the +63 number"},
	{"66", "Asia/Bangkok", "assuming Thailand from the +66 number"},
	{"84", "Asia/Ho_Chi_Minh", "assuming Vietnam from the +84 number"},
	{"60", "Asia/Kuala_Lumpur", "assuming Malaysia from the +60 number"},
	{"90", "Europe/Istanbul", "assuming Turkey from the +90 number"},
	{"7", "Europe/Moscow", "assuming Russia from the +7 number"},
	// +1 spans six US timezones plus Canada — SF is the stated guess, and the
	// draft says so out loud so a wrong guess gets corrected, not silently
	// booked. Kept last among the "1"-family so 1204 etc. win.
	{"1", "America/Los_Angeles", "that's a +1 number — assuming SF?"},
}

// InferContactTZ guesses the contact's timezone from the phone-number part of
// their JID (e.g. "14155552671@s.whatsapp.net"). Returns ("", "") when the
// JID isn't a phone number (LID contacts) or the prefix is unknown.
func InferContactTZ(contactJID string) (tz, note string) {
	num := contactJID
	if i := strings.Index(num, "@"); i >= 0 {
		if !strings.HasSuffix(num, "@s.whatsapp.net") {
			return "", "" // LID or group — no phone number to read
		}
		num = num[:i]
	}
	num = strings.TrimPrefix(num, "+")
	if num == "" || num[0] < '0' || num[0] > '9' {
		return "", ""
	}
	for _, e := range tzByPrefix {
		if strings.HasPrefix(num, e.prefix) {
			return e.tz, e.note
		}
	}
	return "", ""
}
