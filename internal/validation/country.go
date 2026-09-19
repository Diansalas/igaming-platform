package validation

// This file implements Stage 4I Phase B's requirement (docs/decisions/
// 0042-human-decision-response.md HDR-J-3b/3c; docs/plans/stage-4i-
// jurisdiction-implementation-plan.md) that a declared/verified residence
// value can never be an arbitrary string - the database CHECK constraint
// (migration 0074) only enforces the SHAPE of a code (two uppercase
// letters), not that it names a real jurisdiction. Without this validator,
// "ZZ" would satisfy the DB constraint and become a permanently-stored
// value nothing else in the platform could recognize.
//
// This is a static reference fact, not a policy: it validates that a
// string IS a real ISO-3166-1 alpha-2 code, and nothing more. It performs
// no country-to-jurisdiction mapping (canonical-model §2.4's collision
// trap - jurisdictions.code is a DIFFERENT code space, e.g. "KM-ANJ" is
// sub-national and "MT"/"CO" only coincidentally match ISO alpha-2 - a
// residence country value must NEVER be compared against, cast to, or
// FK'd to jurisdictions.code).
//
// Deliberately code-only, no country names: this validator only ever
// needs to answer "is this a valid code," never "what is this code
// called," so carrying full names here would be dead weight.

// iso3166Alpha2 is the complete, current set of ISO-3166-1 alpha-2 codes
// (249 entries as of this writing - assigned country codes plus the
// currently-assigned exceptional/user-assigned entries in ordinary use,
// e.g. "GB", "EU" is deliberately excluded as it is not a country code).
var iso3166Alpha2 = map[string]bool{
	"AD": true, "AE": true, "AF": true, "AG": true, "AI": true, "AL": true, "AM": true, "AO": true,
	"AQ": true, "AR": true, "AS": true, "AT": true, "AU": true, "AW": true, "AX": true, "AZ": true,
	"BA": true, "BB": true, "BD": true, "BE": true, "BF": true, "BG": true, "BH": true, "BI": true,
	"BJ": true, "BL": true, "BM": true, "BN": true, "BO": true, "BQ": true, "BR": true, "BS": true,
	"BT": true, "BV": true, "BW": true, "BY": true, "BZ": true,
	"CA": true, "CC": true, "CD": true, "CF": true, "CG": true, "CH": true, "CI": true, "CK": true,
	"CL": true, "CM": true, "CN": true, "CO": true, "CR": true, "CU": true, "CV": true, "CW": true,
	"CX": true, "CY": true, "CZ": true,
	"DE": true, "DJ": true, "DK": true, "DM": true, "DO": true, "DZ": true,
	"EC": true, "EE": true, "EG": true, "EH": true, "ER": true, "ES": true, "ET": true,
	"FI": true, "FJ": true, "FK": true, "FM": true, "FO": true, "FR": true,
	"GA": true, "GB": true, "GD": true, "GE": true, "GF": true, "GG": true, "GH": true, "GI": true,
	"GL": true, "GM": true, "GN": true, "GP": true, "GQ": true, "GR": true, "GS": true, "GT": true,
	"GU": true, "GW": true, "GY": true,
	"HK": true, "HM": true, "HN": true, "HR": true, "HT": true, "HU": true,
	"ID": true, "IE": true, "IL": true, "IM": true, "IN": true, "IO": true, "IQ": true, "IR": true,
	"IS": true, "IT": true,
	"JE": true, "JM": true, "JO": true, "JP": true,
	"KE": true, "KG": true, "KH": true, "KI": true, "KM": true, "KN": true, "KP": true, "KR": true,
	"KW": true, "KY": true, "KZ": true,
	"LA": true, "LB": true, "LC": true, "LI": true, "LK": true, "LR": true, "LS": true, "LT": true,
	"LU": true, "LV": true, "LY": true,
	"MA": true, "MC": true, "MD": true, "ME": true, "MF": true, "MG": true, "MH": true, "MK": true,
	"ML": true, "MM": true, "MN": true, "MO": true, "MP": true, "MQ": true, "MR": true, "MS": true,
	"MT": true, "MU": true, "MV": true, "MW": true, "MX": true, "MY": true, "MZ": true,
	"NA": true, "NC": true, "NE": true, "NF": true, "NG": true, "NI": true, "NL": true, "NO": true,
	"NP": true, "NR": true, "NU": true, "NZ": true,
	"OM": true,
	"PA": true, "PE": true, "PF": true, "PG": true, "PH": true, "PK": true, "PL": true, "PM": true,
	"PN": true, "PR": true, "PS": true, "PT": true, "PW": true, "PY": true,
	"QA": true,
	"RE": true, "RO": true, "RS": true, "RU": true, "RW": true,
	"SA": true, "SB": true, "SC": true, "SD": true, "SE": true, "SG": true, "SH": true, "SI": true,
	"SJ": true, "SK": true, "SL": true, "SM": true, "SN": true, "SO": true, "SR": true, "SS": true,
	"ST": true, "SV": true, "SX": true, "SY": true, "SZ": true,
	"TC": true, "TD": true, "TF": true, "TG": true, "TH": true, "TJ": true, "TK": true, "TL": true,
	"TM": true, "TN": true, "TO": true, "TR": true, "TT": true, "TV": true, "TW": true, "TZ": true,
	"UA": true, "UG": true, "UM": true, "US": true, "UY": true, "UZ": true,
	"VA": true, "VC": true, "VE": true, "VG": true, "VI": true, "VN": true, "VU": true,
	"WF": true, "WS": true,
	"YE": true, "YT": true,
	"ZA": true, "ZM": true, "ZW": true,
}

// IsISO3166Alpha2 reports whether code is a currently-assigned ISO-3166-1
// alpha-2 country code, uppercase, exact match. It does not normalize
// case or trim whitespace - callers validate a value they intend to
// store, not a value they intend to coerce.
func IsISO3166Alpha2(code string) bool {
	return iso3166Alpha2[code]
}

// RequireISO3166Alpha2 adds a field error if value is not a currently
// assigned ISO-3166-1 alpha-2 code, mirroring RequireOneOf's style.
func (e *Errors) RequireISO3166Alpha2(field, value string) {
	if !IsISO3166Alpha2(value) {
		e.Add(field, "must be a valid ISO-3166-1 alpha-2 country code")
	}
}
