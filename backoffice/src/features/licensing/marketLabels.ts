// Fixed, UI-owned wording for the market-configuration filing screens (lic31). No sentence states a legal conclusion: a market
// configuration row is a business decision recorded by named staff, never a licence verdict or a jurisdiction authorisation.

export const MARKET_DISCLAIMER =
  'A market configuration change is a decision recorded by named staff. Naming a country here does not say the country is lawful or permitted, and it is not a legal or regulatory authorisation. Which countries may be served is decided by people with that authority, outside this system.'

export const AUTH_REF_NOTE =
  'The authorization reference is supplied by the operator and is not verified by the platform. The platform stores no document, and a reference is not proof of anything. Enter only an opaque id of the decision record, never names, contact details or document content.'

export const COUNTRY_NOTE =
  'Two upper-case letters (ISO 3166-1 alpha-2), typed by you. Nothing is preselected. The server checks that the code is currently assigned.'

export const WIDENING_RULE =
  'A change that WIDENS (it can only increase where the platform serves players) is filed for approval: it needs 2 approvals from 2 different people and nothing changes until then. A change that NARROWS (a stop) is applied at once by one platform administrator. The database classifies the change; the hint below is only a hint.'

export const CEILING_NOTE =
  'A licence country ceiling is the outermost limit for every tenant bound to this licence: a tenant policy cannot enable a country above it. Enabling a country here does not turn it on for any tenant by itself, and a missing row means the country is closed.'

export const POLICY_NOTE =
  'Operating-country policies are layered: the tenant rung opts a country in (a missing tenant row is a terminal block), and the brand and operation rungs can only narrow within it (a missing row there inherits from the rung above). A tenant policy cannot enable a country above the licence ceiling or the tenant\'s approved operating basis; the server refuses it.'

export const CONFIG_NOTE =
  'A jurisdiction configuration is a new immutable version: the version in force is closed and the new content replaces it entirely. A field you leave out is removed, not kept. Choosing a jurisdiction loads the version in force into the form so that nothing is removed by accident.'

export const JURISDICTION_NOTE =
  'A jurisdiction row is a registry record. It is not an approval of the jurisdiction, it grants no market permission and it creates no licence. Unlike the changes above, creating it is NOT four-eyes gated: it is applied at once by the administrator who files it, with a fresh second-factor check and a reason code, and it is written to the audit log. It cannot be edited or deleted afterwards.'

export const CLASS_HINT = {
  widening: 'This looks like a widening change: it needs 2 approvals from 2 different people and takes effect only after the final approval.',
  narrowing: 'This looks like a narrowing change: one platform administrator applies it at once.',
  neutral: 'This looks like it changes nothing; the server refuses a change that changes nothing.',
} as const

export const CLASS_WORDS: Record<string, string> = {
  widening: 'Widening: needs 2 approvals from 2 different people',
  narrowing: 'Narrowing: applied at once by one platform administrator',
}
